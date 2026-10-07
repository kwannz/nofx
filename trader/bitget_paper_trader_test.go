package trader

import (
	"fmt"
	"math"
	bitgetapi "nofx/provider/bitget"
	"nofx/store"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var _ Trader = (*BitgetPaperTrader)(nil)

// ------------------------------------------------------------------ fakes

type fakePaperSource struct {
	mu        sync.Mutex
	marks     map[string]float64
	rates     map[string]float64
	contracts map[string]*bitgetapi.Contract
	markErr   error
}

func newFakePaperSource() *fakePaperSource {
	btc := &bitgetapi.Contract{
		Symbol: "BTCUSDT", BaseCoin: "BTC",
		MinTradeNum: 0.0001, SizeMultiplier: 0.0001, VolumePlace: 4,
		PricePlace: 1, PriceEndStep: 1, MinTradeUSDT: 5,
		MaxLever: 125, MinLever: 1, SymbolStatus: "normal",
		FundInterval: 8, TakerFeeRate: 0.0006,
	}
	nvda := &bitgetapi.Contract{
		Symbol: "NVDAUSDT", BaseCoin: "NVDA", IsRwa: true,
		MinTradeNum: 0.01, SizeMultiplier: 0.01, VolumePlace: 2,
		PricePlace: 2, PriceEndStep: 1, MinTradeUSDT: 5,
		MaxLever: 25, MinLever: 1, SymbolStatus: "normal",
		FundInterval: 8, TakerFeeRate: 0.0006,
	}
	return &fakePaperSource{
		marks:     map[string]float64{"BTCUSDT": 100, "NVDAUSDT": 100},
		rates:     map[string]float64{},
		contracts: map[string]*bitgetapi.Contract{"BTCUSDT": btc, "NVDAUSDT": nvda},
	}
}

func (f *fakePaperSource) setMark(symbol string, p float64) {
	f.mu.Lock()
	f.marks[symbol] = p
	f.mu.Unlock()
}

func (f *fakePaperSource) setRate(symbol string, r float64) {
	f.mu.Lock()
	f.rates[symbol] = r
	f.mu.Unlock()
}

func (f *fakePaperSource) MarkPrice(symbol string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return 0, f.markErr
	}
	p, ok := f.marks[symbol]
	if !ok {
		return 0, fmt.Errorf("no price for %s", symbol)
	}
	return p, nil
}

func (f *fakePaperSource) Contract(symbol string) (*bitgetapi.Contract, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.contracts[symbol]
	if !ok {
		return nil, fmt.Errorf("contract %s not found", symbol)
	}
	cc := *c
	return &cc, nil
}

func (f *fakePaperSource) FundingRate(symbol string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[symbol], nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// Monday 2026-10-05 12:00 UTC = 08:00 EDT: equity perps are open.
var paperMonday = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func newTestPaper(t *testing.T, initial float64) (*BitgetPaperTrader, *fakePaperSource, *fakeClock) {
	t.Helper()
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := newBitgetPaperTrader(initial, src, clk.Now)
	return p, src, clk
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s: got %.10f want %.10f", name, got, want)
	}
}

func balanceOf(t *testing.T, p *BitgetPaperTrader) map[string]interface{} {
	t.Helper()
	b, err := p.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustFloat(t *testing.T, m map[string]interface{}, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("key %q missing or not float64: %#v", key, m[key])
	}
	return v
}

const (
	slip = 0.0002
	fee  = 0.0006
)

// ------------------------------------------------------------------ tests

func TestPaperDefaults(t *testing.T) {
	p := NewBitgetPaperTrader(0)
	defer p.Stop()
	b := balanceOf(t, p)
	near(t, "default balance", mustFloat(t, b, "totalEquity"), 10000)
	if p.slippage != 0.0002 {
		t.Fatalf("default slippage = %v", p.slippage)
	}
	if p.matchInterval != 5*time.Second {
		t.Fatalf("default match interval = %v", p.matchInterval)
	}
	p.mu.Lock()
	running := p.stopCh != nil
	p.mu.Unlock()
	if running {
		t.Fatal("matcher must not run before the first trade")
	}
	if got := NewBitgetPaperTrader(2500); mustFloat(t, balanceOfNoT(got), "totalEquity") != 2500 {
		t.Fatal("custom initial balance ignored")
	}
}

func balanceOfNoT(p *BitgetPaperTrader) map[string]interface{} {
	b, _ := p.GetBalance()
	return b
}

func TestPaperOpenCloseLong_FeeAndPnL(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)

	res, err := p.OpenLong("BTCUSDT", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	entry := 100 * (1 + slip)
	notional := entry * 10
	margin := notional / 10
	openFee := notional * fee
	near(t, "avgPrice", res["avgPrice"].(float64), entry)
	near(t, "executedQty", res["executedQty"].(float64), 10)
	near(t, "commission", res["commission"].(float64), openFee)
	if res["status"] != "FILLED" {
		t.Fatalf("status = %v", res["status"])
	}

	b := balanceOf(t, p)
	near(t, "available after open", mustFloat(t, b, "availableBalance"), 10000-margin-openFee)
	near(t, "wallet after open", mustFloat(t, b, "totalWalletBalance"), 10000-openFee)
	near(t, "unrealized at entry mark", mustFloat(t, b, "totalUnrealizedProfit"), (100-entry)*10)

	// price rises 10%
	src.setMark("BTCUSDT", 110)
	b = balanceOf(t, p)
	wantUPnL := (110 - entry) * 10
	near(t, "unrealized", mustFloat(t, b, "totalUnrealizedProfit"), wantUPnL)
	near(t, "equity", mustFloat(t, b, "totalEquity"), 10000-openFee+wantUPnL)

	pos, _ := p.GetPositions()
	if len(pos) != 1 {
		t.Fatalf("positions = %d", len(pos))
	}
	near(t, "pos unrealized", pos[0]["unRealizedProfit"].(float64), wantUPnL)

	// close everything
	cres, err := p.CloseLong("BTCUSDT", 0)
	if err != nil {
		t.Fatal(err)
	}
	exit := 110 * (1 - slip)
	closeFee := exit * 10 * fee
	gross := (exit - entry) * 10
	near(t, "close avgPrice", cres["avgPrice"].(float64), exit)
	near(t, "close commission", cres["commission"].(float64), closeFee)

	b = balanceOf(t, p)
	want := 10000 - openFee + gross - closeFee
	near(t, "final balance", mustFloat(t, b, "totalEquity"), want)
	near(t, "available == equity when flat", mustFloat(t, b, "availableBalance"), want)
	if pos, _ := p.GetPositions(); len(pos) != 0 {
		t.Fatalf("position should be closed: %v", pos)
	}

	recs, _ := p.GetClosedPnL(paperMonday.Add(-time.Hour), 10)
	if len(recs) != 1 {
		t.Fatalf("closed records = %d", len(recs))
	}
	r := recs[0]
	if r.Symbol != "BTCUSDT" || r.Side != "long" || r.CloseType != "manual" {
		t.Fatalf("bad record %+v", r)
	}
	near(t, "record pnl (gross)", r.RealizedPnL, gross)
	near(t, "record fee (open+close)", r.Fee, openFee+closeFee)
	near(t, "record entry", r.EntryPrice, entry)
	near(t, "record exit", r.ExitPrice, exit)
	if r.OrderID != cres["orderId"].(string) {
		t.Fatalf("record order id %q != %v", r.OrderID, cres["orderId"])
	}
}

