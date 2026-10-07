package trader

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"nofx/logger"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bitget API endpoints (V2)
const (
	bitgetDefaultBaseURL   = "https://api.bitget.com"
	bitgetProductType      = "USDT-FUTURES"
	bitgetAccountPath      = "/api/v2/mix/account/accounts"
	bitgetPositionPath     = "/api/v2/mix/position/all-position"
	bitgetOrderPath        = "/api/v2/mix/order/place-order"
	bitgetLeveragePath     = "/api/v2/mix/account/set-leverage"
	bitgetTickerPath       = "/api/v2/mix/market/ticker"
	bitgetContractsPath    = "/api/v2/mix/market/contracts"
	bitgetCancelOrderPath  = "/api/v2/mix/order/cancel-order"
	bitgetPendingPath      = "/api/v2/mix/order/orders-pending"
	bitgetHistoryPath      = "/api/v2/mix/order/orders-history"
	bitgetMarginModePath   = "/api/v2/mix/account/set-margin-mode"
	bitgetPositionModePath = "/api/v2/mix/account/set-position-mode"
	bitgetPlaceTPSLPath    = "/api/v2/mix/order/place-tpsl-order"
	bitgetPlanPendingPath  = "/api/v2/mix/order/orders-plan-pending"
	bitgetCancelPlanPath   = "/api/v2/mix/order/cancel-plan-order"
	bitgetOrderDetailPath  = "/api/v2/mix/order/detail"
	bitgetHistoryPosPath   = "/api/v2/mix/position/history-position"
	bitgetFillHistoryPath  = "/api/v2/mix/order/fill-history"
)

// BitgetTrader Bitget futures trader
type BitgetTrader struct {
	apiKey     string
	secretKey  string
	passphrase string

	// baseURL is the REST endpoint (field so tests can point at httptest).
	baseURL string
	// demo enables Bitget demo trading: every request carries "paptrading: 1".
	demo bool

	// HTTP client
	httpClient *http.Client

	// Balance cache
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// Positions cache
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// Contract info cache (per-symbol fetch timestamp)
	contractsCache      map[string]*bitgetCachedContract
	contractsCacheMutex sync.RWMutex

	// Margin mode per symbol ("crossed" / "isolated"), defaults to crossed
	marginModes     map[string]string
	marginModeMutex sync.RWMutex

	// Order fill polling
	orderPollAttempts int
	orderPollInterval time.Duration

	// Cache duration
	cacheDuration time.Duration
}

type bitgetCachedContract struct {
	contract  *BitgetContract
	fetchedAt time.Time
}

const bitgetContractCacheTTL = 5 * time.Minute

// BitgetContract Bitget contract info
type BitgetContract struct {
	Symbol         string  // Symbol name
	BaseCoin       string  // Base coin
	QuoteCoin      string  // Quote coin
	MinTradeNum    float64 // Minimum trade amount (base units)
	MaxTradeNum    float64 // Maximum trade amount
	SizeMultiplier float64 // Order size step
	PricePlace     int     // Price decimal places
	PriceEndStep   float64 // Price step in units of 10^-PricePlace
	VolumePlace    int     // Volume decimal places
	MinTradeUSDT   float64 // Minimum notional in USDT
	MaxLever       int     // Maximum leverage
	SymbolStatus   string  // normal / maintain / ...
	IsRwa          bool    // Real-world asset (stocks/commodities/fx)
	TakerFeeRate   float64
	MakerFeeRate   float64
	FundInterval   int // funding interval in hours
}

// PriceTick returns the minimum price increment.
func (c *BitgetContract) PriceTick() float64 {
	step := c.PriceEndStep
	if step <= 0 {
		step = 1
	}
	return step * math.Pow(10, -float64(c.PricePlace))
}

// BitgetResponse Bitget API response
type BitgetResponse struct {
	Code        string          `json:"code"`
	Msg         string          `json:"msg"`
	Data        json.RawMessage `json:"data"`
	RequestTime int64           `json:"requestTime"`
}

// BitgetAPIError is returned when Bitget responds with a non-"00000" code.
type BitgetAPIError struct {
	Code string
	Msg  string
}

func (e *BitgetAPIError) Error() string {
	if hint := bitgetErrorHint(e.Code); hint != "" {
		return fmt.Sprintf("Bitget API error: code=%s, msg=%s (%s)", e.Code, e.Msg, hint)
	}
	return fmt.Sprintf("Bitget API error: code=%s, msg=%s", e.Code, e.Msg)
}

// bitgetErrorHint maps common Bitget error codes to clearer explanations.
func bitgetErrorHint(code string) string {
	switch code {
	case "40774":
		return "account position mode does not match the order (this bot uses one-way mode: do not send tradeSide)"
	case "45110":
		return "order amount is below the minimum order size/value"
	case "40762", "43012":
		return "insufficient balance"
	case "40808":
		return "size or price precision is invalid"
	case "40917", "45122":
		return "trigger price is on the wrong side of the current price"
	case "22002":
		return "no position to close"
	}
	return ""
}

