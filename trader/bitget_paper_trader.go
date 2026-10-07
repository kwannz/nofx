package trader

import (
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	bitgetapi "nofx/provider/bitget"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BitgetPaperTrader ("bitget_paper") is a local paper-trading exchange. Orders are
// simulated in memory against LIVE Bitget USDT-futures public market data, so it
// needs no API keys and never touches a real or demo account.
//
// Simulation model (see docs/bitget-paper-trading.md):
//
//   - Market orders only. Fills happen at the current mark price moved 2 bps (default)
//     against the trader: buys fill higher, sells fill lower.
//   - Taker fee = the contract's takerFeeRate (fallback 0.06%) on every fill.
//   - Position mode (bitget_position_mode of the exchange setting, default hedge, like the
//     live Bitget trader): in hedge mode a symbol can hold a LONG and a SHORT position at the
//     same time, each with its own entry, margin, SL/TP, liquidation and funding; in one-way
//     mode there is one position per symbol and opening the opposite side while a position
//     exists is rejected (the auto trader closes first). Adding to the same side averages the
//     entry price. The mode can only be switched while the account is flat.
//   - Margin is isolated-style: every position locks notional/leverage of the free
//     balance. SetMarginMode is recorded but both modes behave identically (the liquidation
//     check only looks at the position's own margin; free balance never backs a position).
//   - Liquidation approximation: a position is liquidated when
//     margin + unrealizedPnL <= 0.5% of the position's notional at mark. The remaining
//     equity is forfeited as a liquidation fee (the loss never exceeds the position
//     margin). Bitget's tiered maintenance margin and insurance fund are not modelled.
//   - Position-level SL/TP (one each per symbol) trigger on the mark price and close the
//     whole position with a market fill (with slippage and fee).
//   - Funding settles at UTC boundaries aligned to the contract's fundInterval (8h ->
//     00:00/08:00/16:00) using the CURRENT funding rate: longs pay when the rate is
//     positive. The amount is added to / removed from the position margin. Progress is
//     tracked per position as the unix time funding has been settled through (the open time
//     at first, the last settled boundary afterwards) and the due boundaries are counted
//     with the interval of the moment, so a fundInterval change on the exchange only moves
//     the grid instead of corrupting a stored slot index.
//   - State is kept in memory and snapshotted to disk (JSON, atomic replace) after every
//     mutation, keyed by trader ID (see bitget_paper_state.go). A restart restores balance,
//     positions, SL/TP and history; AutoTrader.Run then reconciles the database positions.
//
// A background matcher goroutine (interval 5s) evaluates SL/TP, liquidation and funding.
// It is started lazily when a position is opened (or explicitly via Start) and stopped by
// Stop/Close; it never runs for a trader that has not traded.
type BitgetPaperTrader struct {
	source paperPriceSource
	now    func() time.Time

	slippage      float64 // fraction, e.g. 0.0002 for 2 bps
	matchInterval time.Duration
	autoMatch     bool          // false in unit tests: they drive Tick directly
	priceTTL      time.Duration // mark price cache used by the read-only calls

	tickMu sync.Mutex // serialises Tick

	mu        sync.Mutex // guards everything below
	initial   float64
	cash      float64                   // free (unlocked) balance
	positions map[string]*paperPosition // keyed by paperPosKey(symbol, side)
	posMode   string                    // BitgetPositionModeHedge (default) / BitgetPositionModeOneWay
	leverage  map[string]int            // per-symbol leverage setting
	crossMode map[string]bool           // per-symbol margin mode flag (informational)
	fills     []*PaperFill
	fillIndex map[string]*PaperFill
	closed    []ClosedPnLRecord
	seq       uint64
	realized  float64 // cumulative gross price PnL of closing fills
	// lastFundingAt is the most recent funding boundary (unix seconds) settled for any
	// position (bookkeeping only: the per-position fundedThrough decides what is still due).
	lastFundingAt int64
	dirty         bool // matcher changed state (funding) that still has to be persisted
	lastMark      map[string]float64
	markCache     map[string]paperMarkEntry
	onSysFill     func(PaperFill)
	stopCh        chan struct{}
	matcherEnd    chan struct{}

	// persistence (bitget_paper_state.go); statePath == "" means in-memory only
	traderID      string
	statePath     string
	persistFailed bool
}

// paperPriceSource supplies live market data. Tests inject a fake.
type paperPriceSource interface {
	// MarkPrice returns the current mark price (falling back to the last price).
	MarkPrice(symbol string) (float64, error)
	// Contract returns the contract metadata (precision, limits, fees, status).
	Contract(symbol string) (*bitgetapi.Contract, error)
	// FundingRate returns the current funding rate (fraction per funding interval).
	FundingRate(symbol string) (float64, error)
}

// bitgetLiveSource is the production price source backed by provider/bitget (public API).
type bitgetLiveSource struct{}

func (bitgetLiveSource) MarkPrice(symbol string) (float64, error) {
	tk, err := bitgetapi.GetTicker(symbol)
	if err != nil {
		return 0, err
	}
	if tk.MarkPrice > 0 {
		return tk.MarkPrice, nil
	}
	if tk.LastPrice > 0 {
		return tk.LastPrice, nil
	}
	return 0, fmt.Errorf("bitget ticker for %s has no price", symbol)
}

func (bitgetLiveSource) Contract(symbol string) (*bitgetapi.Contract, error) {
	return bitgetapi.GetContract(symbol)
}

func (bitgetLiveSource) FundingRate(symbol string) (float64, error) {
	return bitgetapi.GetFundingRate(symbol)
}

const (
	paperDefaultBalance        = 10000.0
	paperDefaultTakerFee       = 0.0006
	paperDefaultSlippageBps    = 2.0
	paperMaintenanceMarginRate = 0.005
	paperDefaultMatchInterval  = 5 * time.Second
	paperDefaultFundHours      = 8
	paperMarkCacheTTL          = 2 * time.Second
	paperMaxHistory            = 5000
	paperEps                   = 1e-9
	paperMaxFundingCatchUp     = 100

	paperReasonManual      = "manual"
	paperReasonStopLoss    = "stop_loss"
	paperReasonTakeProfit  = "take_profit"
	paperReasonLiquidation = "liquidation"
)

type paperMarkEntry struct {
	price float64
	at    time.Time
}

type paperTrigger struct {
	id    string
	price float64
}

type paperPosition struct {
	id       string
	symbol   string
	side     string // "long" / "short"
	qty      float64
	entry    float64
	leverage int
	margin   float64 // locked margin incl. funding adjustments
	openFee  float64 // opening fees still attributed to the open quantity
	funding  float64 // cumulative funding received (+) / paid (-)
	openedAt time.Time
	// fundedThrough is the unix time (seconds) funding has been settled through: the open time
	// for a new position, the last settled funding boundary afterwards. It is a timestamp, not
	// a slot index, so it stays meaningful when the contract's fundInterval changes.
	fundedThrough int64
	sl, tp        *paperTrigger
}

func (p *paperPosition) dir() float64 {
	if p.side == "short" {
		return -1
	}
	return 1
}

func (p *paperPosition) unrealized(mark float64) float64 {
	return p.dir() * (mark - p.entry) * p.qty
}

// liquidationPrice is the mark price at which margin + uPnL equals the maintenance margin.
func (p *paperPosition) liquidationPrice() float64 {
	if p.qty <= 0 {
		return 0
	}
	var px float64
	if p.side == "short" {
		px = (p.entry*p.qty + p.margin) / (p.qty * (1 + paperMaintenanceMarginRate))
	} else {
		px = (p.entry*p.qty - p.margin) / (p.qty * (1 - paperMaintenanceMarginRate))
	}
	if px < 0 || math.IsNaN(px) || math.IsInf(px, 0) {
		return 0
	}
	return px
}

// PaperFill describes one simulated fill. Fills caused by the matcher (stop loss, take
// profit, liquidation) are also pushed to the handler set with SetSystemFillHandler so the
// AutoTrader can persist them like exchange-reported trades.
type PaperFill struct {
	OrderID      string
	Symbol       string
	Action       string // open_long, open_short, close_long, close_short
	PositionSide string // LONG / SHORT
	Side         string // BUY / SELL
	Price        float64
	Quantity     float64
	Fee          float64
	RealizedPnL  float64 // gross price PnL for closing fills (0 for opens)
	EntryPrice   float64 // position entry price after the fill
	Leverage     int
	Reason       string // manual / stop_loss / take_profit / liquidation
	Time         time.Time
}

// NewBitgetPaperTrader creates a paper trader on live Bitget public data.
// initialBalance <= 0 falls back to 10000 USDT.
func NewBitgetPaperTrader(initialBalance float64) *BitgetPaperTrader {
	t := newBitgetPaperTrader(initialBalance, bitgetLiveSource{}, time.Now)
	t.autoMatch = true
	t.priceTTL = paperMarkCacheTTL
	return t
}

// newBitgetPaperTrader is the injectable constructor (fake price source and clock). The
// background matcher and the mark price cache are disabled; tests call Tick directly.
func newBitgetPaperTrader(initialBalance float64, src paperPriceSource, now func() time.Time) *BitgetPaperTrader {
	if initialBalance <= 0 {
		initialBalance = paperDefaultBalance
	}
	if now == nil {
		now = time.Now
	}
	return &BitgetPaperTrader{
		source:        src,
		now:           now,
		slippage:      paperDefaultSlippageBps / 10000.0,
		matchInterval: paperDefaultMatchInterval,
		initial:       initialBalance,
		cash:          initialBalance,
		positions:     make(map[string]*paperPosition),
		posMode:       BitgetPositionModeHedge,
		leverage:      make(map[string]int),
		crossMode:     make(map[string]bool),
		fillIndex:     make(map[string]*PaperFill),
		lastMark:      make(map[string]float64),
		markCache:     make(map[string]paperMarkEntry),
	}
}

// SetSlippageBps sets the adverse slippage applied to every fill (default 2 bps).
func (t *BitgetPaperTrader) SetSlippageBps(bps float64) {
	if bps < 0 {
		bps = 0
	}
	t.mu.Lock()
	t.slippage = bps / 10000.0
	t.mu.Unlock()
}

// paperPosKey is the key of a position: symbol and side. Hedge mode holds a long and a short
// per symbol; one-way mode never holds both, so the key is unambiguous in both modes.
func paperPosKey(symbol, side string) string { return symbol + "|" + side }

// PositionMode returns the paper account's position mode (BitgetPositionModeHedge or
// BitgetPositionModeOneWay).
func (t *BitgetPaperTrader) PositionMode() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.posMode
}