func TestPaperOpenCloseShort(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	if _, err := p.OpenShort("BTCUSDT", 5, 5); err != nil {
		t.Fatal(err)
	}
	entry := 100 * (1 - slip)
	src.setMark("BTCUSDT", 90)
	pos, _ := p.GetPositions()
	if pos[0]["side"] != "short" || pos[0]["positionAmt"].(float64) != 5 {
		t.Fatalf("bad short position %v", pos[0])
	}
	near(t, "short uPnL", pos[0]["unRealizedProfit"].(float64), (entry-90)*5)
	liq := pos[0]["liquidationPrice"].(float64)
	if liq <= entry {
		t.Fatalf("short liquidation price %v must be above entry %v", liq, entry)
	}
	if _, err := p.CloseShort("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	exit := 90 * (1 + slip)
	openFee := entry * 5 * fee
	closeFee := exit * 5 * fee
	want := 10000 - openFee + (entry-exit)*5 - closeFee
	near(t, "short final balance", mustFloat(t, balanceOf(t, p), "totalEquity"), want)
}

func TestPaperQuantityFlooredAndPartialClose(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	res, err := p.OpenLong("BTCUSDT", 1.23456789, 3)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "floored qty", res["executedQty"].(float64), 1.2345)

	// partial close of 0.5, remainder stays
	if _, err := p.CloseLong("BTCUSDT", 0.5); err != nil {
		t.Fatal(err)
	}
	pos, _ := p.GetPositions()
	near(t, "remaining", pos[0]["positionAmt"].(float64), 0.7345)

	// add to the same side: entry price is averaged
	if _, err := p.OpenLong("BTCUSDT", 1, 3); err != nil {
		t.Fatal(err)
	}
	pos, _ = p.GetPositions()
	near(t, "qty after add", pos[0]["positionAmt"].(float64), 1.7345)

	// closing more than held closes the position
	if _, err := p.CloseLong("BTCUSDT", 99); err != nil {
		t.Fatal(err)
	}
	if pos, _ := p.GetPositions(); len(pos) != 0 {
		t.Fatalf("expected flat, got %v", pos)
	}
	if _, err := p.CloseLong("BTCUSDT", 0); err == nil {
		t.Fatal("closing a flat symbol must fail")
	}
}

func TestPaperRejections(t *testing.T) {
	p, src, _ := newTestPaper(t, 100)

	// insufficient balance: 10 BTC @ 100 with 1x needs 1000
	if _, err := p.OpenLong("BTCUSDT", 10, 1); err == nil || !strings.Contains(err.Error(), "insufficient") {
		t.Fatalf("expected insufficient balance error, got %v", err)
	}
	// the margin + fee boundary: 1x leverage on the whole balance fails because of the fee
	if _, err := p.OpenLong("BTCUSDT", 1, 1); err == nil {
		t.Fatal("100 USDT margin + fee exceeds a 100 USDT balance")
	}
	// nothing was charged
	near(t, "balance untouched", mustFloat(t, balanceOf(t, p), "availableBalance"), 100)

	// below minimum quantity (and rounds to zero)
	if _, err := p.OpenLong("BTCUSDT", 0.00005, 10); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("expected min quantity error, got %v", err)
	}
	// below minimum notional: 0.01 * 100 = 1 USDT < 5
	if _, err := p.OpenLong("BTCUSDT", 0.01, 10); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("expected min notional error, got %v", err)
	}
	if _, err := p.OpenLong("BTCUSDT", 0, 10); err == nil {
		t.Fatal("zero quantity must fail")
	}
	if _, err := p.OpenLong("BTCUSDT", 1, 0); err == nil {
		t.Fatal("zero leverage without a stored setting must fail")
	}
	if _, err := p.OpenLong("DOGEUSDT", 1, 5); err == nil {
		t.Fatal("unknown contract must fail")
	}

	// not tradable
	src.mu.Lock()
	src.contracts["BTCUSDT"].SymbolStatus = "maintain"
	src.mu.Unlock()
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err == nil || !strings.Contains(err.Error(), "not tradable") {
		t.Fatalf("expected not tradable error, got %v", err)
	}
	src.mu.Lock()
	src.contracts["BTCUSDT"].SymbolStatus = "normal"
	src.mu.Unlock()

	// price feed down
	src.mu.Lock()
	src.markErr = fmt.Errorf("network down")
	src.mu.Unlock()
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err == nil {
		t.Fatal("open without a price must fail")
	}
	src.mu.Lock()
	src.markErr = nil
	src.mu.Unlock()

	// a rejected order leaves no position behind
	if pos, _ := p.GetPositions(); len(pos) != 0 {
		t.Fatalf("rejections must not create positions: %v", pos)
	}
}