// bitgetErrMsgContains reports whether err is a Bitget API error whose raw msg contains any of subs.
func bitgetErrMsgContains(err error, subs ...string) bool {
	var apiErr *BitgetAPIError
	msg := ""
	if errors.As(err, &apiErr) {
		msg = apiErr.Msg
	} else if err != nil {
		msg = err.Error()
	}
	msg = strings.ToLower(msg)
	for _, s := range subs {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// NewBitgetTrader creates a Bitget (live) trader
func NewBitgetTrader(apiKey, secretKey, passphrase string) *BitgetTrader {
	return NewBitgetTraderWithOptions(apiKey, secretKey, passphrase, false)
}

// NewBitgetTraderWithOptions creates a Bitget trader; demo=true enables the demo (paptrading) environment.
func NewBitgetTraderWithOptions(apiKey, secretKey, passphrase string, demo bool) *BitgetTrader {
	trader := newBitgetTraderCore(apiKey, secretKey, passphrase, demo)

	// Set one-way position mode (net mode)
	if err := trader.setPositionMode(); err != nil {
		logger.Infof("⚠️ Failed to set Bitget position mode: %v (ignore if already set)", err)
	}

	if demo {
		logger.Infof("🟢 [Bitget] Trader initialized (DEMO / paptrading)")
	} else {
		logger.Infof("🟢 [Bitget] Trader initialized")
	}

	return trader
}

// newBitgetTraderCore builds the struct without any network call.
func newBitgetTraderCore(apiKey, secretKey, passphrase string, demo bool) *BitgetTrader {
	return &BitgetTrader{
		apiKey:     apiKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		baseURL:    bitgetDefaultBaseURL,
		demo:       demo,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: http.DefaultTransport,
		},
		cacheDuration:     15 * time.Second,
		contractsCache:    make(map[string]*bitgetCachedContract),
		marginModes:       make(map[string]string),
		orderPollAttempts: 3,
		orderPollInterval: 300 * time.Millisecond,
	}
}

// setPositionMode sets one-way position mode
func (t *BitgetTrader) setPositionMode() error {
	body := map[string]interface{}{
		"productType": bitgetProductType,
		"posMode":     "one_way_mode",
	}

	_, err := t.doRequest("POST", bitgetPositionModePath, body)
	if err != nil {
		if bitgetErrMsgContains(err, "same", "already") {
			return nil
		}
		return err
	}

	logger.Infof("  ✓ Bitget account switched to one-way position mode")
	return nil
}

// sign generates Bitget API signature
func (t *BitgetTrader) sign(timestamp, method, requestPath, body string) string {
	// Signature = BASE64(HMAC_SHA256(timestamp + method + requestPath + body, secretKey))
	preHash := timestamp + method + requestPath + body
	h := hmac.New(sha256.New, []byte(t.secretKey))
	h.Write([]byte(preHash))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// doRequest executes HTTP request
func (t *BitgetTrader) doRequest(method, path string, body interface{}) ([]byte, error) {
	var bodyBytes []byte
	var err error

	if body != nil {
		if method == "GET" {
			// For GET requests, body is query parameters (sorted for determinism)
			if params, ok := body.(map[string]interface{}); ok && len(params) > 0 {
				keys := make([]string, 0, len(params))
				for k := range params {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(fmt.Sprintf("%v", params[k])))
				}
				path = path + "?" + strings.Join(parts, "&")
			}
		} else {
			bodyBytes, err = json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize request body: %w", err)
			}
		}
	}

	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())

	// Signature includes body for POST, nothing for GET (query is in path)
	signBody := ""
	if method != "GET" && bodyBytes != nil {
		signBody = string(bodyBytes)
	}
	signature := t.sign(timestamp, method, path, signBody)

	req, err := http.NewRequest(method, t.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("ACCESS-KEY", t.apiKey)
	req.Header.Set("ACCESS-SIGN", signature)
	req.Header.Set("ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("ACCESS-PASSPHRASE", t.passphrase)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "en-US")
	if t.demo {
		req.Header.Set("paptrading", "1")
	}
	// Channel code only for order endpoints
	if strings.Contains(path, "/order/") {
		req.Header.Set("X-CHANNEL-API-CODE", "7fygt")
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var bitgetResp BitgetResponse
	if err := json.Unmarshal(respBody, &bitgetResp); err != nil {
		return nil, fmt.Errorf("failed to parse response (http %d): %w, body: %s", resp.StatusCode, err, string(respBody))
	}

	if bitgetResp.Code != "00000" {
		return nil, &BitgetAPIError{Code: bitgetResp.Code, Msg: bitgetResp.Msg}
	}

	return bitgetResp.Data, nil
}

// convertSymbol converts generic symbol to Bitget format
// e.g., BTCUSDT -> BTCUSDT
func (t *BitgetTrader) convertSymbol(symbol string) string {
	// Bitget uses same format as input, just ensure uppercase
	return strings.ToUpper(symbol)
}

// GetBalance gets account balance
func (t *BitgetTrader) GetBalance() (map[string]interface{}, error) {
	// Check cache
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		t.balanceCacheMutex.RUnlock()
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
	}

	data, err := t.doRequest("GET", bitgetAccountPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get account balance: %w", err)
	}

	var accounts []struct {
		MarginCoin    string `json:"marginCoin"`
		Available     string `json:"available"`     // Available balance
		AccountEquity string `json:"accountEquity"` // Total equity
		UsdtEquity    string `json:"usdtEquity"`    // USDT equity
		UnrealizedPL  string `json:"unrealizedPL"`  // Unrealized P&L
	}

	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("failed to parse balance data: %w, raw: %s", err, string(data))
	}

	var totalEquity, availableBalance, unrealizedPnL float64
	for _, acc := range accounts {
		if acc.MarginCoin == "USDT" {
			totalEquity, _ = strconv.ParseFloat(acc.AccountEquity, 64)
			availableBalance, _ = strconv.ParseFloat(acc.Available, 64)
			unrealizedPnL, _ = strconv.ParseFloat(acc.UnrealizedPL, 64)
			logger.Infof("✓ [Bitget] Balance: equity=%.2f, available=%.2f", totalEquity, availableBalance)
			break
		}
	}

	result := map[string]interface{}{
		"totalWalletBalance":    totalEquity - unrealizedPnL,
		"availableBalance":      availableBalance,
		"totalUnrealizedProfit": unrealizedPnL,
		"total_equity":          totalEquity,
		"totalEquity":           totalEquity,
	}

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// GetPositions gets all positions
func (t *BitgetTrader) GetPositions() ([]map[string]interface{}, error) {
	// Check cache
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		t.positionsCacheMutex.RUnlock()
		return t.cachedPositions, nil
	}
	t.positionsCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
	}

	data, err := t.doRequest("GET", bitgetPositionPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var positions []struct {
		Symbol           string `json:"symbol"`
		HoldSide         string `json:"holdSide"`         // long, short
		OpenPriceAvg     string `json:"openPriceAvg"`     // Average entry price
		MarkPrice        string `json:"markPrice"`        // Mark price
		Total            string `json:"total"`            // Total position size
		Available        string `json:"available"`        // Available to close
		UnrealizedPL     string `json:"unrealizedPL"`     // Unrealized P&L
		Leverage         string `json:"leverage"`         // Leverage
		LiquidationPrice string `json:"liquidationPrice"` // Liquidation price
		MarginSize       string `json:"marginSize"`       // Position margin
		CTime            string `json:"cTime"`            // Create time
		UTime            string `json:"uTime"`            // Update time
	}

	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("failed to parse position data: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		total, _ := strconv.ParseFloat(pos.Total, 64)
		if total == 0 {
			continue
		}

		entryPrice, _ := strconv.ParseFloat(pos.OpenPriceAvg, 64)
		markPrice, _ := strconv.ParseFloat(pos.MarkPrice, 64)
		unrealizedPnL, _ := strconv.ParseFloat(pos.UnrealizedPL, 64)
		leverage, _ := strconv.ParseFloat(pos.Leverage, 64)
		liqPrice, _ := strconv.ParseFloat(pos.LiquidationPrice, 64)
		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)

		// Normalize side
		side := "long"
		if pos.HoldSide == "short" {
			side = "short"
		}

		posMap := map[string]interface{}{
			"symbol":           pos.Symbol,
			"positionAmt":      total,
			"entryPrice":       entryPrice,
			"markPrice":        markPrice,
			"unRealizedProfit": unrealizedPnL,
			"leverage":         leverage,
			"liquidationPrice": liqPrice,
			"side":             side,
			"createdTime":      cTime,
			"updatedTime":      uTime,
		}
		result = append(result, posMap)
	}

	// Update cache
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return result, nil
}

