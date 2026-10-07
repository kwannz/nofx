package trader

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// Hedge-mode tests driven by REAL responses captured on the Bitget Demo environment
// (trader/testdata/bitget/hedge_*.json, account_detail_*.json, *_err_*.json): the account was
// switched to hedge_mode, a long and a short BTCUSDT 0.0001 were opened at the same time, TP/SL
// was placed on both sides, then each side was closed on its own.

func TestBitgetHedgeFixtureOpenCloseBothSides(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetContractsPath, `[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","minTradeNum":"0.0001","priceEndStep":"1","volumePlace":"4","pricePlace":"1","sizeMultiplier":"0.0001","minTradeUSDT":"5","maxLever":"125","symbolStatus":"normal","isRwa":"NO","takerFeeRate":"0.0006","makerFeeRate":"0.0002","fundInterval":"8"}]`)
	f.set(bitgetTickerPath, `[{"symbol":"BTCUSDT","lastPr":"83479.4"}]`)
	f.setFixture(bitgetLeveragePath, "hedge_set_leverage")
	f.setFixture(bitgetOrderDetailPath, "hedge_order_detail_close_long")

	steps := []struct {
		name, fixture, orderID string
		call                   func() (map[string]interface{}, error)
		side, tradeSide        string
	}{
		{"open long", "hedge_place_open_long", "1491774559689830401", func() (map[string]interface{}, error) { return tr.OpenLong("BTCUSDT", 0.0001, 3) }, "buy", "open"},
		{"open short", "hedge_place_open_short", "1491774564899155969", func() (map[string]interface{}, error) { return tr.OpenShort("BTCUSDT", 0.0001, 3) }, "sell", "open"},
		{"close long", "hedge_place_close_long", "1491774628346392577", func() (map[string]interface{}, error) { return tr.CloseLong("BTCUSDT", 0.0001) }, "buy", "close"},
		{"close short", "hedge_place_close_short", "1491774645425598465", func() (map[string]interface{}, error) { return tr.CloseShort("BTCUSDT", 0.0001) }, "sell", "close"},
	}
	for _, s := range steps {
		f.setFixture(bitgetOrderPath, s.fixture)
		res, err := s.call()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if res["orderId"] != s.orderID {
			t.Errorf("%s: orderId = %v, want %s", s.name, res["orderId"], s.orderID)
		}
		b := f.last(bitgetOrderPath).Body
		if b["side"] != s.side || b["tradeSide"] != s.tradeSide || b["size"] != "0.0001" || b["orderType"] != "market" || b["marginMode"] != "crossed" {
			t.Errorf("%s: body = %v", s.name, b)
		}
		if _, has := b["reduceOnly"]; has {
			t.Errorf("%s: reduceOnly must not be sent in hedge mode (Bitget ignores it: a reduceOnly open order still opened a position): %v", s.name, b)
		}
	}
	// the order detail of the real hedge close: filled, fee negative -> positive commission
	st, err := tr.GetOrderStatus("BTCUSDT", "1491774628346392577")
	if err != nil {
		t.Fatal(err)
	}
	if st["status"] != "FILLED" || st["avgPrice"].(float64) != 83473.1 || st["executedQty"].(float64) != 0.0001 || !approx(st["commission"].(float64), 0.00500838) {
		t.Errorf("order status = %v", st)
	}
}

func TestBitgetHedgeFixtureGetPositionsBothSides(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.setFixture(bitgetPositionPath, "hedge_positions")
	pos, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 {
		t.Fatalf("a long and a short of the same symbol expected: %v", pos)
	}
	by := map[string]map[string]interface{}{}
	for _, p := range pos {
		by[p["side"].(string)] = p
	}
	l, s := by["long"], by["short"]
	if l == nil || s == nil || l["symbol"] != "BTCUSDT" || s["symbol"] != "BTCUSDT" {
		t.Fatalf("positions = %v", pos)
	}
	if l["positionAmt"].(float64) != 0.0001 || l["entryPrice"].(float64) != 83471.2 || l["leverage"].(float64) != 3 ||
		s["positionAmt"].(float64) != 0.0001 || s["entryPrice"].(float64) != 83464.4 || s["leverage"].(float64) != 3 {
		t.Errorf("long %v / short %v", l, s)
	}
	// the real hedge legs both report liquidationPrice 60549679693.56 (~725000 x mark, offsetting
	// legs on a cross account): not applicable, must never reach the prompt as a number
	if l["liquidationPrice"].(float64) != 0 || s["liquidationPrice"].(float64) != 0 {
		t.Errorf("absurd hedge liquidation prices must be reported as 0 (n/a): long %v short %v", l["liquidationPrice"], s["liquidationPrice"])
	}

	// the same rows carry TP/SL ids once protection is set
	f.setFixture(bitgetPositionPath, "hedge_positions_with_tpsl")
	tr.clearCache()
	if pos, err = tr.GetPositions(); err != nil || len(pos) != 2 {
		t.Fatalf("with TP/SL: %v %v", pos, err)
	}
}