// SetPositionMode applies the configured position mode ("hedge" / "one_way", empty = hedge).
// Like the real exchange it only switches while the account holds no position: with open
// positions it logs a warning and keeps operating in the current mode (a restored account
// keeps the mode it was saved with).
func (t *BitgetPaperTrader) SetPositionMode(mode string) {
	mode = NormalizeBitgetPositionMode(mode)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.posMode == mode {
		return
	}
	if len(t.positions) > 0 {
		logger.Warnf("⚠️ [BitgetPaper] cannot switch the paper account from %s to %s position mode while it holds %d open position(s): keeping %s (close all positions and restart the trader to apply %s)",
			t.posMode, mode, len(t.positions), t.posMode, mode)
		return
	}
	t.posMode = mode
	t.persistLocked()
	logger.Infof("  ✓ [BitgetPaper] position mode set to %s", mode)
}

// SetMatchInterval sets the matcher interval (default 5s). Takes effect on the next Start.
func (t *BitgetPaperTrader) SetMatchInterval(d time.Duration) {
	if d <= 0 {
		d = paperDefaultMatchInterval
	}
	t.mu.Lock()
	t.matchInterval = d
	t.mu.Unlock()
}

// SetSystemFillHandler registers a callback for fills produced by the matcher (stop loss,
// take profit, liquidation). It is invoked on the matcher goroutine without internal locks held.
func (t *BitgetPaperTrader) SetSystemFillHandler(h func(PaperFill)) {
	t.mu.Lock()
	t.onSysFill = h
	t.mu.Unlock()
}

// ----------------------------------------------------------- matcher lifecycle

// Start launches the background matcher if it is not running. It is idempotent and a
// no-op when the matcher is disabled (unit tests).
func (t *BitgetPaperTrader) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.autoMatch || t.stopCh != nil {
		return
	}
	stop := make(chan struct{})
	end := make(chan struct{})
	t.stopCh, t.matcherEnd = stop, end
	interval := t.matchInterval
	go func() {
		defer close(end)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				t.Tick()
			}
		}
	}()
}

