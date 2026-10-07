package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/logger"
	"nofx/store"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BitgetTrade represents a trade record from Bitget fill history
type BitgetTrade struct {
	Symbol      string
	TradeID     string
	OrderID     string
	Side        string // buy or sell
	FillPrice   float64
	FillQty     float64
	Fee         float64
	FeeAsset    string
	ExecTime    time.Time
	ProfitLoss  float64
	OrderType   string
	OrderAction string // open_long, open_short, close_long, close_short
}

// bitgetFillMaxPages bounds fill-history pagination per sync run
const bitgetFillMaxPages = 10

// classifyBitgetFill determines the order action (open_long/open_short/close_long/close_short)
// from a fill's side, tradeSide and realized profit.
//
// Hedge mode reports tradeSide=open|close (and reduce_close_long, burst_close_short, ...);
// one-way mode reports buy_single|sell_single, where a non-zero realized profit means the
// fill reduced a position. Prefixes reduce_/burst_/offset_/delivery_/dte_ and "close" are closes.
func classifyBitgetFill(side, tradeSide string, profit float64) string {
	side = strings.ToLower(side)
	ts := strings.ToLower(tradeSide)

	openBySide := func() string {
		if side == "buy" {
			return "open_long"
		}
		return "open_short"
	}
	// A closing sell reduces a long; a closing buy reduces a short.
	closeBySide := func() string {
		if side == "sell" {
			return "close_long"
		}
		return "close_short"
	}

	switch {
	case ts == "open":
		return openBySide()
	case ts == "close":
		return closeBySide()
	case strings.HasSuffix(ts, "_long") && isBitgetCloseTradeSide(ts):
		return "close_long"
	case strings.HasSuffix(ts, "_short") && isBitgetCloseTradeSide(ts):
		return "close_short"
	case isBitgetCloseTradeSide(ts):
		return closeBySide()
	case ts == "buy_single" || ts == "sell_single":
		if profit != 0 {
			return closeBySide()
		}
		return openBySide()
	}
	// Unknown tradeSide: fall back to realized profit
	if profit != 0 {
		return closeBySide()
	}
	return openBySide()
}

func isBitgetCloseTradeSide(ts string) bool {
	if strings.Contains(ts, "close") {
		return true
	}
	for _, p := range []string{"reduce_", "burst_", "offset_", "delivery_", "dte_"} {
		if strings.HasPrefix(ts, p) {
			return true
		}
	}
	return false
}

