package trader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Hedge (dual-side) mode of the bitget_paper account: simultaneous long + short per symbol,
// independent SL/TP, liquidation and funding per position, mode switching rules, persistence and
// the v2 -> v3 snapshot migration.

func sidesOf(t *testing.T, p *BitgetPaperTrader, symbol string) map[string]map[string]interface{} {
	t.Helper()
	pos, err := p.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]interface{}{}
	for _, x := range pos {
		if x["symbol"] == symbol {
			out[x["side"].(string)] = x
		}
	}
	return out
}

func TestPaperHedgeIsTheDefault(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	if p.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("default mode = %q, want hedge (parity with the live trader)", p.PositionMode())
	}
	if NewBitgetPaperTrader(0).PositionMode() != BitgetPositionModeHedge {
		t.Fatal("NewBitgetPaperTrader must default to hedge")
	}
}

func TestPaperHedgeSimultaneousLongAndShort(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.SetSlippageBps(0)

	long, err := p.OpenLong("BTCUSDT", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	short, err := p.OpenShort("BTCUSDT", 4, 5)
	if err != nil {
		t.Fatalf("hedge mode must allow the opposite side: %v", err)
	}
	if long["orderId"] == short["orderId"] {
		t.Fatal("distinct orders expected")
	}

	sides := sidesOf(t, p, "BTCUSDT")
	if len(sides) != 2 {
		t.Fatalf("expected a long and a short, got %v", sides)
	}
	near(t, "long qty", sides["long"]["positionAmt"].(float64), 10)
	near(t, "short qty", sides["short"]["positionAmt"].(float64), 4)
	near(t, "long leverage", sides["long"]["leverage"].(float64), 10)
	near(t, "short leverage", sides["short"]["leverage"].(float64), 5)

	// positions are listed deterministically: long before short
	all, _ := p.GetPositions()
	if all[0]["side"] != "long" || all[1]["side"] != "short" {
		t.Fatalf("order = %v, %v", all[0]["side"], all[1]["side"])
	}

	// both legs reserve their own margin (100/10 + 100*4/5) plus fees
	b := balanceOf(t, p)
	near(t, "available", mustFloat(t, b, "availableBalance"), 10000-100-80-1000*fee-400*fee)

	// adding to the long averages only the long
	src.setMark("BTCUSDT", 110)
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	sides = sidesOf(t, p, "BTCUSDT")
	near(t, "long entry averaged", sides["long"]["entryPrice"].(float64), 105)
	near(t, "short entry untouched", sides["short"]["entryPrice"].(float64), 100)
	near(t, "short qty untouched", sides["short"]["positionAmt"].(float64), 4)

	// closing the long keeps the short
	cl, err := p.CloseLong("BTCUSDT", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cl["side"] != "SELL" {
		t.Fatalf("close long side = %v", cl["side"])
	}
	sides = sidesOf(t, p, "BTCUSDT")
	if len(sides) != 1 || sides["short"] == nil {
		t.Fatalf("only the short must remain: %v", sides)
	}
	cs, err := p.CloseShort("BTCUSDT", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cs["side"] != "BUY" {
		t.Fatalf("close short side = %v", cs["side"])
	}
	if pos, _ := p.GetPositions(); len(pos) != 0 {
		t.Fatalf("flat expected: %v", pos)
	}

	recs, _ := p.GetClosedPnL(paperMonday.Add(-time.Hour), 10)
	if len(recs) != 2 || recs[0].Side != "short" || recs[1].Side != "long" {
		t.Fatalf("closed records (newest first) = %+v", recs)
	}
}

func TestPaperHedgeCloseWrongSideStillFails(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CloseShort("BTCUSDT", 0); err == nil {
		t.Fatal("closing a short that does not exist must fail even in hedge mode")
	}
	if _, err := p.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
}

func TestPaperHedgeIndependentSLTP(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.SetSlippageBps(0)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })

	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	// long: SL 95 / TP 120; short: SL 105 / TP 80 (each protects its own leg)
	for _, c := range []struct {
		set  func(string, string, float64, float64) error
		side string
		px   float64
	}{
		{p.SetStopLoss, "LONG", 95}, {p.SetTakeProfit, "LONG", 120},
		{p.SetStopLoss, "SHORT", 105}, {p.SetTakeProfit, "SHORT", 80},
	} {
		if err := c.set("BTCUSDT", c.side, 1, c.px); err != nil {
			t.Fatalf("%s %v: %v", c.side, c.px, err)
		}
	}

	orders, _ := p.GetOpenOrders("BTCUSDT")
	if len(orders) != 4 {
		t.Fatalf("expected 4 plan orders, got %+v", orders)
	}
	for _, o := range orders {
		wantSide := map[string]string{"LONG": "SELL", "SHORT": "BUY"}[o.PositionSide]
		if o.Side != wantSide {
			t.Errorf("%+v: closing side must be %s", o, wantSide)
		}
	}

	// replacing the long's SL leaves the short's SL alone
	if err := p.SetStopLoss("BTCUSDT", "LONG", 1, 96); err != nil {
		t.Fatal(err)
	}
	orders, _ = p.GetOpenOrders("BTCUSDT")
	if len(orders) != 4 {
		t.Fatalf("upsert must not add orders: %+v", orders)
	}
	for _, o := range orders {
		if o.Type == "STOP_MARKET" && o.PositionSide == "SHORT" && o.StopPrice != 105 {
			t.Errorf("short SL changed by a long SL upsert: %+v", o)
		}
		if o.Type == "STOP_MARKET" && o.PositionSide == "LONG" && o.StopPrice != 96 {
			t.Errorf("long SL not replaced: %+v", o)
		}
	}

	// price drops to 95: only the long's stop triggers, the short keeps running
	src.setMark("BTCUSDT", 95)
	p.Tick()
	if len(got) != 1 || got[0].Reason != paperReasonStopLoss || got[0].PositionSide != "LONG" || got[0].Action != "close_long" {
		t.Fatalf("expected the long SL only, got %+v", got)
	}
	sides := sidesOf(t, p, "BTCUSDT")
	if len(sides) != 1 || sides["short"] == nil {
		t.Fatalf("short must survive the long's stop: %v", sides)
	}
	orders, _ = p.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 {
		t.Fatalf("only the short's SL/TP remain: %+v", orders)
	}
	for _, o := range orders {
		if o.PositionSide != "SHORT" {
			t.Errorf("leftover plan order of the closed long: %+v", o)
		}
	}

	// price rallies to 106: the short's stop triggers
	src.setMark("BTCUSDT", 106)
	p.Tick()
	if len(got) != 2 || got[1].PositionSide != "SHORT" || got[1].Reason != paperReasonStopLoss {
		t.Fatalf("expected the short SL second, got %+v", got)
	}
	if pos, _ := p.GetPositions(); len(pos) != 0 {
		t.Fatalf("flat expected: %v", pos)
	}
}

func TestPaperHedgeTakeProfitPerSide(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.SetSlippageBps(0)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })
	_, _ = p.OpenLong("BTCUSDT", 1, 5)
	_, _ = p.OpenShort("BTCUSDT", 1, 5)
	if err := p.SetTakeProfit("BTCUSDT", "LONG", 1, 110); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "SHORT", 1, 90); err != nil {
		t.Fatal(err)
	}
	src.setMark("BTCUSDT", 90)
	p.Tick()
	if len(got) != 1 || got[0].PositionSide != "SHORT" || got[0].Reason != paperReasonTakeProfit {
		t.Fatalf("only the short's TP at 90 expected: %+v", got)
	}
	if sides := sidesOf(t, p, "BTCUSDT"); len(sides) != 1 || sides["long"] == nil {
		t.Fatalf("long must remain: %v", sides)
	}
}