// Stop halts the background matcher and waits for it to exit. Account state is kept; a
// later trade (or Start) restarts the matcher. While stopped, SL/TP, liquidation and
// funding are not evaluated; on the next Tick they are evaluated against the then-current
// mark price and any missed funding slots are settled.
func (t *BitgetPaperTrader) Stop() {
	t.mu.Lock()
	stop, end := t.stopCh, t.matcherEnd
	t.stopCh, t.matcherEnd = nil, nil
	t.mu.Unlock()
	if stop != nil {
		close(stop)
		<-end
	}
	// State is persisted on every mutation; this only retries a write that failed earlier.
	t.flushIfFailed()
}

// Close is an alias of Stop.
func (t *BitgetPaperTrader) Close() { t.Stop() }

var (
	paperRegistryMu sync.Mutex
	paperRegistry   = map[string]*BitgetPaperTrader{}
)

// AcquireBitgetPaperTrader returns the paper account registered for traderID, creating it
// (with initialBalance) on first use. The API reloads an AutoTrader on every start and
// config update, so the account must outlive the AutoTrader instance or a Stop/Start would
// silently reset balance and positions. An empty id returns an unregistered account.
//
// When a state directory is configured (SetPaperStateDir) the account is restored from the
// trader's snapshot file if one exists, so it also survives a process restart; initialBalance
// then only applies to accounts without a snapshot. Every later mutation is written back.
func AcquireBitgetPaperTrader(traderID string, initialBalance float64) *BitgetPaperTrader {
	return acquireBitgetPaperTrader(traderID, initialBalance, NewBitgetPaperTrader)
}

// acquireBitgetPaperTrader is AcquireBitgetPaperTrader with an injectable constructor (tests
// use a fake price source).
func acquireBitgetPaperTrader(traderID string, initialBalance float64, newTrader func(float64) *BitgetPaperTrader) *BitgetPaperTrader {
	if traderID == "" {
		return newTrader(initialBalance)
	}
	paperRegistryMu.Lock()
	defer paperRegistryMu.Unlock()
	if t, ok := paperRegistry[traderID]; ok {
		return t
	}
	t := newTrader(initialBalance)
	t.attachState(traderID)
	paperRegistry[traderID] = t
	return t
}

// ReleaseBitgetPaperTrader stops and forgets the paper account of a deleted trader and deletes
// its snapshot file.
func ReleaseBitgetPaperTrader(traderID string) {
	paperRegistryMu.Lock()
	t := paperRegistry[traderID]
	delete(paperRegistry, traderID)
	paperRegistryMu.Unlock()
	if t != nil {
		t.disablePersist() // a late mutation must not resurrect the file
		t.Stop()
	}
	removePaperSnapshot(traderID)
}

// ------------------------------------------------------------------- helpers

// paperSymbol normalizes a symbol to the Bitget contract name (same rules as the market data
// source: upper case, no xyz: prefix, USDT suffix).
func paperSymbol(symbol string) string {
	return market.NormalizeForSource(symbol, market.SourceBitget)
}

func paperFeeRate(c *bitgetapi.Contract) float64 {
	if c != nil && c.TakerFeeRate > 0 {
		return c.TakerFeeRate
	}
	return paperDefaultTakerFee
}

func paperFundHours(c *bitgetapi.Contract) int {
	if c != nil && c.FundInterval > 0 {
		return c.FundInterval
	}
	return paperDefaultFundHours
}

// paperFundingBoundary returns the most recent funding boundary (unix seconds, UTC-aligned to
// multiples of the interval) at or before t.
func paperFundingBoundary(t time.Time, hours int) int64 {
	step := int64(hours) * 3600
	u := t.UTC().Unix()
	return u - ((u%step)+step)%step
}

// paperFundingDue returns how many funding boundaries of the given interval lie in
// (settledThrough, now]. It is negative-safe: a marker in the future (clock moved back) yields 0.
func paperFundingDue(now time.Time, hours int, settledThrough int64) int64 {
	step := int64(hours) * 3600
	u := now.UTC().Unix()
	n := paperFloorDiv(u, step) - paperFloorDiv(settledThrough, step)
	if n < 0 {
		return 0
	}
	return n
}

func paperFloorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// paperStep returns the contract's quantity step.
func paperStep(c *bitgetapi.Contract) float64 {
	if c.SizeMultiplier > 0 {
		return c.SizeMultiplier
	}
	return math.Pow(10, -float64(c.VolumePlace))
}

// paperFloorQty floors to the contract step and strips float noise using volumePlace.
func paperFloorQty(c *bitgetapi.Contract, qty float64) float64 {
	q := bitgetFloorQuantity(qty, paperStep(c))
	if c.VolumePlace >= 0 {
		if v, err := strconv.ParseFloat(strconv.FormatFloat(q, 'f', c.VolumePlace, 64), 64); err == nil {
			q = v
		}
	}
	return q
}

// paperRoundPrice rounds a trigger price to the contract's price tick (reusing the live
// Bitget trader's rounding).
func paperRoundPrice(c *bitgetapi.Contract, price float64) float64 {
	s := roundBitgetPrice(&BitgetContract{PricePlace: c.PricePlace, PriceEndStep: c.PriceEndStep}, price)
	if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
		return v
	}
	return price
}

func paperClampLeverage(c *bitgetapi.Contract, lev int) int {
	if c.MinLever >= 1 && lev < int(c.MinLever) {
		lev = int(c.MinLever)
	}
	if c.MaxLever >= 1 && lev > int(c.MaxLever) {
		lev = int(c.MaxLever)
	}
	if lev < 1 {
		lev = 1
	}
	return lev
}

func (t *BitgetPaperTrader) nextIDLocked(prefix string) string {
	t.seq++
	return fmt.Sprintf("%s-%d-%d", prefix, t.now().UnixMilli(), t.seq)
}

func validPrice(p float64) bool { return p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0) }