// getContract returns cached contract info (per-symbol TTL), fetching on miss.
func (t *BitgetTrader) getContract(symbol string) (*BitgetContract, error) {
	symbol = t.convertSymbol(symbol)

	t.contractsCacheMutex.RLock()
	if c, ok := t.contractsCache[symbol]; ok && time.Since(c.fetchedAt) < bitgetContractCacheTTL {
		t.contractsCacheMutex.RUnlock()
		return c.contract, nil
	}
	t.contractsCacheMutex.RUnlock()

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"symbol":      symbol,
	}

	data, err := t.doRequest("GET", bitgetContractsPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get contract info for %s: %w", symbol, err)
	}

	var contracts []struct {
		Symbol         string `json:"symbol"`
		BaseCoin       string `json:"baseCoin"`
		QuoteCoin      string `json:"quoteCoin"`
		MinTradeNum    string `json:"minTradeNum"`
		MaxOrderQty    string `json:"maxOrderQty"`
		SizeMultiplier string `json:"sizeMultiplier"`
		PricePlace     string `json:"pricePlace"`
		PriceEndStep   string `json:"priceEndStep"`
		VolumePlace    string `json:"volumePlace"`
		MinTradeUSDT   string `json:"minTradeUSDT"`
		MaxLever       string `json:"maxLever"`
		SymbolStatus   string `json:"symbolStatus"`
		IsRwa          string `json:"isRwa"`
		TakerFeeRate   string `json:"takerFeeRate"`
		MakerFeeRate   string `json:"makerFeeRate"`
		FundInterval   string `json:"fundInterval"`
	}

	if err := json.Unmarshal(data, &contracts); err != nil {
		return nil, fmt.Errorf("failed to parse contract info: %w", err)
	}

	for _, c := range contracts {
		if !strings.EqualFold(c.Symbol, symbol) {
			continue
		}
		contract := &BitgetContract{
			Symbol:       c.Symbol,
			BaseCoin:     c.BaseCoin,
			QuoteCoin:    c.QuoteCoin,
			SymbolStatus: c.SymbolStatus,
			IsRwa:        strings.EqualFold(c.IsRwa, "YES"),
		}
		contract.MinTradeNum, _ = strconv.ParseFloat(c.MinTradeNum, 64)
		contract.MaxTradeNum, _ = strconv.ParseFloat(c.MaxOrderQty, 64)
		contract.SizeMultiplier, _ = strconv.ParseFloat(c.SizeMultiplier, 64)
		contract.PricePlace, _ = strconv.Atoi(c.PricePlace)
		contract.PriceEndStep, _ = strconv.ParseFloat(c.PriceEndStep, 64)
		contract.VolumePlace, _ = strconv.Atoi(c.VolumePlace)
		contract.MinTradeUSDT, _ = strconv.ParseFloat(c.MinTradeUSDT, 64)
		contract.MaxLever, _ = strconv.Atoi(c.MaxLever)
		contract.TakerFeeRate, _ = strconv.ParseFloat(c.TakerFeeRate, 64)
		contract.MakerFeeRate, _ = strconv.ParseFloat(c.MakerFeeRate, 64)
		contract.FundInterval, _ = strconv.Atoi(c.FundInterval)
		if contract.SizeMultiplier <= 0 {
			contract.SizeMultiplier = math.Pow(10, -float64(contract.VolumePlace))
		}

		t.contractsCacheMutex.Lock()
		t.contractsCache[symbol] = &bitgetCachedContract{contract: contract, fetchedAt: time.Now()}
		t.contractsCacheMutex.Unlock()

		return contract, nil
	}

	return nil, fmt.Errorf("contract info not found: %s", symbol)
}