func TestPaperHedgeTriggerNeedsSideWhenBothHeld(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	_, _ = p.OpenLong("BTCUSDT", 1, 5)
	if err := p.SetStopLoss("BTCUSDT", "", 1, 90); err != nil {
		t.Fatalf("a single position needs no side: %v", err)
	}
	_, _ = p.OpenShort("BTCUSDT", 1, 5)
	err := p.SetStopLoss("BTCUSDT", "", 1, 90)
	if err == nil || !strings.Contains(err.Error(), "position side") {
		t.Fatalf("both legs open and no side given must be rejected, got %v", err)
	}
	if err := p.SetStopLoss("BTCUSDT", "short", 1, 90); err == nil {
		t.Fatal("a short SL below the mark must still be rejected")
	}
	if err := p.SetStopLoss("BTCUSDT", "SHORT", 1, 110); err != nil {
		t.Fatalf("explicit side: %v", err)
	}
	_, _ = p.CloseShort("BTCUSDT", 0)
	if err := p.SetStopLoss("BTCUSDT", "SHORT", 1, 110); err == nil {
		t.Fatal("no short position any more")
	}
}

func TestPaperHedgeCancelOnlyTheRequestedSide(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	_, _ = p.OpenLong("BTCUSDT", 1, 5)
	_, _ = p.OpenShort("BTCUSDT", 1, 5)
	set := func() {
		t.Helper()
		for _, c := range []struct {
			f    func(string, string, float64, float64) error
			side string
			px   float64
		}{
			{p.SetStopLoss, "LONG", 90}, {p.SetTakeProfit, "LONG", 120},
			{p.SetStopLoss, "SHORT", 110}, {p.SetTakeProfit, "SHORT", 80},
		} {
			if err := c.f("BTCUSDT", c.side, 1, c.px); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(side, typ string) int {
		orders, _ := p.GetOpenOrders("BTCUSDT")
		n := 0
		for _, o := range orders {
			if (side == "" || o.PositionSide == side) && (typ == "" || o.Type == typ) {
				n++
			}
		}
		return n
	}
	set()
	_ = p.CancelStopLossOrdersForSide("BTCUSDT", "LONG")
	if count("LONG", "STOP_MARKET") != 0 || count("SHORT", "STOP_MARKET") != 1 || count("LONG", "TAKE_PROFIT_MARKET") != 1 {
		t.Fatalf("CancelStopLossOrdersForSide(LONG) must only drop the long SL: %+v", mustOrders(p))
	}
	set()
	_ = p.CancelTakeProfitOrdersForSide("BTCUSDT", "short")
	if count("SHORT", "TAKE_PROFIT_MARKET") != 0 || count("LONG", "TAKE_PROFIT_MARKET") != 1 || count("SHORT", "STOP_MARKET") != 1 {
		t.Fatalf("CancelTakeProfitOrdersForSide(short): %+v", mustOrders(p))
	}
	set()
	_ = p.CancelStopOrdersForSide("BTCUSDT", "LONG")
	if count("LONG", "") != 0 || count("SHORT", "") != 2 {
		t.Fatalf("CancelStopOrdersForSide(LONG): %+v", mustOrders(p))
	}
	set()
	// the side-less interface methods cover the whole symbol (both legs)
	_ = p.CancelStopLossOrders("BTCUSDT")
	if count("", "STOP_MARKET") != 0 || count("", "TAKE_PROFIT_MARKET") != 2 {
		t.Fatalf("CancelStopLossOrders: %+v", mustOrders(p))
	}
	_ = p.CancelStopOrders("BTCUSDT")
	set()
	_ = p.CancelAllOrders("BTCUSDT")
	if count("", "") != 0 {
		t.Fatalf("CancelAllOrders: %+v", mustOrders(p))
	}
}

func mustOrders(p *BitgetPaperTrader) []OpenOrder {
	o, _ := p.GetOpenOrders("")
	return o
}

func TestPaperHedgeFundingIsPerPosition(t *testing.T) {
	p, src, clk := newTestPaper(t, 10000)
	p.SetSlippageBps(0)
	clk.Set(time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC))
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	// the short is opened AFTER the 08:00 boundary: only the long owes this settlement
	clk.Set(time.Date(2026, 10, 5, 8, 0, 5, 0, time.UTC))
	if _, err := p.OpenShort("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	src.setRate("BTCUSDT", 0.001)
	base := mustFloat(t, balanceOf(t, p), "totalEquity")
	clk.Set(time.Date(2026, 10, 5, 8, 0, 10, 0, time.UTC))
	p.Tick()
	// long pays rate * qty * mark = 0.001 * 10 * 100 = 1.0; the short is not due yet
	near(t, "only the long pays", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -1.0)
	p.Tick()
	near(t, "no double settlement", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -1.0)

	// next boundary (16:00): the long pays again, the short RECEIVES the same amount
	clk.Set(time.Date(2026, 10, 5, 16, 0, 5, 0, time.UTC))
	p.Tick()
	near(t, "long pays, short receives: net zero", mustFloat(t, balanceOf(t, p), "totalEquity")-base, -1.0)

	p.mu.Lock()
	long := p.positions[paperPosKey("BTCUSDT", "long")]
	short := p.positions[paperPosKey("BTCUSDT", "short")]
	lf, sf := long.funding, short.funding
	p.mu.Unlock()
	near(t, "long funding", lf, -2.0)
	near(t, "short funding", sf, 1.0)
}

func TestPaperHedgeLiquidationIsPerPosition(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.SetSlippageBps(0)
	var got []PaperFill
	p.SetSystemFillHandler(func(f PaperFill) { got = append(got, f) })
	// same symbol: a 2x long (far from liquidation) and a 50x short (margin 2% of notional)
	if _, err := p.OpenLong("BTCUSDT", 10, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 10, 50); err != nil {
		t.Fatal(err)
	}
	sides := sidesOf(t, p, "BTCUSDT")
	if liq := sides["short"]["liquidationPrice"].(float64); liq <= 100 || liq >= 102 {
		t.Fatalf("50x short liquidation price should be just above entry, got %v", liq)
	}
	if liq := sides["long"]["liquidationPrice"].(float64); liq < 49 || liq > 52 {
		t.Fatalf("2x long liquidation price should be ~50, got %v", liq)
	}
	src.setMark("BTCUSDT", 102)
	p.Tick()
	if len(got) != 1 || got[0].Reason != paperReasonLiquidation || got[0].PositionSide != "SHORT" {
		t.Fatalf("only the short must be liquidated: %+v", got)
	}
	if s := sidesOf(t, p, "BTCUSDT"); len(s) != 1 || s["long"] == nil {
		t.Fatalf("the long must survive: %v", s)
	}
}

func TestPaperHedgeBalanceCountsBothLegs(t *testing.T) {
	p, src, _ := newTestPaper(t, 10000)
	p.SetSlippageBps(0)
	_, _ = p.OpenLong("BTCUSDT", 10, 10)
	_, _ = p.OpenShort("BTCUSDT", 6, 10)
	src.setMark("BTCUSDT", 110)
	b := balanceOf(t, p)
	// long +100, short -60
	near(t, "unrealized", mustFloat(t, b, "totalUnrealizedProfit"), 40)
	near(t, "equity", mustFloat(t, b, "totalEquity"), 10000-1000*fee-600*fee+40)
}

func TestPaperHedgeFillsKeepPositionSide(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	ol, _ := p.OpenLong("BTCUSDT", 1, 5)
	os, _ := p.OpenShort("BTCUSDT", 1, 5)
	cl, _ := p.CloseLong("BTCUSDT", 0)
	cs, _ := p.CloseShort("BTCUSDT", 0)
	for _, c := range []struct {
		res          map[string]interface{}
		action, side string
		posSide      string
	}{
		{ol, "open_long", "BUY", "LONG"},
		{os, "open_short", "SELL", "SHORT"},
		{cl, "close_long", "SELL", "LONG"},
		{cs, "close_short", "BUY", "SHORT"},
	} {
		st, err := p.GetOrderStatus("BTCUSDT", c.res["orderId"].(string))
		if err != nil || st["side"] != c.side {
			t.Errorf("%s: status %v err %v, want side %s", c.action, st, err, c.side)
		}
		p.mu.Lock()
		f := p.fillIndex[c.res["orderId"].(string)]
		p.mu.Unlock()
		if f.Action != c.action || f.PositionSide != c.posSide {
			t.Errorf("fill %+v, want %s / %s", f, c.action, c.posSide)
		}
	}
}

// ------------------------------------------------------------------ one-way

func TestPaperOneWayModeKeepsOldBehaviour(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	p.SetPositionMode("one_way")
	if p.PositionMode() != BitgetPositionModeOneWay {
		t.Fatalf("mode = %q", p.PositionMode())
	}
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err == nil || !strings.Contains(err.Error(), "one-way") {
		t.Fatalf("one-way must reject the opposite side, got %v", err)
	}
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("adding to the same side is fine: %v", err)
	}
	if _, err := p.OpenShort("NVDAUSDT", 1, 5); err != nil {
		t.Fatalf("a different symbol is independent: %v", err)
	}
}

func TestPaperPositionModeSwitchOnlyWhenFlat(t *testing.T) {
	p, _, _ := newTestPaper(t, 10000)
	p.SetPositionMode("one_way") // flat: switches
	if p.PositionMode() != BitgetPositionModeOneWay {
		t.Fatal("flat account must switch")
	}
	p.SetPositionMode("") // empty = hedge (default)
	if p.PositionMode() != BitgetPositionModeHedge {
		t.Fatal("empty setting means hedge")
	}
	_, _ = p.OpenLong("BTCUSDT", 1, 5)
	p.SetPositionMode("one_way") // positions open: refused, stays hedge
	if p.PositionMode() != BitgetPositionModeHedge {
		t.Fatal("must not switch while positions are open")
	}
	if _, err := p.OpenShort("BTCUSDT", 1, 5); err != nil {
		t.Fatalf("still hedge: %v", err)
	}
	_, _ = p.CloseLong("BTCUSDT", 0)
	_, _ = p.CloseShort("BTCUSDT", 0)
	p.SetPositionMode("one_way")
	if p.PositionMode() != BitgetPositionModeOneWay {
		t.Fatal("flat again: switch allowed")
	}
	p.SetPositionMode("garbage") // unknown values fall back to the default
	if p.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("unknown value must mean hedge, got %q", p.PositionMode())
	}
}