// currentMark returns the mark price. fresh=false may serve a value cached for a few
// seconds (read-only calls); order placement and the matcher always pass fresh=true.
func (t *BitgetPaperTrader) currentMark(symbol string, fresh bool) (float64, error) {
	if !fresh && t.priceTTL > 0 {
		t.mu.Lock()
		e, ok := t.markCache[symbol]
		t.mu.Unlock()
		if ok && t.now().Sub(e.at) < t.priceTTL {
			return e.price, nil
		}
	}
	p, err := t.source.MarkPrice(symbol)
	if err != nil {
		return 0, err
	}
	if !validPrice(p) {
		return 0, fmt.Errorf("invalid mark price %v for %s", p, symbol)
	}
	t.mu.Lock()
	t.markCache[symbol] = paperMarkEntry{price: p, at: t.now()}
	t.lastMark[symbol] = p
	t.mu.Unlock()
	return p, nil
}

// heldMarks fetches marks (outside the lock) for every symbol with an open position.
func (t *BitgetPaperTrader) heldMarks(fresh bool) map[string]float64 {
	t.mu.Lock()
	seen := make(map[string]bool, len(t.positions))
	symbols := make([]string, 0, len(t.positions))
	for _, p := range t.positions {
		if !seen[p.symbol] {
			seen[p.symbol] = true
			symbols = append(symbols, p.symbol)
		}
	}
	t.mu.Unlock()
	marks := make(map[string]float64, len(symbols))
	for _, s := range symbols {
		if p, err := t.currentMark(s, fresh); err == nil {
			marks[s] = p
		} else {
			logger.Infof("  ⚠️ [BitgetPaper] mark price for %s unavailable, using last known: %v", s, err)
		}
	}
	return marks
}

// markFor returns the fetched mark, else the last known mark, else the entry price.
func (t *BitgetPaperTrader) markForLocked(p *paperPosition, marks map[string]float64) float64 {
	if m, ok := marks[p.symbol]; ok {
		return m
	}
	if m, ok := t.lastMark[p.symbol]; ok && m > 0 {
		return m
	}
	return p.entry
}

func (t *BitgetPaperTrader) sortedPositionsLocked() []*paperPosition {
	out := make([]*paperPosition, 0, len(t.positions))
	for _, p := range t.positions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].symbol != out[j].symbol {
			return out[i].symbol < out[j].symbol
		}
		return out[i].side < out[j].side
	})
	return out
}

func (t *BitgetPaperTrader) recordFillLocked(f *PaperFill) {
	t.fills = append(t.fills, f)
	t.fillIndex[f.OrderID] = f
	if len(t.fills) > paperMaxHistory {
		drop := len(t.fills) - paperMaxHistory
		for _, old := range t.fills[:drop] {
			delete(t.fillIndex, old.OrderID)
		}
		t.fills = append([]*PaperFill(nil), t.fills[drop:]...)
	}
}

func (t *BitgetPaperTrader) recordClosedLocked(r ClosedPnLRecord) {
	t.closed = append(t.closed, r)
	if len(t.closed) > paperMaxHistory {
		t.closed = append([]ClosedPnLRecord(nil), t.closed[len(t.closed)-paperMaxHistory:]...)
	}
}

func fillResult(f *PaperFill) map[string]interface{} {
	return map[string]interface{}{
		"orderId":     f.OrderID,
		"symbol":      f.Symbol,
		"status":      "FILLED",
		"avgPrice":    f.Price,
		"executedQty": f.Quantity,
		"commission":  f.Fee,
		"side":        f.Side,
	}
}

// ------------------------------------------------------------------ balance

// GetBalance returns the simulated USDT account. totalWalletBalance = free balance +
// locked margin; totalEquity adds unrealized PnL; availableBalance is the free balance.
func (t *BitgetPaperTrader) GetBalance() (map[string]interface{}, error) {
	marks := t.heldMarks(false)
	t.mu.Lock()
	defer t.mu.Unlock()
	margin, unreal := 0.0, 0.0
	for _, p := range t.positions {
		margin += p.margin
		unreal += p.unrealized(t.markForLocked(p, marks))
	}
	wallet := t.cash + margin
	equity := wallet + unreal
	return map[string]interface{}{
		"totalWalletBalance":    wallet,
		"availableBalance":      t.cash,
		"totalUnrealizedProfit": unreal,
		"totalEquity":           equity,
		"total_equity":          equity,
	}, nil
}

// GetPositions returns open positions in the common map format (positionAmt is positive for
// both sides, like the live Bitget trader).
func (t *BitgetPaperTrader) GetPositions() ([]map[string]interface{}, error) {
	marks := t.heldMarks(false)
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(t.positions))
	for _, p := range t.sortedPositionsLocked() {
		mark := t.markForLocked(p, marks)
		out = append(out, map[string]interface{}{
			"symbol":           p.symbol,
			"side":             p.side,
			"entryPrice":       p.entry,
			"markPrice":        mark,
			"positionAmt":      p.qty,
			"unRealizedProfit": p.unrealized(mark),
			"liquidationPrice": p.liquidationPrice(),
			"leverage":         float64(p.leverage),
			"createdTime":      p.openedAt.UnixMilli(),
			"updatedTime":      t.now().UnixMilli(),
		})
	}
	return out, nil
}

// GetMarketPrice returns the current mark price (the price all fills are based on).
func (t *BitgetPaperTrader) GetMarketPrice(symbol string) (float64, error) {
	return t.currentMark(paperSymbol(symbol), false)
}

// ------------------------------------------------------------------- orders

// OpenLong opens (or adds to) a long position with a simulated market buy.
func (t *BitgetPaperTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.open(symbol, "long", quantity, leverage)
}

// OpenShort opens (or adds to) a short position with a simulated market sell.
func (t *BitgetPaperTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return t.open(symbol, "short", quantity, leverage)
}

// CloseLong closes a long position (quantity 0 = all) with a simulated market sell.
func (t *BitgetPaperTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeManual(symbol, "long", quantity)
}

// CloseShort closes a short position (quantity 0 = all) with a simulated market buy.
func (t *BitgetPaperTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return t.closeManual(symbol, "short", quantity)
}