func TestPaperOppositeSideRejected(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	p.SetPositionMode(BitgetPositionModeOneWay) // hedge (the default) allows both sides
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err == nil || !strings.Contains(err.Error(), "one-way") {
		t.Fatalf("expected one-way error, got %v", err)
	}
	if _, err := p.CloseShort("BTCUSDT", 0); err == nil {
		t.Fatal("closing the wrong side must fail")
	}
	// after closing the long, the short can be opened
	if _, err := p.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
}

func TestPaperClosedSessionRejectsEquityOpen(t *testing.T) {
	p, _, clk := newTestPaper(t, 10000)

	// Monday: equity perps open
	res, err := p.OpenLong("NVDAUSDT", 5, 5)
	if err != nil {
		t.Fatalf("equity open on Monday: %v", err)
	}
	near(t, "nvda qty", res["executedQty"].(float64), 5)
	if _, err := p.CloseLong("NVDAUSDT", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenLong("NVDAUSDT", 5, 5); err != nil {
		t.Fatal(err)
	}

	// Saturday 13:00 ET: equity closed, crypto still open
	clk.Set(time.Date(2026, 10, 10, 17, 0, 0, 0, time.UTC))
	if _, err := p.OpenShort("NVDAUSDT", 1, 5); err == nil {
		t.Fatal("equity open must be rejected on Saturday")
	}
	// the Saturday rejection is about the session, not the open long (message mentions closed)
	_, err = p.OpenLong("NVDAUSDT", 1, 5)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected market closed error, got %v", err)
	}
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("crypto must stay tradable on Saturday: %v", err)
	}
	// closing is always allowed
	if _, err := p.CloseLong("NVDAUSDT", 0); err != nil {
		t.Fatalf("close during closed session: %v", err)
	}
}

func TestPaperLeverageClampAndStoredSetting(t *testing.T) {
	p, _, _ := newTestPaper(t, 100000)
	if err := p.SetLeverage("NVDAUSDT", 100); err != nil {
		t.Fatal(err)
	}
	// stored (clamped) leverage is used when the order passes 0
	if _, err := p.OpenLong("NVDAUSDT", 10, 0); err != nil {
		t.Fatal(err)
	}
	pos, _ := p.GetPositions()
	if pos[0]["leverage"].(float64) != 25 {
		t.Fatalf("leverage = %v, want clamped to 25", pos[0]["leverage"])
	}
	// order-level leverage above max is clamped too
	if _, err := p.OpenLong("BTCUSDT", 1, 1000); err != nil {
		t.Fatal(err)
	}
	for _, ps := range mustPositions(t, p) {
		if ps["symbol"] == "BTCUSDT" && ps["leverage"].(float64) != 125 {
			t.Fatalf("btc leverage = %v", ps["leverage"])
		}
	}
	if err := p.SetLeverage("BTCUSDT", 0); err == nil {
		t.Fatal("non-positive leverage must be rejected")
	}
	if err := p.SetMarginMode("BTCUSDT", false); err != nil {
		t.Fatal(err)
	}
}

func mustPositions(t *testing.T, p *BitgetPaperTrader) []map[string]interface{} {
	t.Helper()
	pos, err := p.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestPaperStopLossLong(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })

	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	entry := 100 * (1 + slip)
	if err := p.SetStopLoss("BTCUSDT", "LONG", 10, 95); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "LONG", 10, 120); err != nil {
		t.Fatal(err)
	}

	orders, _ := p.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 {
		t.Fatalf("open orders = %v", orders)
	}
	if orders[0].Type != "STOP_MARKET" || orders[0].StopPrice != 95 || orders[0].Side != "SELL" || orders[0].PositionSide != "LONG" || orders[0].Status != "NEW" {
		t.Fatalf("bad SL order %+v", orders[0])
	}
	if orders[1].Type != "TAKE_PROFIT_MARKET" || orders[1].StopPrice != 120 {
		t.Fatalf("bad TP order %+v", orders[1])
	}
	if all, _ := p.GetOpenOrders(""); len(all) != 2 {
		t.Fatalf("all symbols: %v", all)
	}
	if other, _ := p.GetOpenOrders("ETHUSDT"); len(other) != 0 {
		t.Fatalf("other symbol: %v", other)
	}

	// not yet
	src.setMark("BTCUSDT", 95.01)
	p.Tick()
	if len(mustPositions(t, p)) != 1 {
		t.Fatal("SL must not trigger above the stop")
	}

	// mark hits the stop
	src.setMark("BTCUSDT", 95)
	p.Tick()
	if len(mustPositions(t, p)) != 0 {
		t.Fatal("SL must close the position")
	}
	if left, _ := p.GetOpenOrders(""); len(left) != 0 {
		t.Fatalf("SL/TP must be removed with the position: %v", left)
	}

	exit := 95 * (1 - slip)
	openFee := entry * 10 * fee
	closeFee := exit * 10 * fee
	want := 10000 - openFee + (exit-entry)*10 - closeFee
	near(t, "balance after SL", mustFloat(t, balanceOf(t, p), "totalEquity"), want)

	recs, _ := p.GetClosedPnL(time.Time{}, 10)
	if len(recs) != 1 || recs[0].CloseType != "stop_loss" {
		t.Fatalf("closed records: %+v", recs)
	}
	if len(got) != 1 || got[0].Reason != "stop_loss" || got[0].Action != "close_long" {
		t.Fatalf("system fill handler: %+v", got)
	}
	// the matcher's fill is queryable like any other order
	st, err := p.GetOrderStatus("BTCUSDT", got[0].OrderID)
	if err != nil || st["status"] != "FILLED" {
		t.Fatalf("order status: %v %v", st, err)
	}
}