// GetTrades retrieves trade/fill records from Bitget (fill-history, paginated with idLessThan,
// up to bitgetFillMaxPages pages of `limit` records each)
func (t *BitgetTrader) GetTrades(startTime time.Time, limit int) ([]BitgetTrade, error) {
	if limit <= 0 || limit > 100 {
		limit = 100 // Bitget max limit is 100
	}

	endTime := time.Now().UnixMilli()
	idLessThan := ""
	trades := make([]BitgetTrade, 0, limit)

	for page := 0; page < bitgetFillMaxPages; page++ {
		params := map[string]interface{}{
			"productType": bitgetProductType,
			"startTime":   fmt.Sprintf("%d", startTime.UnixMilli()),
			"endTime":     fmt.Sprintf("%d", endTime),
			"limit":       fmt.Sprintf("%d", limit),
		}
		if idLessThan != "" {
			params["idLessThan"] = idLessThan
		}

		data, err := t.doRequest("GET", bitgetFillHistoryPath, params)
		if err != nil {
			return nil, fmt.Errorf("failed to get fill history: %w", err)
		}

		var resp struct {
			FillList []struct {
				TradeID    string `json:"tradeId"`
				Symbol     string `json:"symbol"`
				OrderID    string `json:"orderId"`
				Side       string `json:"side"`
				Price      string `json:"price"`
				BaseVolume string `json:"baseVolume"`
				Fee        string `json:"fee"` // legacy field, feeDetail preferred
				Profit     string `json:"profit"`
				CTime      string `json:"cTime"`
				TradeSide  string `json:"tradeSide"`
				FeeDetail  []struct {
					FeeCoin  string `json:"feeCoin"`
					TotalFee string `json:"totalFee"`
				} `json:"feeDetail"`
			} `json:"fillList"`
			EndID string `json:"endId"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse fills: %w", err)
		}

		for _, fill := range resp.FillList {
			fillPrice, _ := strconv.ParseFloat(fill.Price, 64)
			fillQty, _ := strconv.ParseFloat(fill.BaseVolume, 64)
			profit, _ := strconv.ParseFloat(fill.Profit, 64)
			cTime, _ := strconv.ParseInt(fill.CTime, 10, 64)

			// Fees are negative costs in feeDetail[].totalFee; sum absolute values
			fee := 0.0
			feeAsset := ""
			for _, fd := range fill.FeeDetail {
				v, _ := strconv.ParseFloat(fd.TotalFee, 64)
				fee += math.Abs(v)
				if feeAsset == "" {
					feeAsset = fd.FeeCoin
				}
			}
			if len(fill.FeeDetail) == 0 {
				v, _ := strconv.ParseFloat(fill.Fee, 64)
				fee = math.Abs(v)
			}
			if feeAsset == "" {
				feeAsset = "USDT"
			}

			trades = append(trades, BitgetTrade{
				Symbol:      strings.ToUpper(fill.Symbol),
				TradeID:     fill.TradeID,
				OrderID:     fill.OrderID,
				Side:        fill.Side,
				FillPrice:   fillPrice,
				FillQty:     fillQty,
				Fee:         fee,
				FeeAsset:    feeAsset,
				ExecTime:    time.UnixMilli(cTime).UTC(),
				ProfitLoss:  profit,
				OrderType:   "MARKET",
				OrderAction: classifyBitgetFill(fill.Side, fill.TradeSide, profit),
			})
		}

		// Stop when there is no cursor, no data, or the cursor did not advance
		if resp.EndID == "" || len(resp.FillList) == 0 || resp.EndID == idLessThan {
			break
		}
		idLessThan = resp.EndID
	}

	return trades, nil
}

// SyncOrdersFromBitget syncs Bitget exchange order history to local database
// Also creates/updates position records to ensure orders/fills/positions data consistency
// exchangeID: Exchange account UUID (from exchanges.id)
// exchangeType: Exchange type ("bitget")
func (t *BitgetTrader) SyncOrdersFromBitget(traderID string, exchangeID string, exchangeType string, st *store.Store) error {
	if st == nil {
		return fmt.Errorf("store is nil")
	}

	// Get recent trades (last 24 hours)
	startTime := time.Now().Add(-24 * time.Hour)

	logger.Infof("🔄 Syncing Bitget trades from: %s", startTime.Format(time.RFC3339))

	// Use GetTrades method to fetch trade records
	trades, err := t.GetTrades(startTime, 100)
	if err != nil {
		return fmt.Errorf("failed to get trades: %w", err)
	}

	logger.Infof("📥 Received %d trades from Bitget", len(trades))

	// Sort trades by time ASC (oldest first) for proper position building
	sort.Slice(trades, func(i, j int) bool {
		return trades[i].ExecTime.UnixMilli() < trades[j].ExecTime.UnixMilli()
	})

	// Process trades one by one (no transaction to avoid deadlock)
	orderStore := st.Order()
	positionStore := st.Position()
	posBuilder := store.NewPositionBuilder(positionStore)
	syncedCount := 0

	for _, trade := range trades {
		// Check if trade already exists (use exchangeID which is UUID, not exchange type)
		existing, err := orderStore.GetOrderByExchangeID(exchangeID, trade.TradeID)
		if err == nil && existing != nil {
			continue // Order already exists, skip
		}

		// Keep the native Bitget symbol (market.Normalize would map e.g. NVDAUSDT to xyz:NVDA)
		symbol := strings.ToUpper(trade.Symbol)

		// Determine position side from order action
		positionSide := "LONG"
		if strings.Contains(trade.OrderAction, "short") {
			positionSide = "SHORT"
		}

		// Normalize side for storage
		side := strings.ToUpper(trade.Side)

		// Create order record - use UTC time in milliseconds to avoid timezone issues
		execTimeMs := trade.ExecTime.UTC().UnixMilli()
		orderRecord := &store.TraderOrder{
			TraderID:        traderID,
			ExchangeID:      exchangeID,   // UUID
			ExchangeType:    exchangeType, // Exchange type
			ExchangeOrderID: trade.TradeID,
			Symbol:          symbol,
			Side:            side,
			PositionSide:    "BOTH", // Bitget uses one-way position mode
			Type:            trade.OrderType,
			OrderAction:     trade.OrderAction,
			Quantity:        trade.FillQty,
			Price:           trade.FillPrice,
			Status:          "FILLED",
			FilledQuantity:  trade.FillQty,
			AvgFillPrice:    trade.FillPrice,
			Commission:      trade.Fee,
			FilledAt:        execTimeMs,
			CreatedAt:       execTimeMs,
			UpdatedAt:       execTimeMs,
		}

		// Insert order record
		if err := orderStore.CreateOrder(orderRecord); err != nil {
			logger.Infof("  ⚠️ Failed to sync trade %s: %v", trade.TradeID, err)
			continue
		}

		// Create fill record - use UTC time in milliseconds
		fillRecord := &store.TraderFill{
			TraderID:        traderID,
			ExchangeID:      exchangeID,   // UUID
			ExchangeType:    exchangeType, // Exchange type
			OrderID:         orderRecord.ID,
			ExchangeOrderID: trade.OrderID,
			ExchangeTradeID: trade.TradeID,
			Symbol:          symbol,
			Side:            side,
			Price:           trade.FillPrice,
			Quantity:        trade.FillQty,
			QuoteQuantity:   trade.FillPrice * trade.FillQty,
			Commission:      trade.Fee,
			CommissionAsset: trade.FeeAsset,
			RealizedPnL:     trade.ProfitLoss,
			IsMaker:         false,
			CreatedAt:       execTimeMs,
		}

		if err := orderStore.CreateFill(fillRecord); err != nil {
			logger.Infof("  ⚠️ Failed to sync fill for trade %s: %v", trade.TradeID, err)
		}

		// Create/update position record using PositionBuilder
		if err := posBuilder.ProcessTrade(
			traderID, exchangeID, exchangeType,
			symbol, positionSide, trade.OrderAction,
			trade.FillQty, trade.FillPrice, trade.Fee, trade.ProfitLoss,
			execTimeMs, trade.TradeID,
		); err != nil {
			logger.Infof("  ⚠️ Failed to sync position for trade %s: %v", trade.TradeID, err)
		} else {
			logger.Infof("  📍 Position updated for trade: %s (action: %s, qty: %.6f)", trade.TradeID, trade.OrderAction, trade.FillQty)
		}

		syncedCount++
		logger.Infof("  ✅ Synced trade: %s %s %s qty=%.6f price=%.6f pnl=%.2f fee=%.6f action=%s",
			trade.TradeID, symbol, side, trade.FillQty, trade.FillPrice, trade.ProfitLoss, trade.Fee, trade.OrderAction)
	}

	logger.Infof("✅ Bitget order sync completed: %d new trades synced", syncedCount)
	return nil
}

// StartOrderSync starts background order sync task for Bitget
func (t *BitgetTrader) StartOrderSync(traderID string, exchangeID string, exchangeType string, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			if err := t.SyncOrdersFromBitget(traderID, exchangeID, exchangeType, st); err != nil {
				logger.Infof("⚠️  Bitget order sync failed: %v", err)
			}
		}
	}()
	logger.Infof("🔄 Bitget order sync started (interval: %v)", interval)
}