// ------------------------------------------------------------------ persistence

func TestPaperHedgeSnapshotRoundTrip(t *testing.T) {
	dir := usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: time.Date(2026, 10, 5, 7, 59, 0, 0, time.UTC)}
	p := acquirePaper("t-hedge", 10000, src, clk)
	p.SetPositionMode("hedge")
	if _, err := p.OpenLong("BTCUSDT", 10, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenShort("BTCUSDT", 4, 5); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStopLoss("BTCUSDT", "LONG", 10, 95); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTakeProfit("BTCUSDT", "SHORT", 4, 80); err != nil {
		t.Fatal(err)
	}
	want := canonicalSnapshot(t, p)
	wantOrders, _ := p.GetOpenOrders("BTCUSDT")
	wantEquity := mustFloat(t, balanceOf(t, p), "totalEquity")

	raw, err := os.ReadFile(filepath.Join(dir, "t-hedge.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version      int    `json:"version"`
		PositionMode string `json:"position_mode"`
		Positions    []struct {
			Symbol, Side string
		} `json:"positions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 3 || doc.PositionMode != "hedge" || len(doc.Positions) != 2 {
		t.Fatalf("snapshot must be v3 with the mode and both legs: %s", raw)
	}

	resetPaperRegistryForTest() // process restart
	p2 := acquirePaper("t-hedge", 777, src, clk)
	if p2 == p {
		t.Fatal("a new instance is expected after the registry reset")
	}
	if p2.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("mode lost: %q", p2.PositionMode())
	}
	if got := canonicalSnapshot(t, p2); got != want {
		t.Fatalf("snapshot differs after the round trip:\n got %s\nwant %s", got, want)
	}
	sides := sidesOf(t, p2, "BTCUSDT")
	if len(sides) != 2 {
		t.Fatalf("both legs must be restored: %v", sides)
	}
	gotOrders, _ := p2.GetOpenOrders("BTCUSDT")
	if len(gotOrders) != len(wantOrders) {
		t.Fatalf("SL/TP lost: %+v vs %+v", gotOrders, wantOrders)
	}
	near(t, "equity", mustFloat(t, balanceOf(t, p2), "totalEquity"), wantEquity)
	// the restored hedge account still closes each leg independently
	if _, err := p2.CloseShort("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	if s := sidesOf(t, p2, "BTCUSDT"); len(s) != 1 || s["long"] == nil {
		t.Fatalf("after closing the short: %v", s)
	}
}

func TestPaperRestoredModeSurvivesConfigWhileOpenAndAppliesWhenFlat(t *testing.T) {
	usePaperStateDir(t)
	src := newFakePaperSource()
	clk := &fakeClock{t: paperMonday}
	p := acquirePaper("t-mode", 10000, src, clk)
	p.SetPositionMode("one_way")
	if _, err := p.OpenLong("BTCUSDT", 1, 5); err != nil {
		t.Fatal(err)
	}
	resetPaperRegistryForTest()

	// restart with the default config (hedge): the open one-way account keeps its mode
	p2 := acquirePaper("t-mode", 10000, src, clk)
	p2.SetPositionMode("hedge")
	if p2.PositionMode() != BitgetPositionModeOneWay {
		t.Fatalf("an account with open positions keeps its saved mode, got %q", p2.PositionMode())
	}
	if _, err := p2.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	resetPaperRegistryForTest()

	// flat now: the configured mode applies on the next load and is persisted
	p3 := acquirePaper("t-mode", 10000, src, clk)
	p3.SetPositionMode("hedge")
	if p3.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("flat account must take the configured mode, got %q", p3.PositionMode())
	}
	resetPaperRegistryForTest()
	p4 := acquirePaper("t-mode", 10000, src, clk)
	if p4.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("the switched mode must be persisted, got %q", p4.PositionMode())
	}
}

// ------------------------------------------------------------------ migration / validation

func writeSnapshotJSON(t *testing.T, dir, id string, doc map[string]interface{}) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func v2Snapshot(id string, positions []map[string]interface{}) map[string]interface{} {
	saved := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	return map[string]interface{}{
		"version": 2, "trader_id": id, "saved_at": saved, "initial_balance": 10000.0, "cash": 9900.0,
		"realized_pnl": 0.0, "next_order_seq": 4, "last_funding_at": saved.Unix(),
		"positions": positions, "leverage": map[string]int{"BTCUSDT": 10}, "cross_mode": map[string]bool{},
		"fills": []interface{}{}, "closed_trades": []interface{}{},
	}
}

func v2Position(symbol, side string) map[string]interface{} {
	return map[string]interface{}{
		"id": "paperpos-" + symbol + side, "symbol": symbol, "side": side, "qty": 10.0, "entry_price": 100.0,
		"leverage": 10, "margin": 100.0, "open_fee": 0.6, "funding": 0.0,
		"opened_at": time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), "funding_boundary": time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC).Unix(),
	}
}

// Accounts written before hedge mode were one-way: a non-empty v2 snapshot must keep behaving
// like one-way (opposite side rejected) even though the default is now hedge; an empty one has
// nothing to preserve and takes the default.
func TestPaperSnapshotV2MigratesToOneWayUnlessEmpty(t *testing.T) {
	t.Run("with positions stays one-way", func(t *testing.T) {
		dir := usePaperStateDir(t)
		writeSnapshotJSON(t, dir, "t-v2", v2Snapshot("t-v2", []map[string]interface{}{v2Position("BTCUSDT", "long")}))
		src := newFakePaperSource()
		clk := &fakeClock{t: paperMonday}
		p := acquirePaper("t-v2", 1, src, clk)
		if p.PositionMode() != BitgetPositionModeOneWay {
			t.Fatalf("v2 account with positions must migrate as one_way, got %q", p.PositionMode())
		}
		p.SetPositionMode("hedge") // the default config must not flip an account that holds positions
		if p.PositionMode() != BitgetPositionModeOneWay {
			t.Fatal("must stay one-way while positions are open")
		}
		if _, err := p.OpenShort("BTCUSDT", 1, 5); err == nil || !strings.Contains(err.Error(), "one-way") {
			t.Fatalf("opposite side must be rejected like before, got %v", err)
		}
		if len(sidesOf(t, p, "BTCUSDT")) != 1 {
			t.Fatal("position must be restored")
		}
		// the next write is a v3 snapshot carrying the mode
		raw, _ := os.ReadFile(filepath.Join(dir, "t-v2.json"))
		var doc struct {
			Version      int    `json:"version"`
			PositionMode string `json:"position_mode"`
		}
		_ = json.Unmarshal(raw, &doc)
		if doc.Version != 3 {
			// the migration itself does not rewrite; the first mutation does
			if _, err := p.OpenLong("NVDAUSDT", 1, 5); err != nil {
				t.Fatal(err)
			}
			raw, _ = os.ReadFile(filepath.Join(dir, "t-v2.json"))
			_ = json.Unmarshal(raw, &doc)
		}
		if doc.Version != 3 || doc.PositionMode != "one_way" {
			t.Fatalf("rewritten snapshot: %s", raw)
		}
	})

	t.Run("empty account takes the default", func(t *testing.T) {
		dir := usePaperStateDir(t)
		writeSnapshotJSON(t, dir, "t-v2e", v2Snapshot("t-v2e", []map[string]interface{}{}))
		p := acquirePaper("t-v2e", 1, newFakePaperSource(), &fakeClock{t: paperMonday})
		if p.PositionMode() != BitgetPositionModeHedge {
			t.Fatalf("empty v2 account must take hedge, got %q", p.PositionMode())
		}
		near(t, "cash restored", p.cash, 9900)
	})

	t.Run("empty account follows a one_way config", func(t *testing.T) {
		dir := usePaperStateDir(t)
		writeSnapshotJSON(t, dir, "t-v2o", v2Snapshot("t-v2o", nil))
		p := acquirePaper("t-v2o", 1, newFakePaperSource(), &fakeClock{t: paperMonday})
		p.SetPositionMode("one_way")
		if p.PositionMode() != BitgetPositionModeOneWay {
			t.Fatalf("got %q", p.PositionMode())
		}
	})
}

func TestPaperSnapshotValidationOfHedgePositions(t *testing.T) {
	base := func(mode string, positions ...map[string]interface{}) map[string]interface{} {
		d := v2Snapshot("t-val", positions)
		d["version"] = 3
		d["position_mode"] = mode
		return d
	}
	cases := []struct {
		name   string
		doc    map[string]interface{}
		loaded bool // true = the snapshot is accepted (account restored)
	}{
		{"hedge with both legs", base("hedge", v2Position("BTCUSDT", "long"), v2Position("BTCUSDT", "short")), true},
		{"hedge with a duplicate leg", base("hedge", v2Position("BTCUSDT", "long"), v2Position("BTCUSDT", "long")), false},
		{"one_way with both legs of a symbol", base("one_way", v2Position("BTCUSDT", "long"), v2Position("BTCUSDT", "short")), false},
		{"one_way with different symbols", base("one_way", v2Position("BTCUSDT", "long"), v2Position("NVDAUSDT", "short")), true},
		{"v2 snapshots never had both legs", func() map[string]interface{} {
			d := v2Snapshot("t-val", []map[string]interface{}{v2Position("BTCUSDT", "long"), v2Position("BTCUSDT", "short")})
			return d
		}(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := usePaperStateDir(t)
			writeSnapshotJSON(t, dir, "t-val", c.doc)
			p := acquirePaper("t-val", 5000, newFakePaperSource(), &fakeClock{t: paperMonday})
			restored := p.cash == 9900
			if restored != c.loaded {
				t.Fatalf("restored=%v want %v (cash %v)", restored, c.loaded, p.cash)
			}
			if !c.loaded {
				if _, err := os.Stat(filepath.Join(dir, "t-val.json"+paperRejectedSuffix)); err != nil {
					t.Fatalf("rejected snapshot must be kept aside: %v", err)
				}
			}
		})
	}
}

func TestPaperSnapshotUnknownModeFallsBackToHedge(t *testing.T) {
	dir := usePaperStateDir(t)
	d := v2Snapshot("t-unk", nil)
	d["version"] = 3
	d["position_mode"] = "weird"
	writeSnapshotJSON(t, dir, "t-unk", d)
	p := acquirePaper("t-unk", 1, newFakePaperSource(), &fakeClock{t: paperMonday})
	if p.PositionMode() != BitgetPositionModeHedge || p.cash != 9900 {
		t.Fatalf("mode %q cash %v", p.PositionMode(), p.cash)
	}
}