func TestPaperTakeProfitShort(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })

	if _, err := p.OpenShort("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "SHORT", 10, 105); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "SHORT", 10, 90); err != nil {
		t.Fatal(err)
	}
	orders, _ := p.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 || orders[0].Side != "BUY" || orders[0].PositionSide != "SHORT" {
		t.Fatalf("bad short orders %+v", orders)
	}

	src.setMark("BTCUSDT", 89)
	p.Tick()
	if len(mustPositions(t, p)) != 0 {
		t.Fatal("TP must close the short")
	}
	recs, _ := p.GetClosedPnL(time.Time{}, 10)
	if len(recs) != 1 || recs[0].CloseType != "take_profit" || recs[0].RealizedPnL <= 0 {
		t.Fatalf("closed records: %+v", recs)
	}
	if len(got) != 1 || got[0].Reason != "take_profit" || got[0].Action != "close_short" {
		t.Fatalf("handler: %+v", got)
	}
}

func TestPaperShortStopLossTriggersUp(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	if _, err := p.OpenShort("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "SHORT", 10, 105); err != nil {
		t.Fatal(err)
	}
	src.setMark("BTCUSDT", 104.9)
	p.Tick()
	if len(mustPositions(t, p)) != 1 {
		t.Fatal("must stay open below the stop")
	}
	src.setMark("BTCUSDT", 105)
	p.Tick()
	recs, _ := p.GetClosedPnL(time.Time{}, 10)
	if len(recs) != 1 || recs[0].CloseType != "stop_loss" || recs[0].RealizedPnL >= 0 {
		t.Fatalf("closed records: %+v", recs)
	}
}

func TestPaperSLTPReplaceCancelValidate(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	if err := p.SetStopLoss("BTCUSDT", "LONG", 1, 90); err == nil {
		t.Fatal("SL without a position must fail")
	}
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	// wrong side of the mark is rejected like the exchange does
	if err := p.SetStopLoss("BTCUSDT", "LONG", 1, 101); err == nil {
		t.Fatal("long SL above mark must fail")
	}
	if err := p.SetTakeProfit("BTCUSDT", "LONG", 1, 99); err == nil {
		t.Fatal("long TP below mark must fail")
	}
	if err := p.SetStopLoss("BTCUSDT", "SHORT", 1, 90); err == nil {
		t.Fatal("SL for the wrong side must fail")
	}
	if err := p.SetStopLoss("BTCUSDT", "LONG", 1, -1); err == nil {
		t.Fatal("invalid price must fail")
	}

	// one SL and one TP per position; setting again replaces
	_ = p.SetStopLoss("BTCUSDT", "LONG", 1, 90)
	_ = p.SetStopLoss("BTCUSDT", "LONG", 1, 92.34)
	_ = p.SetTakeProfit("BTCUSDT", "LONG", 1, 110)
	orders, _ := p.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 {
		t.Fatalf("expected one SL + one TP, got %v", orders)
	}
	near(t, "replaced SL", orders[0].StopPrice, 92.3) // rounded to PricePlace=1

	if err := p.CancelStopLossOrders("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	orders, _ = p.GetOpenOrders("BTCUSDT")
	if len(orders) != 1 || orders[0].Type != "TAKE_PROFIT_MARKET" {
		t.Fatalf("after cancel SL: %v", orders)
	}
	_ = p.SetStopLoss("BTCUSDT", "LONG", 1, 90)
	if err := p.CancelTakeProfitOrders("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	orders, _ = p.GetOpenOrders("BTCUSDT")
	if len(orders) != 1 || orders[0].Type != "STOP_MARKET" {
		t.Fatalf("after cancel TP: %v", orders)
	}
	_ = p.SetTakeProfit("BTCUSDT", "LONG", 1, 110)
	if err := p.CancelStopOrders("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	if orders, _ = p.GetOpenOrders("BTCUSDT"); len(orders) != 0 {
		t.Fatalf("after cancel stops: %v", orders)
	}
	_ = p.SetStopLoss("BTCUSDT", "LONG", 1, 90)
	_ = p.SetTakeProfit("BTCUSDT", "LONG", 1, 110)
	if err := p.CancelAllOrders("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	if orders, _ = p.GetOpenOrders(""); len(orders) != 0 {
		t.Fatalf("after cancel all: %v", orders)
	}
	// cancelling on a flat symbol is harmless
	if err := p.CancelAllOrders("ETHUSDT"); err != nil {
		t.Fatal(err)
	}
}

func TestPaperLiquidationLong(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })

	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	entry := 100 * (1 + slip)
	margin := entry * 10 / 10
	openFee := entry * 10 * fee

	pos := mustPositions(t, p)
	liq := pos[0]["liquidationPrice"].(float64)
	wantLiq := (entry*10 - margin) / (10 * (1 - 0.005))
	near(t, "liquidation price", liq, wantLiq)

	// just above liquidation: survives
	src.setMark("BTCUSDT", liq+0.01)
	p.Tick()
	if len(mustPositions(t, p)) != 1 {
		t.Fatal("must survive above the liquidation price")
	}

	src.setMark("BTCUSDT", liq-0.01)
	p.Tick()
	if len(mustPositions(t, p)) != 0 {
		t.Fatal("must be liquidated below the liquidation price")
	}
	// the whole position margin is lost, nothing beyond it
	b := balanceOf(t, p)
	near(t, "equity after liquidation", mustFloat(t, b, "totalEquity"), 10000-margin-openFee)
	near(t, "available after liquidation", mustFloat(t, b, "availableBalance"), 10000-margin-openFee)
	recs, _ := p.GetClosedPnL(time.Time{}, 10)
	if len(recs) != 1 || recs[0].CloseType != "liquidation" {
		t.Fatalf("records %+v", recs)
	}
	if len(got) != 1 || got[0].Reason != "liquidation" {
		t.Fatalf("handler %+v", got)
	}
}

func TestPaperLiquidationGapIsCappedAtMargin(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	if _, err := p.OpenShort("BTCUSDT", 10, 20); err != nil {
		t.Fatal(err)
	}
	entry := 100 * (1 - slip)
	margin := entry * 10 / 20
	openFee := entry * 10 * fee
	// price gaps far beyond the liquidation price
	src.setMark("BTCUSDT", 300)
	p.Tick()
	near(t, "loss capped at margin", mustFloat(t, balanceOf(t, p), "totalEquity"), 10000-margin-openFee)
	if len(mustPositions(t, p)) != 0 {
		t.Fatal("must be liquidated")
	}
}