func TestSanitizeBitgetPositionLiquidation(t *testing.T) {
	for _, c := range []struct {
		name, side string
		mark       float64
		raw        string
		want       float64
	}{
		{"real hedge leg (absurd)", "long", 83479.4, "60549679693.5589689660869565", 0},
		{"real hedge short leg (absurd)", "short", 83479.4, "60549679693.5589689660869565", 0},
		{"real one-way long (negative)", "long", 83344.1, "-279781303.0828121933333334", 0},
		{"long below the mark is plausible", "long", 100, "60.5", 60.5},
		{"short above the mark is plausible", "short", 100, "130.25", 130.25},
		{"long above the mark is impossible", "long", 100, "130", 0},
		{"short below the mark is impossible", "short", 100, "60", 0},
		{"1x short at 2x entry is fine", "short", 100, "199", 199},
		{"no mark price: only the basic sanitising", "long", 0, "130", 130},
		{"empty", "long", 100, "", 0},
	} {
		if got := sanitizeBitgetPositionLiquidation(c.side, c.mark, c.raw); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBitgetHedgeFixturePlanOrders(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.setFixture(bitgetPlanPendingPath, "hedge_plan_pending")
	orders, err := tr.GetOpenOrders("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		pos, side, typ string
		trigger        float64
	}{
		"1491774575234019328": {"LONG", "SELL", "STOP_MARKET", 75131.5},
		"1491774577079500800": {"LONG", "SELL", "TAKE_PROFIT_MARKET", 91827.3},
		"1491774578761416704": {"SHORT", "BUY", "STOP_MARKET", 91827.3},
		"1491774580497870848": {"SHORT", "BUY", "TAKE_PROFIT_MARKET", 75131.5},
	}
	if len(orders) != 4 {
		t.Fatalf("orders = %+v", orders)
	}
	for _, o := range orders {
		w, ok := want[o.OrderID]
		if !ok || o.PositionSide != w.pos || o.Side != w.side || o.Type != w.typ || o.StopPrice != w.trigger || o.Status != "NEW" || o.Symbol != "BTCUSDT" {
			t.Errorf("%+v, want %+v", o, w)
		}
	}

	// after the long was closed only the short's protection is listed; after both, nothing
	f.setFixture(bitgetPlanPendingPath, "hedge_plan_pending_after_close_long")
	orders, _ = tr.GetOpenOrders("BTCUSDT")
	if len(orders) != 2 || orders[0].PositionSide != "SHORT" || orders[1].PositionSide != "SHORT" {
		t.Errorf("only the short's plan orders remain: %+v", orders)
	}
	f.setFixture(bitgetPlanPendingPath, "hedge_plan_pending_after_close_all")
	if orders, err = tr.GetOpenOrders("BTCUSDT"); err != nil || len(orders) != 0 {
		t.Errorf("flat: %+v %v", orders, err)
	}
}

func TestBitgetHedgeFixturePendingLimitOrder(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.setFixture(bitgetPendingPath, "hedge_orders_pending_with_close")
	orders, err := tr.GetOpenOrders("BTCUSDT")
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders = %+v err %v", orders, err)
	}
	// real order: side=buy posSide=long tradeSide=close (a limit order that reduces the long) =>
	// the real order direction is SELL on the LONG leg
	o := orders[0]
	if o.OrderID != "1491774617118240769" || o.Side != "SELL" || o.PositionSide != "LONG" || o.Type != "LIMIT" || o.Price != 166958.8 || o.Quantity != 0.0001 || o.Status != "NEW" {
		t.Errorf("order = %+v", o)
	}
}

func TestBitgetHedgeFixtureCancelPerSide(t *testing.T) {
	const (
		longSL  = "1491774575234019328"
		longTP  = "1491774577079500800"
		shortSL = "1491774578761416704"
		shortTP = "1491774580497870848"
	)
	cases := []struct {
		name string
		call func(tr *BitgetTrader) error
		want map[string][]string // planType -> ids
	}{
		{"short side only", func(tr *BitgetTrader) error { return tr.CancelStopOrdersForSide("BTCUSDT", "SHORT") },
			map[string][]string{"pos_loss": {shortSL}, "pos_profit": {shortTP}}},
		{"long SL only", func(tr *BitgetTrader) error { return tr.CancelStopLossOrdersForSide("BTCUSDT", "LONG") },
			map[string][]string{"pos_loss": {longSL}}},
		{"all take profits", func(tr *BitgetTrader) error { return tr.CancelTakeProfitOrders("BTCUSDT") },
			map[string][]string{"pos_profit": {longTP, shortTP}}},
		{"everything", func(tr *BitgetTrader) error { return tr.CancelStopOrders("BTCUSDT") },
			map[string][]string{"pos_loss": {longSL, shortSL}, "pos_profit": {longTP, shortTP}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitgetHedge(t)
			f.setFixture(bitgetPlanPendingPath, "hedge_plan_pending")
			if err := tc.call(tr); err != nil {
				t.Fatal(err)
			}
			got := map[string][]string{}
			for _, r := range f.find(bitgetCancelPlanPath) {
				pt, _ := r.Body["planType"].(string)
				for _, e := range r.Body["orderIdList"].([]interface{}) {
					got[pt] = append(got[pt], e.(map[string]interface{})["orderId"].(string))
				}
			}
			for _, ids := range got {
				sort.Strings(ids)
			}
			for _, ids := range tc.want {
				sort.Strings(ids)
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(tc.want)
			if string(gj) != string(wj) {
				t.Errorf("cancelled %s, want %s (the planType must be the plan order's own: Bitget ignores \"profit_loss\")", gj, wj)
			}
		})
	}
}

// Real Demo behaviour: cancel-plan-order with planType "profit_loss" answers success with empty
// lists and cancels nothing, with the order's own planType it answers with the cancelled id.
func TestBitgetCancelPlanOrderRealResponses(t *testing.T) {
	t.Run("the silent no-op is detected", func(t *testing.T) {
		tr, f := newTestBitgetHedge(t)
		f.setFixture(bitgetPlanPendingPath, "hedge_plan_pending")
		f.setFixture(bitgetCancelPlanPath, "hedge_cancel_plan_profit_loss_noop")
		err := tr.CancelStopLossOrdersForSide("BTCUSDT", "LONG")
		if err == nil || !strings.Contains(err.Error(), "not confirmed") {
			t.Fatalf("an empty successList must be an error, got %v", err)
		}
	})
	t.Run("the real success shape is accepted", func(t *testing.T) {
		tr, f := newTestBitgetHedge(t)
		f.set(bitgetPlanPendingPath, `{"entrustedList":[{"planType":"pos_loss","symbol":"BTCUSDT","orderId":"1491774854834700288","posSide":"long","side":"buy"}]}`)
		f.setFixture(bitgetCancelPlanPath, "hedge_cancel_plan_poslosstype")
		if err := tr.CancelStopLossOrdersForSide("BTCUSDT", "LONG"); err != nil {
			t.Fatalf("%v", err)
		}
	})
}

// ---------------------------------------------------------------- mode handling with real responses

func TestBitgetPositionModeDetectionRealResponses(t *testing.T) {
	for _, c := range []struct{ fixture, want string }{
		{"account_detail_one_way", bitgetPosModeOneWay},
		{"account_detail_hedge", bitgetPosModeHedge},
	} {
		tr, f := newTestBitgetMode(t, false, "")
		f.setFixture(bitgetAccountDetailPath, c.fixture)
		got, err := tr.detectPositionMode()
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %s", c.fixture, got, err, c.want)
		}
	}
}

func TestBitgetEnsurePositionModeRealResponses(t *testing.T) {
	t.Run("one-way account is switched to hedge", func(t *testing.T) {
		f := newFakeBitget(t)
		f.setFixture(bitgetAccountDetailPath, "account_detail_one_way")
		f.setFixture(bitgetPositionModePath, "hedge_set_position_mode")
		tr := newBitgetTraderAt(f.srv.URL, "k", "s", "p", true)
		if tr.PositionMode() != BitgetPositionModeHedge {
			t.Errorf("mode = %q", tr.PositionMode())
		}
		if b := f.last(bitgetPositionModePath).Body; b["posMode"] != "hedge_mode" || b["productType"] != "USDT-FUTURES" {
			t.Errorf("switch body = %v", b)
		}
	})
	t.Run("refused while positions/orders exist: operate in the detected mode", func(t *testing.T) {
		f := newFakeBitget(t)
		f.setFixture(bitgetAccountDetailPath, "account_detail_one_way")
		f.setFixture(bitgetPositionModePath, "hedge_err_switch_with_positions") // 40920
		tr := newBitgetTraderAt(f.srv.URL, "k", "s", "p", true)
		if tr.PositionMode() != BitgetPositionModeOneWay || tr.TargetPositionMode() != BitgetPositionModeHedge {
			t.Errorf("effective %q target %q", tr.PositionMode(), tr.TargetPositionMode())
		}
		if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err != nil {
			t.Fatal(err)
		}
		if b := f.last(bitgetOrderPath).Body; b["tradeSide"] != nil || b["side"] != "buy" {
			t.Errorf("one-way order expected while the switch is refused: %v", b)
		}
	})
	t.Run("same-mode switch answers success", func(t *testing.T) {
		f := newFakeBitget(t)
		f.setFixture(bitgetPositionModePath, "set_position_mode_same")
		tr := newBitgetTraderCore("k", "s", "p", true)
		tr.baseURL = f.srv.URL
		if err := tr.switchPositionMode(bitgetPosModeOneWay); err != nil {
			t.Errorf("%v", err)
		}
	})
}

// The real 40774 rejections (the SAME text in both directions) trigger the refresh + retry.
func TestBitgetOrderRetryWithRealRejections(t *testing.T) {
	t.Run("hedge order sent to a one-way account", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, true, bitgetPosModeHedge)
		f.setFixture(bitgetAccountDetailPath, "account_detail_one_way")
		code, msg, _ := bitgetFixture(t, "oneway_err_40774_tradeside")
		f.failFirstN(bitgetOrderPath, 1, code, msg)
		if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err != nil {
			t.Fatal(err)
		}
		reqs := f.find(bitgetOrderPath)
		if len(reqs) != 2 || reqs[0].Body["tradeSide"] != "open" || reqs[1].Body["tradeSide"] != nil {
			t.Fatalf("requests = %+v", reqs)
		}
		if tr.PositionMode() != BitgetPositionModeOneWay {
			t.Errorf("mode = %q", tr.PositionMode())
		}
	})
	t.Run("one-way order sent to a hedge account", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, true, bitgetPosModeOneWay)
		f.setFixture(bitgetAccountDetailPath, "account_detail_hedge")
		code, msg, _ := bitgetFixture(t, "hedge_err_40774_no_tradeside")
		f.failFirstN(bitgetOrderPath, 1, code, msg)
		if _, err := tr.CloseShort("BTCUSDT", 0.001); err != nil {
			t.Fatal(err)
		}
		reqs := f.find(bitgetOrderPath)
		if len(reqs) != 2 || reqs[0].Body["reduceOnly"] != "YES" || reqs[1].Body["tradeSide"] != "close" || reqs[1].Body["side"] != "sell" {
			t.Fatalf("requests = %+v", reqs)
		}
		if _, has := reqs[1].Body["reduceOnly"]; has {
			t.Errorf("no reduceOnly in hedge mode: %v", reqs[1].Body)
		}
	})
}