// GetContractInfo returns Bitget contract metadata (precision, min size, max leverage, RWA flag...).
func (t *BitgetTrader) GetContractInfo(symbol string) (*BitgetContract, error) {
	return t.getContract(symbol)
}

// marginModeFor returns the margin mode recorded for symbol (default crossed).
func (t *BitgetTrader) marginModeFor(symbol string) string {
	t.marginModeMutex.RLock()
	defer t.marginModeMutex.RUnlock()
	if m, ok := t.marginModes[symbol]; ok {
		return m
	}
	return "crossed"
}

// SetMarginMode sets margin mode
func (t *BitgetTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	symbol = t.convertSymbol(symbol)

	marginMode := "isolated"
	if isCrossMargin {
		marginMode = "crossed"
	}

	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
		"marginMode":  marginMode,
	}

	_, err := t.doRequest("POST", bitgetMarginModePath, body)
	if err != nil {
		if bitgetErrMsgContains(err, "same", "already") {
			t.setMarginModeLocal(symbol, marginMode)
			return nil
		}
		if bitgetErrMsgContains(err, "position") {
			logger.Infof("  ⚠️ %s has positions, cannot change margin mode (keeping %s)", symbol, t.marginModeFor(symbol))
			return nil
		}
		return err
	}

	t.setMarginModeLocal(symbol, marginMode)
	logger.Infof("  ✓ %s margin mode set to %s", symbol, marginMode)
	return nil
}

func (t *BitgetTrader) setMarginModeLocal(symbol, mode string) {
	t.marginModeMutex.Lock()
	t.marginModes[symbol] = mode
	t.marginModeMutex.Unlock()
}

// SetLeverage sets leverage
func (t *BitgetTrader) SetLeverage(symbol string, leverage int) error {
	symbol = t.convertSymbol(symbol)

	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
		"leverage":    fmt.Sprintf("%d", leverage),
	}

	_, err := t.doRequest("POST", bitgetLeveragePath, body)
	if err != nil {
		if bitgetErrMsgContains(err, "same") {
			return nil
		}
		logger.Infof("  ⚠️ Failed to set %s leverage: %v", symbol, err)
		return fmt.Errorf("failed to set leverage for %s: %w", symbol, err)
	}

	logger.Infof("  ✓ %s leverage set to %dx", symbol, leverage)
	return nil
}

// placeMarketOrder places a one-way-mode market order and confirms the fill via order/detail.
// Open long = buy, open short = sell, close long = sell+reduceOnly, close short = buy+reduceOnly.
// tradeSide is intentionally never sent (one-way mode, would trigger error 40774).
func (t *BitgetTrader) placeMarketOrder(symbol, side string, quantity float64, reduceOnly bool, label string) (map[string]interface{}, error) {
	var qtyStr string
	var err error
	if reduceOnly {
		// Closing must never be blocked by local min-size/notional checks: a residual
		// position below the minimum would otherwise become impossible to close.
		qtyStr, err = t.formatQuantityStep(symbol, quantity)
	} else {
		qtyStr, err = t.FormatQuantity(symbol, quantity)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}

	body := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"marginMode":  t.marginModeFor(symbol),
		"marginCoin":  "USDT",
		"side":        side,
		"orderType":   "market",
		"size":        qtyStr,
		"clientOid":   genBitgetClientOid(),
	}
	if reduceOnly {
		body["reduceOnly"] = "YES"
	}

	logger.Infof("  📊 Bitget %s: symbol=%s, qty=%s", label, symbol, qtyStr)

	data, err := t.doRequest("POST", bitgetOrderPath, body)
	if err != nil {
		return nil, fmt.Errorf("failed to %s: %w", label, err)
	}

	var order struct {
		OrderId   string `json:"orderId"`
		ClientOid string `json:"clientOid"`
	}
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, fmt.Errorf("failed to parse order response: %w", err)
	}

	t.clearCache()

	result := t.confirmOrder(symbol, order.OrderId)
	logger.Infof("✓ Bitget %s submitted: %s orderId=%s status=%v avg=%v", label, symbol, order.OrderId, result["status"], result["avgPrice"])
	return result, nil
}

// confirmOrder polls order/detail (default 3 x 300ms) until the order is filled or canceled.
// If detail is unavailable the order is reported as NEW.
func (t *BitgetTrader) confirmOrder(symbol, orderID string) map[string]interface{} {
	result := map[string]interface{}{
		"orderId":     orderID,
		"symbol":      symbol,
		"status":      "NEW",
		"avgPrice":    0.0,
		"executedQty": 0.0,
	}

	attempts := t.orderPollAttempts
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(t.orderPollInterval)
		}
		st, err := t.GetOrderStatus(symbol, orderID)
		if err != nil {
			logger.Infof("  ⚠️ Bitget order detail query failed (attempt %d/%d): %v", i+1, attempts, err)
			continue
		}
		for _, k := range []string{"status", "avgPrice", "executedQty", "commission"} {
			result[k] = st[k]
		}
		if s, _ := st["status"].(string); s == "FILLED" || s == "CANCELED" {
			break
		}
	}
	return result
}