func TestPaperFundingSettlement(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)

	// open at 07:59:00 UTC, one minute before the 08:00 settlement
	clk.Set(time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("NVDAUSDT", 5, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.0001)
	src.setRate("NVDAUSDT", 0.0002)

	baseEq := mustFloat(t, balanceOf(t, p), "totalEquity")
	// before the boundary nothing happens
	p.Tick()
	near(t, "no funding before 08:00", mustFloat(t, balanceOf(t, p), "totalEquity"), baseEq)

	clk.Set(time.Date(2026, 10, 5, 8, 0, 5, 0, time.UTC))
	p.Tick()
	// long pays 100 * 10 * 0.0001 = 0.1; short receives 100 * 5 * 0.0002 = 0.1
	afterEq := mustFloat(t, balanceOf(t, p), "totalEquity")
	near(t, "equity change after funding", afterEq-baseEq, -(100*10*0.0001)+(100*5*0.0002))

	// settling again within the same slot is a no-op
	p.Tick()
	near(t, "idempotent within a slot", mustFloat(t, balanceOf(t, p), "totalEquity"), afterEq)

	// next boundary 16:00 settles again; a negative rate flips the direction
	src.setRate("BTCUSDT", -0.0003)
	src.setRate("NVDAUSDT", 0)
	clk.Set(time.Date(2026, 10, 5, 16, 0, 1, 0, time.UTC))
	p.Tick()
	near(t, "second settlement", mustFloat(t, balanceOf(t, p), "totalEquity")-afterEq, 100*10*0.0003)

	// funding is part of the position margin and is returned on close
	if _, err := p.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
}