func TestBitgetHoldSideMismatchRealRejections(t *testing.T) {
	for _, c := range []struct {
		fixture string
		want    bool
	}{
		{"oneway_err_tpsl_holdside_long", true}, // one-way account, holdSide=long
		{"hedge_err_tpsl_holdside_buy", true},   // hedge account, holdSide=buy
		{"hedge_err_tpsl_wrong_side", false},    // 40917: a genuine trigger-price rejection
	} {
		code, msg, _ := bitgetFixture(t, c.fixture)
		err := &BitgetAPIError{Code: code, Msg: msg}
		if got := isBitgetHoldSideMismatch(err); got != c.want {
			t.Errorf("%s (%s %q): %v, want %v", c.fixture, code, msg, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- fills / order sync with real hedge fills

func TestBitgetHedgeFixtureGetTrades(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	_, _, fills := bitgetFixture(t, "hedge_fills")
	f.setSeq(bitgetFillsPath, string(fills), `{"fillList":null,"endId":null}`)
	trades, err := tr.GetTrades(time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]BitgetTrade{}
	for _, tt := range trades {
		by[tt.TradeID] = tt
	}
	for _, w := range []struct {
		id, side, tradeSide, action string
		price, profit               float64
	}{
		{"1491774559888846848", "buy", "open", "open_long", 83471.2, 0},
		{"1491774565102366720", "sell", "open", "open_short", 83464.4, 0},
		{"1491774628545409024", "buy", "close", "close_long", 83473.1, 0.00019},
		{"1491774645620420608", "sell", "close", "close_short", 83464.9, -0.00005},
	} {
		g, ok := by[w.id]
		if !ok {
			t.Errorf("fill %s missing", w.id)
			continue
		}
		if g.Side != w.side || g.TradeSide != w.tradeSide || g.PosMode != "hedge_mode" || g.OrderAction != w.action || g.ActionAmbiguous ||
			g.FillPrice != w.price || !approx(g.ProfitLoss, w.profit) || g.FillQty != 0.0001 || g.Fee <= 0 || g.Symbol != "BTCUSDT" {
			t.Errorf("fill %s = %+v", w.id, g)
		}
	}
	// the account was one-way before: those fills keep their one-way classification
	var oneWay int
	for _, tt := range trades {
		if tt.PosMode == "one_way_mode" {
			oneWay++
			if !strings.HasSuffix(tt.TradeSide, "_single") {
				t.Errorf("one-way fill with tradeSide %q", tt.TradeSide)
			}
		}
	}
	if oneWay == 0 {
		t.Errorf("the capture contains one-way fills too")
	}
}

// The real hedge fills of the Demo run (two overlapping legs, plus an accidental reduceOnly open
// that Bitget executed as a normal open and the cleanup close) pushed through the whole sync.
func TestSyncOrdersFromBitgetRealHedgeFills(t *testing.T) {
	_, _, data := bitgetFixture(t, "hedge_fills")
	var resp struct {
		FillList []map[string]interface{} `json:"fillList"`
		EndID    string                   `json:"endId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	var hedge []map[string]interface{}
	for _, fl := range resp.FillList {
		if fl["posMode"] == "hedge_mode" && fl["cTime"].(string) >= "1791392000000" { // this run only
			hedge = append(hedge, fl)
		}
	}
	if len(hedge) != 6 {
		t.Fatalf("hedge fills of the run = %d, want 6", len(hedge))
	}
	page, _ := json.Marshal(map[string]interface{}{"fillList": hedge, "endId": hedge[len(hedge)-1]["tradeId"]})

	st, err := store.New(filepath.Join(t.TempDir(), "real-hedge.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr, f := newTestBitgetHedge(t)
	f.setSeq(bitgetFillsPath, string(page), `{"fillList":null,"endId":null}`)
	if err := tr.SyncOrdersFromBitget("trader-r", "exch-r", "bitget", st); err != nil {
		t.Fatal(err)
	}
	orders, err := st.Order().GetTraderOrders("trader-r", 50)
	if err != nil || len(orders) != 6 {
		t.Fatalf("orders = %d (%v)", len(orders), err)
	}
	got := map[string]*store.TraderOrder{}
	for _, o := range orders {
		got[o.ExchangeOrderID] = o
	}
	for id, w := range map[string]struct{ action, side, pos string }{
		"1491774545330417664": {"open_long", "BUY", "LONG"},   // the reduceOnly "open" (Bitget ignores reduceOnly in hedge mode)
		"1491774552590757888": {"close_long", "SELL", "LONG"}, // cleanup close of it
		"1491774559888846848": {"open_long", "BUY", "LONG"},
		"1491774565102366720": {"open_short", "SELL", "SHORT"},
		"1491774628545409024": {"close_long", "SELL", "LONG"},
		"1491774645620420608": {"close_short", "BUY", "SHORT"},
	} {
		o := got[id]
		if o == nil || o.OrderAction != w.action || o.Side != w.side || o.PositionSide != w.pos {
			t.Errorf("trade %s = %+v, want %+v", id, o, w)
		}
	}
	if open, _ := st.Position().GetOpenPositions("trader-r"); len(open) != 0 {
		t.Errorf("everything was closed again, open positions: %+v", open)
	}
	closed, err := st.Position().GetClosedPositions("trader-r", 10)
	if err != nil || len(closed) != 3 {
		t.Fatalf("closed positions = %d (%v), want 3 (2 long + 1 short)", len(closed), err)
	}
	var longs, shorts int
	for _, p := range closed {
		switch p.Side {
		case "LONG":
			longs++
		case "SHORT":
			shorts++
		}
	}
	if longs != 2 || shorts != 1 {
		t.Errorf("long/short = %d/%d", longs, shorts)
	}
}

// Facts established on the Bitget Demo that the trader's design relies on, pinned to the raw
// captures so a regenerated capture that contradicts them fails loudly.
func TestBitgetHedgeCapturedFacts(t *testing.T) {
	plan := func(name string) map[string]bitgetPlanOrder { // "<planType>/<posSide>" -> order
		_, _, data := bitgetFixture(t, name)
		var resp struct {
			EntrustedList []bitgetPlanOrder `json:"entrustedList"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]bitgetPlanOrder{}
		for _, o := range resp.EntrustedList {
			out[o.PlanType+"/"+o.PosSide] = o
		}
		return out
	}

	before, after := plan("hedge_plan_pending"), plan("hedge_plan_pending_after_upsert")
	if len(before) != 4 || len(after) != 4 {
		t.Fatalf("four plan orders (SL+TP per side) expected: %d / %d", len(before), len(after))
	}
	b, a := before["pos_loss/long"], after["pos_loss/long"]
	if b.OrderId == "" || b.OrderId != a.OrderId || b.TriggerPrice == a.TriggerPrice {
		t.Errorf("placing pos_loss twice for the same holdSide must replace in place (same orderId, new trigger): %+v -> %+v", b, a)
	}
	for _, k := range []string{"pos_loss/short", "pos_profit/long", "pos_profit/short"} {
		if before[k] != after[k] {
			t.Errorf("%s must be untouched by the long SL upsert: %+v -> %+v", k, before[k], after[k])
		}
	}
	for k, o := range before { // hedge plan orders name the position in posSide and carry tradeSide=close
		if o.PosSide != "long" && o.PosSide != "short" || o.Side == "" || planOrderDirection(o) != o.PosSide {
			t.Errorf("%s = %+v", k, o)
		}
	}
	// the upsert response returns the ORIGINAL orderId
	var first, again struct{ OrderId string }
	_, _, d1 := bitgetFixture(t, "hedge_tpsl_sl_long")
	_, _, d2 := bitgetFixture(t, "hedge_tpsl_sl_long_upsert")
	_ = json.Unmarshal(d1, &first)
	_ = json.Unmarshal(d2, &again)
	if first.OrderId == "" || first.OrderId != again.OrderId {
		t.Errorf("upsert orderId %q -> %q", first.OrderId, again.OrderId)
	}

	// reduceOnly is accepted but meaningless in hedge mode: the order was a plain open
	if code, _, _ := bitgetFixture(t, "hedge_place_open_reduceonly_ignored"); code != "00000" {
		t.Errorf("reduceOnly open in hedge mode: code %s", code)
	}
	// isolated + hedge: set-leverage sets both sides together (holdSide is not needed)
	_, _, lev := bitgetFixture(t, "hedge_isolated_leverage_long")
	var lv struct{ LongLeverage, ShortLeverage, MarginMode string }
	_ = json.Unmarshal(lev, &lv)
	if lv.LongLeverage == "" || lv.LongLeverage != lv.ShortLeverage || lv.MarginMode != "isolated" {
		t.Errorf("isolated hedge leverage = %+v", lv)
	}
}

func TestBitgetHedgeFixtureGetClosedPnL(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.setFixture(bitgetHistoryPosPath, "hedge_history_position")
	recs, err := tr.GetClosedPnL(time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var hedgeLong, hedgeShort int
	for _, r := range recs {
		if r.Symbol == "BTCUSDT" && r.EntryPrice == 83464.4 && r.Side == "short" {
			hedgeShort++
		}
		if r.Symbol == "BTCUSDT" && r.EntryPrice == 83471.2 && r.Side == "long" {
			hedgeLong++
		}
	}
	if hedgeShort != 1 || hedgeLong != 2 {
		t.Errorf("the hedge round trips must show up as one short and two longs: %+v", recs)
	}
}
