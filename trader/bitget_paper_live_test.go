package trader

import (
	"testing"
	"time"

	"nofx/internal/testutil"
)

// TestPaperLiveSmoke trades a tiny BTCUSDT long against the REAL Bitget public API
// (no keys, no real orders). Skipped unless NOFX_LIVE_TESTS=1.
func TestPaperLiveSmoke(t *testing.T) {
	testutil.RequireLive(t)
	p := NewBitgetPaperTrader(1000)
	defer p.Stop()

	price, err := p.GetMarketPrice("BTCUSDT")
	if err != nil {
		t.Skipf("Bitget public API unreachable: %v", err)
	}
	qty := 10 / price // ~10 USDT notional, floored to the contract step by the trader
	t.Logf("BTCUSDT mark = %.2f, requested qty = %.8f", price, qty)

	order, err := p.OpenLong("BTCUSDT", qty, 5)
	if err != nil {
		t.Fatalf("open long: %v", err)
	}
	t.Logf("open:  %v", order)

	positions, err := p.GetPositions()
	if err != nil || len(positions) != 1 {
		t.Fatalf("positions: %v %v", positions, err)
	}
	t.Logf("position: %v", positions[0])

	sl := price * 0.95
	tp := price * 1.05
	if err := p.SetStopLoss("BTCUSDT", "LONG", qty, sl); err != nil {
		t.Fatalf("stop loss: %v", err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "LONG", qty, tp); err != nil {
		t.Fatalf("take profit: %v", err)
	}
	orders, _ := p.GetOpenOrders("BTCUSDT")
	t.Logf("open orders: %+v", orders)

	bal, _ := p.GetBalance()
	t.Logf("balance with open position: %v", bal)

	// one real matcher pass against live prices (nothing should trigger)
	p.Tick()
	if left, _ := p.GetPositions(); len(left) != 1 {
		t.Fatalf("position unexpectedly closed by a live tick: %v", left)
	}

	closeRes, err := p.CloseLong("BTCUSDT", 0)
	if err != nil {
		t.Fatalf("close long: %v", err)
	}
	t.Logf("close: %v", closeRes)
	if st, err := p.GetOrderStatus("BTCUSDT", closeRes["orderId"].(string)); err != nil || st["status"] != "FILLED" {
		t.Fatalf("order status: %v %v", st, err)
	}

	recs, _ := p.GetClosedPnL(time.Now().Add(-time.Minute), 10)
	t.Logf("closed pnl: %+v", recs)
	bal, _ = p.GetBalance()
	t.Logf("FINAL balance: %v", bal)
	if eq, _ := bal["totalEquity"].(float64); eq >= 1000 || eq < 990 {
		t.Fatalf("expected a few cents of fee/slippage cost on 1000 USDT, got equity %v", eq)
	}
}