func (t *BitgetPaperTrader) open(symbol, side string, quantity float64, leverage int) (map[string]interface{}, error) {
	symbol = paperSymbol(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("open %s: empty symbol", side)
	}
	if !validPrice(quantity) {
		return nil, fmt.Errorf("open %s %s: invalid quantity %v", side, symbol, quantity)
	}
	c, err := t.source.Contract(symbol)
	if err != nil {
		return nil, fmt.Errorf("open %s %s: contract lookup failed: %w", side, symbol, err)
	}
	if !c.Tradable() {
		return nil, fmt.Errorf("open %s %s: contract is not tradable (symbolStatus=%q)", side, symbol, c.SymbolStatus)
	}
	if sessOpen, _, note := market.MarketSession(c.AssetClass(), t.now()); !sessOpen {
		return nil, fmt.Errorf("open %s %s: market closed (%s): %s", side, symbol, c.AssetClass(), note)
	}

	qty := paperFloorQty(c, quantity)
	if qty <= 0 || qty < c.MinTradeNum-1e-12 {
		return nil, fmt.Errorf("open %s %s: quantity %.8f is below minimum trade size %v (step %v)", side, symbol, quantity, c.MinTradeNum, paperStep(c))
	}

	mark, err := t.currentMark(symbol, true)
	if err != nil {
		return nil, fmt.Errorf("open %s %s: mark price unavailable: %w", side, symbol, err)
	}
	if c.MinTradeUSDT > 0 && qty*mark < c.MinTradeUSDT {
		return nil, fmt.Errorf("open %s %s: order value %.4f USDT is below minimum %v USDT", side, symbol, qty*mark, c.MinTradeUSDT)
	}

	t.mu.Lock()
	lev := leverage
	if lev <= 0 {
		lev = t.leverage[symbol]
	}
	if lev <= 0 {
		t.mu.Unlock()
		return nil, fmt.Errorf("open %s %s: leverage must be positive", side, symbol)
	}
	lev = paperClampLeverage(c, lev)
	t.leverage[symbol] = lev

	if t.posMode == BitgetPositionModeOneWay {
		opposite := "short"
		if side == "short" {
			opposite = "long"
		}
		if t.positions[paperPosKey(symbol, opposite)] != nil {
			t.mu.Unlock()
			return nil, fmt.Errorf("open %s %s: a %s position already exists (one-way mode), close it first", side, symbol, opposite)
		}
	}
	pos := t.positions[paperPosKey(symbol, side)]

	slip := t.slippage
	execPrice := mark * (1 + slip)
	action, orderSide := "open_long", "BUY"
	if side == "short" {
		execPrice = mark * (1 - slip)
		action, orderSide = "open_short", "SELL"
	}
	notional := execPrice * qty
	margin := notional / float64(lev)
	fee := notional * paperFeeRate(c)
	if margin+fee > t.cash+paperEps {
		avail := t.cash
		t.mu.Unlock()
		return nil, fmt.Errorf("open %s %s: insufficient available balance: need %.4f USDT (margin %.4f + fee %.4f), have %.4f", side, symbol, margin+fee, margin, fee, avail)
	}

	now := t.now()
	t.cash -= margin + fee
	if pos == nil {
		pos = &paperPosition{
			id:            t.nextIDLocked("paperpos"),
			symbol:        symbol,
			side:          side,
			qty:           qty,
			entry:         execPrice,
			leverage:      lev,
			margin:        margin,
			openFee:       fee,
			openedAt:      now,
			fundedThrough: now.UTC().Unix(),
		}
		t.positions[paperPosKey(symbol, side)] = pos
	} else {
		pos.entry = (pos.entry*pos.qty + execPrice*qty) / (pos.qty + qty)
		pos.qty += qty
		pos.margin += margin
		pos.openFee += fee
		if pos.margin > 0 { // funding can push the margin to <= 0: keep the leverage then
			pos.leverage = int(math.Max(1, math.Round(pos.entry*pos.qty/pos.margin)))
		}
	}

	f := &PaperFill{
		OrderID:      t.nextIDLocked("paper"),
		Symbol:       symbol,
		Action:       action,
		PositionSide: strings.ToUpper(side),
		Side:         orderSide,
		Price:        execPrice,
		Quantity:     qty,
		Fee:          fee,
		EntryPrice:   pos.entry,
		Leverage:     lev,
		Reason:       paperReasonManual,
		Time:         now,
	}
	t.recordFillLocked(f)
	t.persistLocked()
	t.mu.Unlock()

	logger.Infof("  📝 [BitgetPaper] %s %s qty=%.6f @ %.6f (mark %.6f, lev %dx, fee %.4f) id=%s", action, symbol, qty, execPrice, mark, lev, fee, f.OrderID)
	t.Start()
	return fillResult(f), nil
}