func TestPaperFundingUsesContractInterval(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	src.mu.Lock()
	src.contracts["BTCUSDT"].FundInterval = 4
	src.mu.Unlock()
	clk.Set(time.Date(2026, 10, 5, 3, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.0001)
	base := mustFloat(t, balanceOf(t, p), "totalEquity")
	clk.Set(time.Date(2026, 10, 5, 4, 0, 1, 0, time.UTC)) // 4h boundary, not an 8h one
	p.Tick()
	near(t, "4h interval settles at 04:00", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -0.1)
}

func setPaperFundInterval(src *fakePaperSource, symbol string, hours int) {
	src.mu.Lock()
	src.contracts[symbol].FundInterval = hours
	src.mu.Unlock()
}

// The funding marker is a timestamp: when the exchange shortens the interval from 8h to 1h while
// a position is open, only the 1h boundaries that really passed since the open are settled
// (a slot index in units of the old interval would produce a burst of bogus payments).
func TestPaperFundingIntervalShortenedWhileOpen(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	clk.Set(time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil { // 8h interval at open
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.0001)
	setPaperFundInterval(src, "BTCUSDT", 1)

	base := mustFloat(t, balanceOf(t, p), "totalEquity")
	perPayment := 100 * 10 * 0.0001 // mark 100 * qty 10 * rate

	// 10:00:05 -> boundaries 08:00, 09:00, 10:00 passed since the 07:59 open
	clk.Set(time.Date(2026, 10, 5, 10, 0, 5, 0, time.UTC))
	p.Tick()
	near(t, "exactly 3 hourly payments", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -3*perPayment)

	p.Tick()
	near(t, "idempotent within the hour", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -3*perPayment)

	clk.Set(time.Date(2026, 10, 5, 11, 0, 1, 0, time.UTC))
	p.Tick()
	near(t, "one more payment at 11:00", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -4*perPayment)

	// the marker survives a snapshot round trip as a timestamp
	p.mu.Lock()
	snap := p.snapshotLocked()
	p.mu.Unlock()
	want := time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC).Unix()
	if snap.Version != paperSnapshotVersion || len(snap.Positions) != 1 || snap.Positions[0].FundingBoundary != want || snap.LastFundingAt != want {
		t.Fatalf("snapshot funding markers: version=%d positions=%+v last=%d, want boundary %d", snap.Version, snap.Positions, snap.LastFundingAt, want)
	}
}

// Lengthening the interval (1h -> 8h) must not pay anything until the next 8h boundary.
func TestPaperFundingIntervalLengthenedWhileOpen(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	setPaperFundInterval(src, "BTCUSDT", 1)
	clk.Set(time.Date(2026, 10, 5, 8, 30, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.0001)
	setPaperFundInterval(src, "BTCUSDT", 8)
	base := mustFloat(t, balanceOf(t, p), "totalEquity")

	clk.Set(time.Date(2026, 10, 5, 15, 59, 0, 0, time.UTC))
	p.Tick()
	near(t, "no payment before 16:00", mustFloat(t, balanceOf(t, p), "totalEquity"), base)

	clk.Set(time.Date(2026, 10, 5, 16, 0, 1, 0, time.UTC))
	p.Tick()
	near(t, "one payment at 16:00", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -0.1)
}

// Without a contract the interval is unknown: funding must be retried, not settled on a guessed grid.
func TestPaperFundingWaitsForContract(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	clk.Set(time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.0001)
	base := mustFloat(t, balanceOf(t, p), "totalEquity")

	src.mu.Lock()
	saved := src.contracts["BTCUSDT"]
	delete(src.contracts, "BTCUSDT")
	src.mu.Unlock()
	clk.Set(time.Date(2026, 10, 5, 8, 0, 5, 0, time.UTC))
	p.Tick()
	near(t, "nothing settled while the contract is unavailable", mustFloat(t, balanceOf(t, p), "totalEquity"), base)

	src.mu.Lock()
	src.contracts["BTCUSDT"] = saved
	src.mu.Unlock()
	p.Tick()
	near(t, "settled once the contract is back", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -0.1)
}

func TestPaperFundingCatchUpIsCapped(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	setPaperFundInterval(src, "BTCUSDT", 1)
	clk.Set(time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.00001)
	base := mustFloat(t, balanceOf(t, p), "totalEquity")
	clk.Set(time.Date(2026, 10, 20, 0, 0, 1, 0, time.UTC)) // ~455 hourly boundaries later
	p.Tick()
	near(t, "capped at paperMaxFundingCatchUp payments", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -float64(paperMaxFundingCatchUp)*100*10*0.00001)
	// the remainder is dropped, not carried over
	p.Tick()
	near(t, "no further payments in the same hour", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -float64(paperMaxFundingCatchUp)*100*10*0.00001)
}

func TestPaperFundingBoundaryMath(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }
	if got, want := paperFundingBoundary(at(9, 30), 8), at(8, 0).Unix(); got != want {
		t.Errorf("boundary 8h = %d, want %d", got, want)
	}
	if got, want := paperFundingBoundary(at(9, 30), 4), at(8, 0).Unix(); got != want {
		t.Errorf("boundary 4h = %d, want %d", got, want)
	}
	if got, want := paperFundingBoundary(at(8, 0), 8), at(8, 0).Unix(); got != want {
		t.Errorf("boundary exactly on the grid = %d, want %d", got, want)
	}
	if n := paperFundingDue(at(10, 0), 1, at(7, 59).Unix()); n != 3 {
		t.Errorf("due 1h = %d, want 3", n)
	}
	if n := paperFundingDue(at(9, 0), 8, at(8, 0).Unix()); n != 0 {
		t.Errorf("due right after a settlement = %d, want 0", n)
	}
	if n := paperFundingDue(at(9, 0), 8, at(12, 0).Unix()); n != 0 {
		t.Errorf("marker in the future must not go negative, got %d", n)
	}
}

func TestPaperFundingCanLiquidate(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	clk.Set(time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 100); err != nil { // 1% margin
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.01) // absurd 1% funding wipes out the margin buffer
	clk.Set(time.Date(2026, 10, 5, 8, 0, 1, 0, time.UTC))
	p.Tick()
	if len(mustPositions(t, p)) != 0 {
		t.Fatal("funding drained the margin, the position must be liquidated")
	}
}

func TestPaperGetOrderStatus(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	open, err := p.OpenLong("BTCUSDT", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := open["orderId"].(string)
	if !ok || id == "" {
		t.Fatalf("orderId must be a non-empty string, got %#v", open["orderId"])
	}
	st, err := p.GetOrderStatus("BTCUSDT", id)
	if err != nil {
		t.Fatal(err)
	}
	if st["status"] != "FILLED" {
		t.Fatalf("status %v", st["status"])
	}
	near(t, "avgPrice", mustFloat(t, st, "avgPrice"), open["avgPrice"].(float64))
	near(t, "executedQty", mustFloat(t, st, "executedQty"), 2)
	near(t, "commission", mustFloat(t, st, "commission"), open["commission"].(float64))

	cl, _ := p.CloseLong("BTCUSDT", 0)
	st, err = p.GetOrderStatus("BTCUSDT", cl["orderId"].(string))
	if err != nil || st["status"] != "FILLED" {
		t.Fatalf("close status: %v %v", st, err)
	}
	if id == cl["orderId"] {
		t.Fatal("order ids must be unique")
	}
	if _, err := p.GetOrderStatus("BTCUSDT", "nope"); err == nil {
		t.Fatal("unknown order must error")
	}
}

// TestPaperStateContract pins every key and type AutoTrader reads (buildTradingContext,
// GetAccountInfo, the position fallback paths and the initial-balance probe).
func TestPaperStateContract(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	b := balanceOf(t, p)
	for _, k := range []string{"totalWalletBalance", "availableBalance", "totalUnrealizedProfit", "totalEquity", "total_equity"} {
		mustFloat(t, b, k)
	}
	if mustFloat(t, b, "totalEquity") <= 0 {
		t.Fatal("totalEquity must be > 0 (AutoTrader falls back otherwise)")
	}

	if _, err := p.OpenLong("BTCUSDT", 1, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("NVDAUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	pos := mustPositions(t, p)
	if len(pos) != 2 {
		t.Fatalf("positions = %d", len(pos))
	}
	for _, ps := range pos {
		// unchecked type assertions in auto_trader.go panic when these are wrong
		_ = ps["symbol"].(string)
		side := ps["side"].(string)
		if side != "long" && side != "short" {
			t.Fatalf("side = %q", side)
		}
		for _, k := range []string{"entryPrice", "markPrice", "positionAmt", "unRealizedProfit", "liquidationPrice", "leverage"} {
			mustFloat(t, ps, k)
		}
		if ps["positionAmt"].(float64) <= 0 {
			t.Fatalf("positionAmt must be positive: %v", ps["positionAmt"])
		}
		created, ok := ps["createdTime"].(int64)
		if !ok || created <= 0 {
			t.Fatalf("createdTime must be int64 ms, got %#v", ps["createdTime"])
		}
	}
	// order result keys
	res, _ := p.CloseLong("BTCUSDT", 0)
	if _, ok := res["orderId"].(string); !ok {
		t.Fatalf("orderId type: %#v", res["orderId"])
	}
	for _, k := range []string{"avgPrice", "executedQty", "commission"} {
		mustFloat(t, res, k)
	}
	// FormatQuantity
	if s, err := p.FormatQuantity("BTCUSDT", 0.123456); err != nil || s != "0.1234" {
		t.Fatalf("FormatQuantity = %q, %v", s, err)
	}
	if _, err := p.FormatQuantity("BTCUSDT", 0.00001); err == nil {
		t.Fatal("FormatQuantity below min must fail")
	}
	if px, err := p.GetMarketPrice("btc"); err != nil || px != 100 {
		t.Fatalf("GetMarketPrice = %v, %v", px, err)
	}
}

// TestPaperPriceFeedOutageKeepsReadsWorking: GetBalance/GetPositions fall back to the last
// known mark instead of failing (an error would abort the whole AI cycle).
func TestPaperPriceFeedOutageKeepsReadsWorking(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	src.setMark("BTCUSDT", 120)
	mustPositions(t, p) // records mark 120
	src.mu.Lock()
	src.markErr = fmt.Errorf("offline")
	src.mu.Unlock()
	pos, err := p.GetPositions()
	if err != nil || len(pos) != 1 {
		t.Fatalf("positions during outage: %v %v", pos, err)
	}
	near(t, "last known mark", pos[0]["markPrice"].(float64), 120)
	if _, err := p.GetBalance(); err != nil {
		t.Fatal(err)
	}
	p.Tick() // must not panic or close anything without a price
	if len(mustPositions(t, p)) != 1 {
		t.Fatal("position must survive a price outage")
	}
}

func TestPaperConcurrentAccess(t *testing.T) {
	p, src, _ := newTestPaper(t, 1_000_000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			sym := "BTCUSDT"
			if g%2 == 1 {
				sym = "NVDAUSDT"
			}
			for i := 0; i < 25; i++ {
				src.setMark(sym, 100+float64(i%7))
				_, _ = p.GetBalance()
				_, _ = p.GetPositions()
				_, _ = p.GetOpenOrders("")
				if g < 4 {
					_, _ = p.OpenLong(sym, 1, 5)
					_ = p.SetStopLoss(sym, "LONG", 1, 50)
					p.Tick()
					_, _ = p.CloseLong(sym, 0)
				} else {
					_, _ = p.OpenShort(sym, 1, 5)
					_ = p.SetTakeProfit(sym, "SHORT", 1, 50)
					p.Tick()
					_, _ = p.CloseShort(sym, 0)
				}
				_, _ = p.GetClosedPnL(time.Time{}, 5)
			}
		}(g)
	}
	wg.Wait()
	// everything flat again: no leaked margin
	b := balanceOf(t, p)
	near(t, "no locked margin when flat", mustFloat(t, b, "totalWalletBalance"), mustFloat(t, b, "availableBalance"))
	// Equity is not asserted against the starting balance: the goroutines move the marks while
	// positions are open, so the net result can legitimately be a gain (that made this test flaky).
	if eq := mustFloat(t, b, "totalEquity"); eq <= 0 || math.IsNaN(eq) || math.IsInf(eq, 0) {
		t.Fatalf("equity must stay finite and positive, got %v", eq)
	}
}

// TestPaperBackgroundMatcher exercises the real goroutine: lazy start on the first trade,
// SL fired without calling Tick, and a clean Stop.
func TestPaperBackgroundMatcher(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.autoMatch = true
	p.SetMatchInterval(2 * time.Millisecond)

	p.mu.Lock()
	if p.stopCh != nil {
		t.Fatal("matcher must be idle before the first trade")
	}
	p.mu.Unlock()

	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "LONG", 1, 90); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	end := p.matcherEnd
	started := p.stopCh != nil
	p.mu.Unlock()
	if !started {
		t.Fatal("matcher must start lazily on the first open")
	}

	src.setMark("BTCUSDT", 85)
	deadline := time.Now().Add(3 * time.Second)
	for len(mustPositions(t, p)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background matcher did not trigger the stop loss")
		}
		time.Sleep(time.Millisecond)
	}

	p.Stop()
	select {
	case <-end:
	default:
		t.Fatal("Stop must wait for the goroutine to exit")
	}
	p.Stop() // idempotent
	p.Close()

	// restartable after Stop
	src.setMark("BTCUSDT", 100)
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	restarted := p.stopCh != nil
	p.mu.Unlock()
	if !restarted {
		t.Fatal("matcher must restart on the next trade")
	}
	p.Stop()
}

func TestPaperRegistry(t *testing.T) {
	a := AcquireBitgetPaperTrader("reg-test-1", 5000)
	b := AcquireBitgetPaperTrader("reg-test-1", 999)
	if a != b {
		t.Fatal("same trader id must return the same account")
	}
	near(t, "initial balance kept", mustFloat(t, balanceOfNoT(b), "totalEquity"), 5000)
	if c := AcquireBitgetPaperTrader("reg-test-2", 0); c == a {
		t.Fatal("different ids must not share state")
	} else {
		ReleaseBitgetPaperTrader("reg-test-2")
	}
	ReleaseBitgetPaperTrader("reg-test-1")
	if d := AcquireBitgetPaperTrader("reg-test-1", 7000); d == a {
		t.Fatal("released account must be recreated")
	} else {
		ReleaseBitgetPaperTrader("reg-test-1")
	}
	ReleaseBitgetPaperTrader("never-registered") // no-op
}

func TestPaperClosedPnLQuery(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	for i := 0; i < 3; i++ {
		clk.Set(paperMonday.Add(time.Duration(i) * time.Hour))
		src.setMark("BTCUSDT", 100+float64(i))
		if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := p.CloseLong("BTCUSDT", 0); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := p.GetClosedPnL(time.Time{}, 100)
	if len(all) != 3 || !all[0].ExitTime.After(all[2].ExitTime) {
		t.Fatalf("expected 3 records newest first, got %+v", all)
	}
	since, _ := p.GetClosedPnL(paperMonday.Add(90*time.Minute), 100)
	if len(since) != 1 {
		t.Fatalf("startTime filter: %d", len(since))
	}
	limited, _ := p.GetClosedPnL(time.Time{}, 2)
	if len(limited) != 2 {
		t.Fatalf("limit: %d", len(limited))
	}
}

// TestPaperAutoTraderRecordingFlow drives the AutoTrader bookkeeping with the paper trader:
// recordAndConfirmOrder must persist AI orders (bitget_paper is not an OrderSync exchange) and
// matcher-triggered closes must reach the store through recordPaperSystemFill.
func TestPaperAutoTraderRecordingFlow(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "paper.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, src, _ := newTestPaper(t, 10000)
	at := &AutoTrader{
		id:                    "paper-trader-1",
		name:                  "paper",
		exchange:              "bitget_paper",
		exchangeID:            "paper-exchange-1",
		store:                 st,
		trader:                p,
		positionFirstSeenTime: map[string]int64{},
	}
	p.SetSystemFillHandler(at.recordPaperSystemFill)

	order, err := p.OpenLong("BTCUSDT", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	// open via the AI path: recorded immediately, then confirmed through GetOrderStatus
	at.recordAndConfirmOrder(order, "BTCUSDT", "open_long", 2, 100, 10, 0)

	openPos, err := st.Position().GetOpenPositionBySymbol(at.id, "BTCUSDT", "LONG")
	if err != nil || openPos == nil {
		t.Fatalf("open position not recorded: %v %v", openPos, err)
	}
	near(t, "recorded entry = actual fill price", openPos.EntryPrice, 100*(1+slip))
	orders, err := st.Order().GetTraderOrders(at.id, 10)
	if err != nil || len(orders) != 1 || orders[0].Status != "FILLED" {
		t.Fatalf("orders after open: %+v %v", orders, err)
	}
	near(t, "recorded commission", orders[0].Commission, order["commission"].(float64))

	// SL fires in the matcher, nothing calls recordAndConfirmOrder
	if err := p.SetStopLoss("BTCUSDT", "LONG", 2, 90); err != nil {
		t.Fatal(err)
	}
	src.setMark("BTCUSDT", 89)
	p.Tick()

	if pos, _ := st.Position().GetOpenPositionBySymbol(at.id, "BTCUSDT", "LONG"); pos != nil {
		t.Fatalf("position must be closed in the store after the SL: %+v", pos)
	}
	closed, err := st.Position().GetClosedPositions(at.id, 10)
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed positions: %v %v", closed, err)
	}
	if closed[0].RealizedPnL >= 0 {
		t.Fatalf("SL close must record a loss, got %v", closed[0].RealizedPnL)
	}
	orders, _ = st.Order().GetTraderOrders(at.id, 10)
	if len(orders) != 2 {
		t.Fatalf("expected open + SL close orders, got %d", len(orders))
	}
}

// TestPaperFactoryPositionModeWiring: AutoTraderConfig.BitgetPositionMode reaches the paper account
// ("one_way" -> one-way, "" / "hedge" -> hedge), and a rebuilt AutoTrader (stop -> start in the API)
// applies a changed setting while the account is flat.
func TestPaperFactoryPositionModeWiring(t *testing.T) {
	build := func(t *testing.T, id, mode string) *BitgetPaperTrader {
		t.Helper()
		at, err := NewAutoTrader(AutoTraderConfig{
			ID: id, Name: id, AIModel: "deepseek", Exchange: "bitget_paper",
			InitialBalance: 5000, BitgetPositionMode: mode, StrategyConfig: &store.StrategyConfig{},
		}, nil, "user-1")
		if err != nil {
			t.Fatalf("NewAutoTrader(bitget_paper, %q): %v", mode, err)
		}
		paper, ok := at.trader.(*BitgetPaperTrader)
		if !ok {
			t.Fatalf("trader is %T, want *BitgetPaperTrader", at.trader)
		}
		return paper
	}
	for _, tc := range []struct{ name, mode, want string }{
		{"one_way", "one_way", BitgetPositionModeOneWay},
		{"empty is hedge", "", BitgetPositionModeHedge},
		{"hedge", "hedge", BitgetPositionModeHedge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "factory-mode-" + strings.ReplaceAll(tc.name, " ", "-")
			defer ReleaseBitgetPaperTrader(id)
			if got := build(t, id, tc.mode).PositionMode(); got != tc.want {
				t.Fatalf("paper position mode = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("rebuilt trader applies the changed setting while flat", func(t *testing.T) {
		const id = "factory-mode-rebuild"
		defer ReleaseBitgetPaperTrader(id)
		if got := build(t, id, "one_way").PositionMode(); got != BitgetPositionModeOneWay {
			t.Fatalf("first build: %q", got)
		}
		if got := build(t, id, "hedge").PositionMode(); got != BitgetPositionModeHedge {
			t.Fatalf("rebuild with hedge: %q", got)
		}
	})
}

// TestPaperFactoryWiring: the AutoTrader factory builds the paper trader without keys, derives
// the initial balance from it, keeps one account per trader ID across rebuilds (the API rebuilds
// the AutoTrader on every start/update) and stops the matcher in Stop().
func TestPaperFactoryWiring(t *testing.T) {
	const id = "factory-wiring-test"
	defer ReleaseBitgetPaperTrader(id)

	cfg := AutoTraderConfig{
		ID:             id,
		Name:           "paper factory",
		AIModel:        "deepseek",
		Exchange:       "bitget_paper",
		InitialBalance: 0, // not set: must be probed from the trader (default 10000)
		StrategyConfig: &store.StrategyConfig{},
	}
	at, err := NewAutoTrader(cfg, nil, "user-1")
	if err != nil {
		t.Fatalf("NewAutoTrader(bitget_paper): %v", err)
	}
	paper, ok := at.trader.(*BitgetPaperTrader)
	if !ok {
		t.Fatalf("trader is %T, want *BitgetPaperTrader", at.trader)
	}
	if at.initialBalance != 10000 {
		t.Fatalf("initial balance probe = %v, want 10000", at.initialBalance)
	}
	if got := at.marketSource(); got != "bitget" {
		t.Fatalf("market source = %q", got)
	}
	if at.UnderlyingTrader() != Trader(paper) {
		t.Fatal("UnderlyingTrader must expose the paper trader")
	}

	// rebuilding the AutoTrader (stop -> start in the API) keeps the account
	again, err := NewAutoTrader(cfg, nil, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.trader != at.trader {
		t.Fatal("rebuilt AutoTrader must reuse the paper account of the same trader ID")
	}

	// AutoTrader.Stop stops the matcher
	paper.autoMatch = true
	paper.Start()
	paper.mu.Lock()
	running := paper.stopCh != nil
	paper.mu.Unlock()
	if !running {
		t.Fatal("matcher should be running")
	}
	at.isRunning = true
	at.Stop()
	paper.mu.Lock()
	running = paper.stopCh != nil
	paper.mu.Unlock()
	if running {
		t.Fatal("AutoTrader.Stop must stop the paper matcher")
	}
}