// OpenLong opens long position
func (t *BitgetTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, fmt.Errorf("aborting open long: %w", err)
	}
	return t.placeMarketOrder(symbol, "buy", quantity, false, "open long")
}

// OpenShort opens short position
func (t *BitgetTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, fmt.Errorf("aborting open short: %w", err)
	}
	return t.placeMarketOrder(symbol, "sell", quantity, false, "open short")
}

// closeQuantity resolves quantity==0 to the current position size.
func (t *BitgetTrader) closeQuantity(symbol, side string, quantity float64) (float64, error) {
	if quantity < 0 {
		quantity = -quantity
	}
	if quantity != 0 {
		return quantity, nil
	}
	positions, err := t.GetPositions()
	if err != nil {
		return 0, err
	}
	for _, pos := range positions {
		if s, _ := pos["symbol"].(string); s == symbol {
			if ps, _ := pos["side"].(string); ps == side {
				if amt, ok := pos["positionAmt"].(float64); ok && amt != 0 {
					return math.Abs(amt), nil
				}
			}
		}
	}
	return 0, fmt.Errorf("%s position not found for %s", side, symbol)
}

// CloseLong closes long position
func (t *BitgetTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)
	qty, err := t.closeQuantity(symbol, "long", quantity)
	if err != nil {
		return nil, err
	}
	return t.placeMarketOrder(symbol, "sell", qty, true, "close long")
}

// CloseShort closes short position
func (t *BitgetTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)
	qty, err := t.closeQuantity(symbol, "short", quantity)
	if err != nil {
		return nil, err
	}
	return t.placeMarketOrder(symbol, "buy", qty, true, "close short")
}

// GetMarketPrice gets market price
func (t *BitgetTrader) GetMarketPrice(symbol string) (float64, error) {
	symbol = t.convertSymbol(symbol)

	params := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
	}

	data, err := t.doRequest("GET", bitgetTickerPath, params)
	if err != nil {
		return 0, fmt.Errorf("failed to get price: %w", err)
	}

	var tickers []struct {
		LastPr string `json:"lastPr"`
	}

	if err := json.Unmarshal(data, &tickers); err != nil {
		return 0, err
	}

	if len(tickers) == 0 {
		return 0, fmt.Errorf("no price data received")
	}

	price, err := strconv.ParseFloat(tickers[0].LastPr, 64)
	if err != nil {
		return 0, err
	}

	return price, nil
}

// bitgetPlanOrder is one entry of orders-plan-pending.entrustedList
type bitgetPlanOrder struct {
	OrderId      string `json:"orderId"`
	PlanType     string `json:"planType"`
	TriggerPrice string `json:"triggerPrice"`
	Side         string `json:"side"`
	PosSide      string `json:"posSide"`
	HoldSide     string `json:"holdSide"`
	Size         string `json:"size"`
	Symbol       string `json:"symbol"`
	CTime        string `json:"cTime"`
}

type bitgetTPSLKind int

const (
	bitgetKindSL bitgetTPSLKind = 1 << iota
	bitgetKindTP
	bitgetKindAll = bitgetKindSL | bitgetKindTP
)

func bitgetPlanKind(planType string) bitgetTPSLKind {
	switch planType {
	case "pos_loss", "loss_plan":
		return bitgetKindSL
	case "pos_profit", "profit_plan":
		return bitgetKindTP
	}
	return 0
}

// bitgetDirection normalizes buy/long -> "long", sell/short -> "short", otherwise "".
func bitgetDirection(s string) string {
	switch strings.ToLower(s) {
	case "buy", "long":
		return "long"
	case "sell", "short":
		return "short"
	}
	return ""
}

// planOrderDirection returns long/short for a plan order, or "" when unknown.
func planOrderDirection(o bitgetPlanOrder) string {
	if d := bitgetDirection(o.HoldSide); d != "" {
		return d
	}
	if d := bitgetDirection(o.PosSide); d != "" {
		return d
	}
	return ""
}