func (t *BitgetPaperTrader) closeManual(symbol, side string, quantity float64) (map[string]interface{}, error) {
	symbol = paperSymbol(symbol)
	if quantity < 0 {
		quantity = -quantity
	}
	c, err := t.source.Contract(symbol)
	if err != nil {
		return nil, fmt.Errorf("close %s %s: contract lookup failed: %w", side, symbol, err)
	}
	mark, err := t.currentMark(symbol, true)
	if err != nil {
		return nil, fmt.Errorf("close %s %s: mark price unavailable: %w", side, symbol, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	pos := t.positions[paperPosKey(symbol, side)]
	if pos == nil {
		return nil, fmt.Errorf("close %s %s: %s position not found", side, symbol, side)
	}
	qty := pos.qty
	if quantity > 0 {
		// reduce-only: floor to the step but never apply minimum size/notional checks, so a
		// residual below the minimum can always be closed
		qty = paperFloorQty(c, quantity)
		if qty <= 0 {
			return nil, fmt.Errorf("close %s %s: quantity %.8f rounds to zero (step %v)", side, symbol, quantity, paperStep(c))
		}
		if qty > pos.qty {
			qty = pos.qty
		}
	}
	execPrice := mark * (1 - t.slippage)
	if side == "short" {
		execPrice = mark * (1 + t.slippage)
	}
	f := t.closeLocked(pos, qty, execPrice, paperFeeRate(c), paperReasonManual)
	t.persistLocked()
	logger.Infof("  📝 [BitgetPaper] %s %s qty=%.6f @ %.6f pnl=%.4f fee=%.4f id=%s", f.Action, symbol, f.Quantity, f.Price, f.RealizedPnL, f.Fee, f.OrderID)
	return fillResult(f), nil
}

// closeLocked closes qty of pos at execPrice. Loss is capped at the position margin; a
// liquidation forfeits whatever equity is left as a liquidation fee. t.mu must be held.
func (t *BitgetPaperTrader) closeLocked(pos *paperPosition, qty, execPrice, feeRate float64, reason string) *PaperFill {
	if qty >= pos.qty-1e-12 {
		qty = pos.qty
	}
	share := qty / pos.qty
	marginPart := pos.margin * share
	openFeePart := pos.openFee * share
	gross := pos.dir() * (execPrice - pos.entry) * qty
	fee := execPrice * qty * feeRate

	if marginPart+gross < 0 && marginPart >= 0 {
		gross = -marginPart // loss beyond the margin is absorbed by the (unmodelled) insurance fund
	}
	// A negative margin (funding paid beyond the posted margin) is a debt against the position's
	// own PnL: the realized price PnL stays as is and only the payout is floored at zero.
	remaining := math.Max(0, marginPart+gross)
	if reason == paperReasonLiquidation || remaining-fee < 0 {
		fee = remaining // clearance fee: the leftover equity is forfeited
	}
	t.cash += remaining - fee
	t.realized += gross

	action, side := "close_long", "SELL"
	if pos.side == "short" {
		action, side = "close_short", "BUY"
	}
	now := t.now()
	f := &PaperFill{
		OrderID:      t.nextIDLocked("paper"),
		Symbol:       pos.symbol,
		Action:       action,
		PositionSide: strings.ToUpper(pos.side),
		Side:         side,
		Price:        execPrice,
		Quantity:     qty,
		Fee:          fee,
		RealizedPnL:  gross,
		EntryPrice:   pos.entry,
		Leverage:     pos.leverage,
		Reason:       reason,
		Time:         now,
	}
	t.recordFillLocked(f)
	t.recordClosedLocked(ClosedPnLRecord{
		Symbol:      pos.symbol,
		Side:        pos.side,
		EntryPrice:  pos.entry,
		ExitPrice:   execPrice,
		Quantity:    qty,
		RealizedPnL: gross,
		Fee:         openFeePart + fee,
		Leverage:    pos.leverage,
		EntryTime:   pos.openedAt,
		ExitTime:    now,
		OrderID:     f.OrderID,
		CloseType:   reason,
		ExchangeID:  pos.id,
	})

	pos.qty -= qty
	pos.margin -= marginPart
	pos.openFee -= openFeePart
	if pos.qty <= 1e-12 {
		delete(t.positions, paperPosKey(pos.symbol, pos.side))
	}
	return f
}

// SetLeverage sets the leverage used by the next open order of symbol (clamped to the
// contract's min/max leverage). An existing position keeps the leverage it was opened with.
func (t *BitgetPaperTrader) SetLeverage(symbol string, leverage int) error {
	symbol = paperSymbol(symbol)
	if leverage <= 0 {
		return fmt.Errorf("leverage must be positive, got %d", leverage)
	}
	c, err := t.source.Contract(symbol)
	if err != nil {
		return fmt.Errorf("failed to set leverage for %s: %w", symbol, err)
	}
	lev := paperClampLeverage(c, leverage)
	if lev != leverage {
		logger.Infof("  ⚠️ [BitgetPaper] %s leverage %dx clamped to %dx (contract limit)", symbol, leverage, lev)
	}
	t.mu.Lock()
	if t.leverage[symbol] != lev {
		t.leverage[symbol] = lev
		t.persistLocked()
	}
	t.mu.Unlock()
	return nil
}

// SetMarginMode records the mode. Both modes use the same isolated-style liquidation check.
func (t *BitgetPaperTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	sym := paperSymbol(symbol)
	t.mu.Lock()
	if cur, ok := t.crossMode[sym]; !ok || cur != isCrossMargin {
		t.crossMode[sym] = isCrossMargin
		t.persistLocked()
	}
	t.mu.Unlock()
	return nil
}

// FormatQuantity floors quantity to the contract step and applies the minimum size and
// notional checks, like the live Bitget trader.
func (t *BitgetPaperTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	symbol = paperSymbol(symbol)
	c, err := t.source.Contract(symbol)
	if err != nil {
		return "", err
	}
	qty := paperFloorQty(c, quantity)
	if qty <= 0 || qty < c.MinTradeNum-1e-12 {
		return "", fmt.Errorf("quantity %.8f for %s is below minimum trade size %v (step %v)", quantity, symbol, c.MinTradeNum, paperStep(c))
	}
	if c.MinTradeUSDT > 0 {
		if mark, perr := t.currentMark(symbol, false); perr == nil && qty*mark < c.MinTradeUSDT {
			return "", fmt.Errorf("order value %.4f USDT for %s is below minimum %v USDT", qty*mark, symbol, c.MinTradeUSDT)
		}
	}
	return strconv.FormatFloat(qty, 'f', c.VolumePlace, 64), nil
}

// ---------------------------------------------------------------- SL / TP

// triggerTargetLocked resolves the position a SL/TP belongs to. positionSide "long"/"short"
// (any case) names it exactly (hedge mode holds both); an empty / other value is only
// accepted when the symbol has exactly one position. t.mu must be held.
func (t *BitgetPaperTrader) triggerTargetLocked(symbol, positionSide string) (*paperPosition, error) {
	var held []*paperPosition
	for _, side := range []string{"long", "short"} {
		if p := t.positions[paperPosKey(symbol, side)]; p != nil {
			held = append(held, p)
		}
	}
	if len(held) == 0 {
		return nil, fmt.Errorf("no open position")
	}
	if ps := strings.ToLower(positionSide); ps == "long" || ps == "short" {
		for _, p := range held {
			if p.side == ps {
				return p, nil
			}
		}
		return nil, fmt.Errorf("no %s position (holding %s)", ps, held[0].side)
	}
	if len(held) > 1 {
		return nil, fmt.Errorf("both a long and a short position are open (hedge mode): specify the position side")
	}
	return held[0], nil
}

func (t *BitgetPaperTrader) setTrigger(symbol, positionSide string, price float64, isStop bool) error {
	symbol = paperSymbol(symbol)
	label := "take profit"
	if isStop {
		label = "stop loss"
	}
	if !validPrice(price) {
		return fmt.Errorf("invalid %s price %v for %s", label, price, symbol)
	}
	c, err := t.source.Contract(symbol)
	if err != nil {
		return fmt.Errorf("failed to set %s for %s: %w", label, symbol, err)
	}
	mark, err := t.currentMark(symbol, true)
	if err != nil {
		return fmt.Errorf("failed to set %s for %s: mark price unavailable: %w", label, symbol, err)
	}
	price = paperRoundPrice(c, price)

	t.mu.Lock()
	defer t.mu.Unlock()
	pos, err := t.triggerTargetLocked(symbol, positionSide)
	if err != nil {
		return fmt.Errorf("failed to set %s for %s: %w", label, symbol, err)
	}
	// Mirror the exchange: a trigger that is already on the wrong side of the mark is rejected.
	long := pos.side == "long"
	if (long == isStop && price >= mark) || (long != isStop && price <= mark) {
		return fmt.Errorf("failed to set %s for %s: trigger %.6f is not %s the mark price %.6f for a %s position",
			label, symbol, price, map[bool]string{true: "below", false: "above"}[long == isStop], mark, pos.side)
	}
	tr := &paperTrigger{id: t.nextIDLocked("paper-plan"), price: price}
	if isStop {
		pos.sl = tr
	} else {
		pos.tp = tr
	}
	t.persistLocked()
	logger.Infof("  ✓ [BitgetPaper] %s set: %s %s @ %.6f", label, symbol, pos.side, price)
	return nil
}

// SetStopLoss sets (replacing any existing) the position-level stop loss. quantity is
// ignored: the order always covers the whole position.
func (t *BitgetPaperTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	if err := t.setTrigger(symbol, positionSide, stopPrice, true); err != nil {
		return err
	}
	t.Start()
	return nil
}

// SetTakeProfit sets (replacing any existing) the position-level take profit.
func (t *BitgetPaperTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	if err := t.setTrigger(symbol, positionSide, takeProfitPrice, false); err != nil {
		return err
	}
	t.Start()
	return nil
}

// cancelTriggers removes the SL and/or TP of symbol. positionSide ("long"/"short", any case)
// limits it to one position of a hedge-mode symbol; "" covers every position of the symbol.
func (t *BitgetPaperTrader) cancelTriggers(symbol, positionSide string, sl, tp bool) {
	symbol = paperSymbol(symbol)
	side := bitgetDirection(positionSide)
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for _, ps := range []string{"long", "short"} {
		if side != "" && side != ps {
			continue
		}
		pos := t.positions[paperPosKey(symbol, ps)]
		if pos == nil {
			continue
		}
		if sl && pos.sl != nil {
			pos.sl = nil
			changed = true
		}
		if tp && pos.tp != nil {
			pos.tp = nil
			changed = true
		}
	}
	if changed {
		t.persistLocked()
	}
}

// CancelStopLossOrders removes the stop loss of symbol (both positions in hedge mode).
func (t *BitgetPaperTrader) CancelStopLossOrders(symbol string) error {
	t.cancelTriggers(symbol, "", true, false)
	return nil
}

// CancelTakeProfitOrders removes the take profit of symbol (both positions in hedge mode).
func (t *BitgetPaperTrader) CancelTakeProfitOrders(symbol string) error {
	t.cancelTriggers(symbol, "", false, true)
	return nil
}

// CancelStopOrders removes both stop loss and take profit of symbol (both positions in hedge mode).
func (t *BitgetPaperTrader) CancelStopOrders(symbol string) error {
	t.cancelTriggers(symbol, "", true, true)
	return nil
}

// CancelStopLossOrdersForSide removes only the stop loss of one position side ("LONG"/"SHORT").
func (t *BitgetPaperTrader) CancelStopLossOrdersForSide(symbol, positionSide string) error {
	t.cancelTriggers(symbol, positionSide, true, false)
	return nil
}

// CancelTakeProfitOrdersForSide removes only the take profit of one position side.
func (t *BitgetPaperTrader) CancelTakeProfitOrdersForSide(symbol, positionSide string) error {
	t.cancelTriggers(symbol, positionSide, false, true)
	return nil
}

// CancelStopOrdersForSide removes the stop loss and take profit of one position side.
func (t *BitgetPaperTrader) CancelStopOrdersForSide(symbol, positionSide string) error {
	t.cancelTriggers(symbol, positionSide, true, true)
	return nil
}

// CancelAllOrders removes all pending orders of symbol (only SL/TP exist: orders are market).
func (t *BitgetPaperTrader) CancelAllOrders(symbol string) error {
	t.cancelTriggers(symbol, "", true, true)
	return nil
}

// GetOpenOrders lists pending SL/TP orders (symbol "" = all symbols) using the same order
// types as the live Bitget trader.
func (t *BitgetPaperTrader) GetOpenOrders(symbol string) ([]OpenOrder, error) {
	symbol = paperSymbol(symbol)
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []OpenOrder{}
	for _, p := range t.sortedPositionsLocked() {
		if symbol != "" && p.symbol != symbol {
			continue
		}
		closeSide := "SELL"
		if p.side == "short" {
			closeSide = "BUY"
		}
		add := func(tr *paperTrigger, typ string) {
			if tr == nil {
				return
			}
			out = append(out, OpenOrder{
				OrderID:      tr.id,
				Symbol:       p.symbol,
				Side:         closeSide,
				PositionSide: strings.ToUpper(p.side),
				Type:         typ,
				StopPrice:    tr.price,
				Quantity:     p.qty,
				Status:       "NEW",
			})
		}
		add(p.sl, "STOP_MARKET")
		add(p.tp, "TAKE_PROFIT_MARKET")
	}
	return out, nil
}

// ------------------------------------------------------- order / pnl history

// GetOrderStatus reports a fill by order id (every order is filled immediately).
func (t *BitgetPaperTrader) GetOrderStatus(symbol string, orderID string) (map[string]interface{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.fillIndex[orderID]
	if !ok {
		return nil, fmt.Errorf("paper order %s not found", orderID)
	}
	return map[string]interface{}{
		"orderId":     f.OrderID,
		"symbol":      f.Symbol,
		"status":      "FILLED",
		"avgPrice":    f.Price,
		"executedQty": f.Quantity,
		"commission":  f.Fee,
		"side":        f.Side,
		"type":        "MARKET",
		"time":        f.Time.UnixMilli(),
		"updateTime":  f.Time.UnixMilli(),
	}, nil
}

// GetClosedPnL returns closing fills since startTime, newest first. RealizedPnL is the gross
// price PnL; Fee is the open + close trading fees (plus the liquidation clearance fee).
func (t *BitgetPaperTrader) GetClosedPnL(startTime time.Time, limit int) ([]ClosedPnLRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ClosedPnLRecord, 0, limit)
	for i := len(t.closed) - 1; i >= 0 && len(out) < limit; i-- {
		if t.closed[i].ExitTime.Before(startTime) {
			continue
		}
		out = append(out, t.closed[i])
	}
	return out, nil
}

// ------------------------------------------------------------------ matcher

type paperTickData struct {
	mark   float64
	c      *bitgetapi.Contract
	rate   float64
	rateOK bool
}

// Tick runs one matcher pass: funding settlement, liquidation, stop loss and take profit
// for every open position, against fresh mark prices. The background goroutine calls it
// every matchInterval; tests call it directly.
func (t *BitgetPaperTrader) Tick() {
	t.tickMu.Lock()
	defer t.tickMu.Unlock()

	// One entry per symbol: hedge mode can hold a long and a short of the same symbol, which
	// share the mark price, the contract and the funding rate.
	type held struct {
		symbol         string
		oldestFunded   int64 // earliest fundedThrough among the symbol's positions
		hasFundedEntry bool
	}
	t.mu.Lock()
	snapshot := make([]*held, 0, len(t.positions))
	bySymbol := make(map[string]*held, len(t.positions))
	for _, p := range t.sortedPositionsLocked() {
		h := bySymbol[p.symbol]
		if h == nil {
			h = &held{symbol: p.symbol}
			bySymbol[p.symbol] = h
			snapshot = append(snapshot, h)
		}
		if !h.hasFundedEntry || p.fundedThrough < h.oldestFunded {
			h.oldestFunded, h.hasFundedEntry = p.fundedThrough, true
		}
	}
	t.mu.Unlock()
	if len(snapshot) == 0 {
		return
	}

	// Network I/O happens without holding the state lock.
	data := make(map[string]paperTickData, len(snapshot))
	now := t.now()
	for _, h := range snapshot {
		mark, err := t.currentMark(h.symbol, true)
		if err != nil {
			logger.Infof("  ⚠️ [BitgetPaper] matcher: mark price for %s unavailable: %v", h.symbol, err)
			continue
		}
		d := paperTickData{mark: mark}
		if c, err := t.source.Contract(h.symbol); err == nil {
			d.c = c
		}
		if d.c != nil && paperFundingDue(now, paperFundHours(d.c), h.oldestFunded) > 0 {
			if r, err := t.source.FundingRate(h.symbol); err == nil {
				d.rate, d.rateOK = r, true
			} else {
				logger.Infof("  ⚠️ [BitgetPaper] matcher: funding rate for %s unavailable, retrying next tick: %v", h.symbol, err)
			}
		}
		data[h.symbol] = d
	}

	var sys []PaperFill
	t.mu.Lock()
	for _, pos := range t.sortedPositionsLocked() {
		d, ok := data[pos.symbol]
		if !ok {
			continue // opened after the snapshot or price unavailable
		}
		if f := t.matchPositionLocked(pos, d, now); f != nil {
			sys = append(sys, *f)
		}
	}
	if len(sys) > 0 || t.dirty {
		t.dirty = false
		t.persistLocked() // before the handler runs: the account state is durable first
	}
	handler := t.onSysFill
	t.mu.Unlock()

	if handler != nil {
		for _, f := range sys {
			handler(f)
		}
	}
}

// matchPositionLocked applies funding, then liquidation, SL and TP to one position.
// It returns the system fill when the position was (partly) closed.
func (t *BitgetPaperTrader) matchPositionLocked(pos *paperPosition, d paperTickData, now time.Time) *PaperFill {
	mark := d.mark
	t.lastMark[pos.symbol] = mark

	// 1) funding: one settlement per elapsed boundary, using the current rate. Needs the
	// contract: without it the interval is unknown and guessing one could count boundaries on a
	// wrong grid, so settlement is simply retried on the next tick.
	if d.rateOK && d.c != nil {
		hours := paperFundHours(d.c)
		if n := paperFundingDue(now, hours, pos.fundedThrough); n > 0 {
			if n > paperMaxFundingCatchUp {
				logger.Warnf("  ⚠️ [BitgetPaper] %s %s: %d funding boundaries due, settling only the last %d", pos.symbol, pos.side, n, paperMaxFundingCatchUp)
				n = paperMaxFundingCatchUp
			}
			// long pays when rate > 0, short receives (and vice versa)
			delta := -pos.dir() * pos.qty * mark * d.rate * float64(n)
			pos.margin += delta
			pos.funding += delta
			boundary := paperFundingBoundary(now, hours)
			pos.fundedThrough = boundary
			if boundary > t.lastFundingAt {
				t.lastFundingAt = boundary
			}
			t.dirty = true
			logger.Infof("  💸 [BitgetPaper] funding %s %s rate=%.6f x%d -> %+.4f USDT", pos.symbol, pos.side, d.rate, n, delta)
		}
	}

	feeRate := paperFeeRate(d.c)

	// 2) liquidation: margin + uPnL <= 0.5% of notional at mark
	if pos.margin+pos.unrealized(mark) <= paperMaintenanceMarginRate*mark*pos.qty {
		f := t.closeLocked(pos, pos.qty, mark, feeRate, paperReasonLiquidation)
		logger.Infof("  💥 [BitgetPaper] LIQUIDATED %s %s qty=%.6f mark=%.6f loss=%.4f", f.Symbol, f.PositionSide, f.Quantity, mark, f.RealizedPnL)
		return f
	}

	// 3) stop loss / take profit on the mark price, filled as a market order
	reason := ""
	long := pos.side == "long"
	switch {
	case pos.sl != nil && ((long && mark <= pos.sl.price) || (!long && mark >= pos.sl.price)):
		reason = paperReasonStopLoss
	case pos.tp != nil && ((long && mark >= pos.tp.price) || (!long && mark <= pos.tp.price)):
		reason = paperReasonTakeProfit
	}
	if reason == "" {
		return nil
	}
	execPrice := mark * (1 - t.slippage)
	if !long {
		execPrice = mark * (1 + t.slippage)
	}
	f := t.closeLocked(pos, pos.qty, execPrice, feeRate, reason)
	logger.Infof("  🎯 [BitgetPaper] %s triggered %s %s qty=%.6f mark=%.6f fill=%.6f pnl=%.4f", reason, f.Symbol, f.PositionSide, f.Quantity, mark, f.Price, f.RealizedPnL)
	return f
}