// listPlanOrders queries pending TP/SL (profit_loss) plan orders.
func (t *BitgetTrader) listPlanOrders(symbol string) ([]bitgetPlanOrder, error) {
	params := map[string]interface{}{
		"productType": bitgetProductType,
		"planType":    "profit_loss",
	}
	if symbol != "" {
		params["symbol"] = symbol
	}
	data, err := t.doRequest("GET", bitgetPlanPendingPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to list plan orders: %w", err)
	}
	var resp struct {
		EntrustedList []bitgetPlanOrder `json:"entrustedList"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse plan orders: %w", err)
	}
	return resp.EntrustedList, nil
}

// cancelPlanOrderIDs cancels plan orders by id and reports any failures.
func (t *BitgetTrader) cancelPlanOrderIDs(symbol string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	list := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]string{"orderId": id})
	}
	body := map[string]interface{}{
		"productType": bitgetProductType,
		"marginCoin":  "USDT",
		"symbol":      symbol,
		"planType":    "profit_loss",
		"orderIdList": list,
	}
	data, err := t.doRequest("POST", bitgetCancelPlanPath, body)
	if err != nil {
		return fmt.Errorf("failed to cancel plan orders %v: %w", ids, err)
	}
	var resp struct {
		FailureList []struct {
			OrderId  string `json:"orderId"`
			ErrorMsg string `json:"errorMsg"`
		} `json:"failureList"`
	}
	if err := json.Unmarshal(data, &resp); err == nil && len(resp.FailureList) > 0 {
		var msgs []string
		for _, f := range resp.FailureList {
			msgs = append(msgs, fmt.Sprintf("%s: %s", f.OrderId, f.ErrorMsg))
		}
		return fmt.Errorf("failed to cancel plan orders: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// cancelTPSL cancels pending TP/SL orders of the given kinds. direction ("long"/"short"/"")
// restricts the cancel to one position side when the exchange reports it.
func (t *BitgetTrader) cancelTPSL(symbol string, kinds bitgetTPSLKind, direction string) error {
	symbol = t.convertSymbol(symbol)
	orders, err := t.listPlanOrders(symbol)
	if err != nil {
		return err
	}
	var ids []string
	for _, o := range orders {
		if bitgetPlanKind(o.PlanType)&kinds == 0 {
			continue
		}
		if o.Symbol != "" && !strings.EqualFold(o.Symbol, symbol) {
			continue
		}
		if d := planOrderDirection(o); direction != "" && d != "" && d != direction {
			continue
		}
		ids = append(ids, o.OrderId)
	}
	return t.cancelPlanOrderIDs(symbol, ids)
}

// roundBitgetPrice rounds to pricePlace decimals and to a multiple of priceEndStep.
func roundBitgetPrice(c *BitgetContract, price float64) string {
	tick := c.PriceTick()
	v := math.Round(price/tick) * tick
	return strconv.FormatFloat(v, 'f', c.PricePlace, 64)
}

// placeTPSL places a whole-position stop loss / take profit via place-tpsl-order.
func (t *BitgetTrader) placeTPSL(symbol, positionSide string, price float64, planType, label string) error {
	symbol = t.convertSymbol(symbol)

	holdSide, direction := "buy", "long"
	if strings.ToUpper(positionSide) == "SHORT" {
		holdSide, direction = "sell", "short"
	}

	contract, err := t.getContract(symbol)
	if err != nil {
		return fmt.Errorf("failed to set %s: %w", label, err)
	}
	trigger := roundBitgetPrice(contract, price)

	kind := bitgetKindSL
	if planType == "pos_profit" {
		kind = bitgetKindTP
	}
	// Replace any existing order of the same type on this side
	if err := t.cancelTPSL(symbol, kind, direction); err != nil {
		return fmt.Errorf("failed to clear existing %s before placing new one: %w", label, err)
	}

	body := map[string]interface{}{
		"marginCoin":   "USDT",
		"productType":  bitgetProductType,
		"symbol":       symbol,
		"planType":     planType,
		"triggerPrice": trigger,
		"triggerType":  "mark_price",
		"holdSide":     holdSide,
		"clientOid":    genBitgetClientOid(),
	}

	if _, err := t.doRequest("POST", bitgetPlaceTPSLPath, body); err != nil {
		return fmt.Errorf("failed to set %s: %w", label, err)
	}

	logger.Infof("  ✓ [Bitget] %s set: %s @ %s", label, symbol, trigger)
	return nil
}

// SetStopLoss sets a position stop loss (size is the whole position; quantity is unused)
func (t *BitgetTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return t.placeTPSL(symbol, positionSide, stopPrice, "pos_loss", "stop loss")
}

// SetTakeProfit sets a position take profit (size is the whole position; quantity is unused)
func (t *BitgetTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	return t.placeTPSL(symbol, positionSide, takeProfitPrice, "pos_profit", "take profit")
}

// CancelStopLossOrders cancels stop loss orders
func (t *BitgetTrader) CancelStopLossOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindSL, "")
}

// CancelTakeProfitOrders cancels take profit orders
func (t *BitgetTrader) CancelTakeProfitOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindTP, "")
}

// CancelStopOrders cancels stop loss and take profit orders
func (t *BitgetTrader) CancelStopOrders(symbol string) error {
	return t.cancelTPSL(symbol, bitgetKindAll, "")
}

// CancelAllOrders cancels all pending orders (regular + TP/SL plan orders)
func (t *BitgetTrader) CancelAllOrders(symbol string) error {
	symbol = t.convertSymbol(symbol)
	var errs []error

	data, err := t.doRequest("GET", bitgetPendingPath, map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to list pending orders: %w", err))
	} else {
		var orders struct {
			EntrustedList []struct {
				OrderId string `json:"orderId"`
			} `json:"entrustedList"`
		}
		if err := json.Unmarshal(data, &orders); err != nil {
			errs = append(errs, fmt.Errorf("failed to parse pending orders: %w", err))
		}
		for _, order := range orders.EntrustedList {
			body := map[string]interface{}{
				"symbol":      symbol,
				"productType": bitgetProductType,
				"marginCoin":  "USDT",
				"orderId":     order.OrderId,
			}
			if _, err := t.doRequest("POST", bitgetCancelOrderPath, body); err != nil {
				logger.Infof("  ⚠️ Failed to cancel order %s: %v", order.OrderId, err)
				errs = append(errs, fmt.Errorf("cancel order %s: %w", order.OrderId, err))
			}
		}
	}

	if err := t.cancelTPSL(symbol, bitgetKindAll, ""); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// formatQuantityStep floors quantity to the contract's size step without min-size checks.
func (t *BitgetTrader) formatQuantityStep(symbol string, quantity float64) (string, error) {
	contract, err := t.getContract(t.convertSymbol(symbol))
	if err != nil {
		return "", err
	}
	qty := math.Floor(quantity/contract.SizeMultiplier+1e-9) * contract.SizeMultiplier
	if qty <= 0 {
		return "", fmt.Errorf("quantity %.8f for %s rounds to zero (step %v)", quantity, symbol, contract.SizeMultiplier)
	}
	return strconv.FormatFloat(qty, 'f', contract.VolumePlace, 64), nil
}

// FormatQuantity floors quantity to the contract's size step and formats it with volumePlace.
// It errors when the contract is unknown or the order is below the minimum size / notional.
func (t *BitgetTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	symbol = t.convertSymbol(symbol)
	contract, err := t.getContract(symbol)
	if err != nil {
		return "", err
	}

	mult := contract.SizeMultiplier
	steps := math.Floor(quantity/mult + 1e-9)
	qty := steps * mult
	if qty < contract.MinTradeNum-1e-12 {
		return "", fmt.Errorf("quantity %.8f for %s is below minimum trade size %v (step %v)", quantity, symbol, contract.MinTradeNum, mult)
	}

	if contract.MinTradeUSDT > 0 {
		if price, perr := t.GetMarketPrice(symbol); perr == nil && price > 0 {
			if qty*price < contract.MinTradeUSDT {
				return "", fmt.Errorf("order value %.4f USDT for %s is below minimum %v USDT", qty*price, symbol, contract.MinTradeUSDT)
			}
		} else if perr != nil {
			logger.Infof("  ⚠️ Bitget min-notional check skipped for %s: %v", symbol, perr)
		}
	}

	return strconv.FormatFloat(qty, 'f', contract.VolumePlace, 64), nil
}

// normalizeBitgetStatus maps Bitget order state to the uppercase convention used by other traders.
func normalizeBitgetStatus(state string) string {
	switch strings.ToLower(state) {
	case "filled", "full_fill", "full-fill":
		return "FILLED"
	case "live", "new", "init":
		return "NEW"
	case "partially_filled", "partially-filled", "partial_fill":
		return "PARTIALLY_FILLED"
	case "canceled", "cancelled":
		return "CANCELED"
	}
	return strings.ToUpper(state)
}

// GetOrderStatus gets order status
func (t *BitgetTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	symbol = t.convertSymbol(symbol)

	params := map[string]interface{}{
		"symbol":      symbol,
		"productType": bitgetProductType,
		"orderId":     orderID,
	}

	data, err := t.doRequest("GET", bitgetOrderDetailPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get order status: %w", err)
	}

	var order struct {
		OrderId    string `json:"orderId"`
		State      string `json:"state"`      // live, partially_filled, filled, canceled
		PriceAvg   string `json:"priceAvg"`   // Average fill price
		BaseVolume string `json:"baseVolume"` // Filled quantity
		Fee        string `json:"fee"`        // Fee (negative)
		Side       string `json:"side"`
		OrderType  string `json:"orderType"`
		CTime      string `json:"cTime"`
		UTime      string `json:"uTime"`
	}

	if err := json.Unmarshal(data, &order); err != nil {
		return nil, fmt.Errorf("failed to parse order detail: %w", err)
	}

	avgPrice, _ := strconv.ParseFloat(order.PriceAvg, 64)
	fillQty, _ := strconv.ParseFloat(order.BaseVolume, 64)
	fee, _ := strconv.ParseFloat(order.Fee, 64)
	cTime, _ := strconv.ParseInt(order.CTime, 10, 64)
	uTime, _ := strconv.ParseInt(order.UTime, 10, 64)

	return map[string]interface{}{
		"orderId":     order.OrderId,
		"symbol":      symbol,
		"status":      normalizeBitgetStatus(order.State),
		"avgPrice":    avgPrice,
		"executedQty": fillQty,
		"side":        order.Side,
		"type":        order.OrderType,
		"time":        cTime,
		"updateTime":  uTime,
		"commission":  math.Abs(fee),
	}, nil
}

// inferBitgetCloseType derives the close type from an optional reason field.
func inferBitgetCloseType(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case r == "":
		return "unknown"
	case strings.Contains(r, "liquidat") || strings.Contains(r, "burst"):
		return "liquidation"
	case strings.Contains(r, "profit") || strings.Contains(r, "take"):
		return "take_profit"
	case strings.Contains(r, "loss") || strings.Contains(r, "stop"):
		return "stop_loss"
	}
	return "manual"
}

// GetClosedPnL retrieves closed position PnL records
func (t *BitgetTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	params := map[string]interface{}{
		"productType": bitgetProductType,
		"startTime":   fmt.Sprintf("%d", startTime.UnixMilli()),
		"endTime":     fmt.Sprintf("%d", time.Now().UnixMilli()),
		"limit":       fmt.Sprintf("%d", limit),
	}

	data, err := t.doRequest("GET", bitgetHistoryPosPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get positions history: %w", err)
	}

	var resp struct {
		List []struct {
			PositionId    string `json:"positionId"`
			Symbol        string `json:"symbol"`
			HoldSide      string `json:"holdSide"`
			OpenAvgPrice  string `json:"openAvgPrice"`
			CloseAvgPrice string `json:"closeAvgPrice"`
			OpenTotalPos  string `json:"openTotalPos"`
			CloseTotalPos string `json:"closeTotalPos"`
			Pnl           string `json:"pnl"`
			NetProfit     string `json:"netProfit"`
			TotalFunding  string `json:"totalFunding"`
			OpenFee       string `json:"openFee"`
			CloseFee      string `json:"closeFee"`
			CTime         string `json:"cTime"`
			UTime         string `json:"uTime"`
			CloseType     string `json:"closeType"`
		} `json:"list"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	records := make([]ClosedPnLRecord, 0, len(resp.List))
	for _, pos := range resp.List {
		record := ClosedPnLRecord{
			Symbol:     pos.Symbol,
			Side:       strings.ToLower(pos.HoldSide),
			ExchangeID: pos.PositionId,
		}

		record.EntryPrice, _ = strconv.ParseFloat(pos.OpenAvgPrice, 64)
		record.ExitPrice, _ = strconv.ParseFloat(pos.CloseAvgPrice, 64)
		record.Quantity, _ = strconv.ParseFloat(pos.CloseTotalPos, 64)
		if record.Quantity == 0 {
			record.Quantity, _ = strconv.ParseFloat(pos.OpenTotalPos, 64)
		}
		// pnl is the gross price PnL; fees are reported separately
		record.RealizedPnL, _ = strconv.ParseFloat(pos.Pnl, 64)
		openFee, _ := strconv.ParseFloat(pos.OpenFee, 64)
		closeFee, _ := strconv.ParseFloat(pos.CloseFee, 64)
		record.Fee = math.Abs(openFee) + math.Abs(closeFee)

		cTime, _ := strconv.ParseInt(pos.CTime, 10, 64)
		uTime, _ := strconv.ParseInt(pos.UTime, 10, 64)
		record.EntryTime = time.UnixMilli(cTime).UTC()
		record.ExitTime = time.UnixMilli(uTime).UTC()

		record.CloseType = inferBitgetCloseType(pos.CloseType)
		records = append(records, record)
	}

	return records, nil
}

// clearCache clears all caches
func (t *BitgetTrader) clearCache() {
	t.balanceCacheMutex.Lock()
	t.cachedBalance = nil
	t.balanceCacheMutex.Unlock()

	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheMutex.Unlock()
}

// genBitgetClientOid generates unique client order ID
func genBitgetClientOid() string {
	timestamp := time.Now().UnixNano() % 10000000000000
	rand := time.Now().Nanosecond() % 100000
	return fmt.Sprintf("nofx%d%05d", timestamp, rand)
}

// GetOpenOrders gets pending regular orders and TP/SL plan orders (symbol "" = all symbols)
func (t *BitgetTrader) GetOpenOrders(symbol string) ([]OpenOrder, error) {
	symbol = t.convertSymbol(symbol)
	result := []OpenOrder{}

	params := map[string]interface{}{"productType": bitgetProductType}
	if symbol != "" {
		params["symbol"] = symbol
	}
	data, err := t.doRequest("GET", bitgetPendingPath, params)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending orders: %w", err)
	}
	var pending struct {
		EntrustedList []struct {
			OrderId    string `json:"orderId"`
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			Side       string `json:"side"`
			PosSide    string `json:"posSide"`
			TradeSide  string `json:"tradeSide"`
			OrderType  string `json:"orderType"`
			Price      string `json:"price"`
			Size       string `json:"size"`
			ReduceOnly string `json:"reduceOnly"`
		} `json:"entrustedList"`
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("failed to parse pending orders: %w", err)
	}
	for _, o := range pending.EntrustedList {
		price, _ := strconv.ParseFloat(o.Price, 64)
		size, _ := strconv.ParseFloat(o.Size, 64)
		side := strings.ToLower(o.Side)
		posSide := strings.ToUpper(bitgetDirection(o.PosSide))
		if posSide == "" {
			// one-way: reduceOnly sell closes a long, reduceOnly buy closes a short
			reduce := strings.EqualFold(o.ReduceOnly, "YES") || strings.EqualFold(o.TradeSide, "close")
			switch {
			case side == "buy" && !reduce, side == "sell" && reduce:
				posSide = "LONG"
			default:
				posSide = "SHORT"
			}
		}
		status := normalizeBitgetStatus(o.Status)
		result = append(result, OpenOrder{
			OrderID:      o.OrderId,
			Symbol:       o.Symbol,
			Side:         strings.ToUpper(side),
			PositionSide: posSide,
			Type:         strings.ToUpper(o.OrderType),
			Price:        price,
			Quantity:     size,
			Status:       status,
		})
	}

	plans, err := t.listPlanOrders(symbol)
	if err != nil {
		return nil, err
	}
	for _, o := range plans {
		kind := bitgetPlanKind(o.PlanType)
		if kind == 0 {
			continue
		}
		trigger, _ := strconv.ParseFloat(o.TriggerPrice, 64)
		size, _ := strconv.ParseFloat(o.Size, 64)
		typ := "STOP_MARKET"
		if kind == bitgetKindTP {
			typ = "TAKE_PROFIT_MARKET"
		}
		dir := planOrderDirection(o)
		if dir == "" {
			// Fallback: treat plan side like holdSide (buy = long position)
			dir = bitgetDirection(o.Side)
		}
		if dir == "" {
			dir = "long"
		}
		posSide := strings.ToUpper(dir)
		// Closing side is opposite of the position direction
		closeSide := "SELL"
		if dir == "short" {
			closeSide = "BUY"
		}
		result = append(result, OpenOrder{
			OrderID:      o.OrderId,
			Symbol:       o.Symbol,
			Side:         closeSide,
			PositionSide: posSide,
			Type:         typ,
			StopPrice:    trigger,
			Quantity:     size,
			Status:       "NEW",
		})
	}

	return result, nil
}
