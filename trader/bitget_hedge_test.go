package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"nofx/store"
)

// Hedge (dual-side) mode of the live Bitget trader against an httptest server: order bodies,
// TP/SL holdSide, mode detection / switching / fallback, 40774 refresh + retry.

const (
	hedgeAccountDetail  = `{"marginCoin":"USDT","marginMode":"crossed","posMode":"hedge_mode","crossedMarginLeverage":3}`
	oneWayAccountDetail = `{"marginCoin":"USDT","marginMode":"crossed","posMode":"one_way_mode","crossedMarginLeverage":3}`
)

// ---------------------------------------------------------------- order bodies

func TestBitgetHedgeOrderBodies(t *testing.T) {
	tests := []struct {
		name      string
		call      func(tr *BitgetTrader) (map[string]interface{}, error)
		side      string
		tradeSide string
		reduce    string // "YES" on closes (fail-safe), absent on opens
	}{
		// hedge: side = POSITION direction, tradeSide = open|close; closes also carry reduceOnly=YES
		// (Bitget ignores it in hedge mode, but a close routed to a one-way account then fails as
		// reduce-only instead of adding exposure)
		{"OpenLong", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenLong("BTCUSDT", 0.0015, 5) }, "buy", "open", ""},
		{"OpenShort", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenShort("BTCUSDT", 0.0015, 5) }, "sell", "open", ""},
		{"CloseLong", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseLong("BTCUSDT", 0.0015) }, "buy", "close", "YES"},
		{"CloseShort", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseShort("BTCUSDT", 0.0015) }, "sell", "close", "YES"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitgetHedge(t)
			res, err := tc.call(tr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			b := f.last(bitgetOrderPath).Body
			if b["side"] != tc.side || b["tradeSide"] != tc.tradeSide {
				t.Errorf("side/tradeSide = %v/%v, want %s/%s (body %v)", b["side"], b["tradeSide"], tc.side, tc.tradeSide, b)
			}
			if ro, has := b["reduceOnly"]; tc.reduce == "" && has {
				t.Errorf("an opening order must not be reduceOnly: %v", b)
			} else if tc.reduce != "" && ro != tc.reduce {
				t.Errorf("a hedge close must carry reduceOnly=%s as a fail-safe: %v", tc.reduce, b)
			}
			if b["symbol"] != "BTCUSDT" || b["productType"] != "USDT-FUTURES" || b["marginCoin"] != "USDT" ||
				b["orderType"] != "market" || b["marginMode"] != "crossed" || b["size"] != "0.001" {
				t.Errorf("unexpected body: %v", b)
			}
			if res["orderId"] != "111" || res["status"] != "FILLED" {
				t.Errorf("result = %v", res)
			}
			if n := len(f.find(bitgetAccountDetailPath)); n != 0 {
				t.Errorf("a known mode needs no detection call, got %d", n)
			}
		})
	}
}

func TestBitgetOneWayOrderBodiesUnchanged(t *testing.T) {
	// the same four operations in one-way mode: real order direction + reduceOnly, no tradeSide
	want := map[string][2]string{"OpenLong": {"buy", ""}, "OpenShort": {"sell", ""}, "CloseLong": {"sell", "YES"}, "CloseShort": {"buy", "YES"}}
	calls := map[string]func(tr *BitgetTrader) (map[string]interface{}, error){
		"OpenLong":   func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenLong("BTCUSDT", 0.001, 5) },
		"OpenShort":  func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenShort("BTCUSDT", 0.001, 5) },
		"CloseLong":  func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseLong("BTCUSDT", 0.001) },
		"CloseShort": func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseShort("BTCUSDT", 0.001) },
	}
	for name, call := range calls {
		tr, f := newTestBitget(t, false)
		if _, err := call(tr); err != nil {
			t.Fatal(err)
		}
		b := f.last(bitgetOrderPath).Body
		ro, _ := b["reduceOnly"].(string)
		if b["side"] != want[name][0] || ro != want[name][1] {
			t.Errorf("%s: %v", name, b)
		}
		if _, has := b["tradeSide"]; has {
			t.Errorf("%s: tradeSide must not be sent in one-way mode", name)
		}
	}
}

func TestBitgetHedgeCloseWithoutQuantityPicksTheRightSide(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetPositionPath, `[
	 {"symbol":"BTCUSDT","holdSide":"long","total":"0.002","openPriceAvg":"50000","markPrice":"50100","unrealizedPL":"0.2","leverage":"3","liquidationPrice":"30000","posMode":"hedge_mode","cTime":"1","uTime":"2"},
	 {"symbol":"BTCUSDT","holdSide":"short","total":"0.005","openPriceAvg":"50500","markPrice":"50100","unrealizedPL":"2","leverage":"3","liquidationPrice":"70000","posMode":"hedge_mode","cTime":"1","uTime":"2"},
	 {"symbol":"ETHUSDT","holdSide":"short","total":"0.5","openPriceAvg":"2500","markPrice":"2400","unrealizedPL":"50","leverage":"3","liquidationPrice":"3500","posMode":"hedge_mode","cTime":"1","uTime":"2"}]`)

	if _, err := tr.CloseLong("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	b := f.last(bitgetOrderPath).Body
	if b["size"] != "0.002" || b["side"] != "buy" || b["tradeSide"] != "close" {
		t.Errorf("CloseLong(0) must close the 0.002 long: %v", b)
	}
	tr.clearCache()
	if _, err := tr.CloseShort("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	b = f.last(bitgetOrderPath).Body
	if b["size"] != "0.005" || b["side"] != "sell" || b["tradeSide"] != "close" {
		t.Errorf("CloseShort(0) must close the 0.005 short: %v", b)
	}
	// a side that is not held cannot be closed
	if _, err := tr.CloseLong("ETHUSDT", 0); err == nil || !strings.Contains(err.Error(), "long position not found") {
		t.Errorf("no ETH long: %v", err)
	}
}

func TestBitgetGetPositionsReturnsBothSidesOfASymbol(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetPositionPath, `[
	 {"symbol":"BTCUSDT","holdSide":"long","total":"0.002","openPriceAvg":"50000","markPrice":"50100","unrealizedPL":"0.2","leverage":"3","liquidationPrice":"30000","cTime":"1","uTime":"2"},
	 {"symbol":"BTCUSDT","holdSide":"short","total":"0.005","openPriceAvg":"50500","markPrice":"50100","unrealizedPL":"2","leverage":"3","liquidationPrice":"-12345.6","cTime":"3","uTime":"4"}]`)
	pos, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 2 || pos[0]["side"] != "long" || pos[1]["side"] != "short" {
		t.Fatalf("both legs expected, got %v", pos)
	}
	if pos[0]["positionAmt"].(float64) != 0.002 || pos[1]["positionAmt"].(float64) != 0.005 {
		t.Errorf("quantities: %v", pos)
	}
	if pos[1]["liquidationPrice"].(float64) != 0 {
		t.Errorf("a negative liquidation price means n/a (0): %v", pos[1])
	}
}

// ---------------------------------------------------------------- TP/SL

func TestBitgetHedgeTPSLHoldSide(t *testing.T) {
	tests := []struct {
		name       string
		call       func(tr *BitgetTrader) error
		hold, plan string
		trigger    string
	}{
		{"SL long", func(tr *BitgetTrader) error { return tr.SetStopLoss("BTCUSDT", "LONG", 1, 49123.46) }, "long", "pos_loss", "49123.5"},
		{"SL short", func(tr *BitgetTrader) error { return tr.SetStopLoss("BTCUSDT", "SHORT", 1, 51000) }, "short", "pos_loss", "51000.0"},
		{"TP long", func(tr *BitgetTrader) error { return tr.SetTakeProfit("BTCUSDT", "long", 1, 52000.5) }, "long", "pos_profit", "52000.5"},
		{"TP short", func(tr *BitgetTrader) error { return tr.SetTakeProfit("BTCUSDT", "short", 1, 48000) }, "short", "pos_profit", "48000.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitgetHedge(t)
			if err := tc.call(tr); err != nil {
				t.Fatal(err)
			}
			b := f.last(bitgetPlaceTPSLPath).Body
			if b["holdSide"] != tc.hold || b["planType"] != tc.plan || b["triggerPrice"] != tc.trigger ||
				b["triggerType"] != "mark_price" || b["symbol"] != "BTCUSDT" || b["marginCoin"] != "USDT" {
				t.Errorf("body = %v", b)
			}
			// TP/SL placement is an upsert: nothing is listed or cancelled first
			if len(f.find(bitgetPlanPendingPath)) != 0 || len(f.find(bitgetCancelPlanPath)) != 0 {
				t.Errorf("hedge TP/SL must not cancel first")
			}
		})
	}
}

const hedgePlanList = `{"entrustedList":[
 {"planType":"pos_loss","symbol":"BTCUSDT","orderId":"L-SL","triggerPrice":"49000","side":"buy","posSide":"long","size":"0"},
 {"planType":"pos_profit","symbol":"BTCUSDT","orderId":"L-TP","triggerPrice":"52000","side":"buy","posSide":"long","size":"0"},
 {"planType":"pos_loss","symbol":"BTCUSDT","orderId":"S-SL","triggerPrice":"51000","side":"sell","posSide":"short","size":"0"},
 {"planType":"pos_profit","symbol":"BTCUSDT","orderId":"S-TP","triggerPrice":"48000","side":"sell","posSide":"short","size":"0"}]}`

func canceledPlanIDs(f *fakeBitget) []string {
	var ids []string
	for _, r := range f.find(bitgetCancelPlanPath) {
		list, _ := r.Body["orderIdList"].([]interface{})
		for _, e := range list {
			ids = append(ids, e.(map[string]interface{})["orderId"].(string))
		}
	}
	return ids
}

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestBitgetHedgeCancelOnlyTheRequestedSide(t *testing.T) {
	cases := []struct {
		name string
		call func(tr *BitgetTrader) error
		want []string
	}{
		{"SL of the long", func(tr *BitgetTrader) error { return tr.CancelStopLossOrdersForSide("BTCUSDT", "LONG") }, []string{"L-SL"}},
		{"SL of the short", func(tr *BitgetTrader) error { return tr.CancelStopLossOrdersForSide("BTCUSDT", "short") }, []string{"S-SL"}},
		{"TP of the long", func(tr *BitgetTrader) error { return tr.CancelTakeProfitOrdersForSide("BTCUSDT", "LONG") }, []string{"L-TP"}},
		{"TP of the short", func(tr *BitgetTrader) error { return tr.CancelTakeProfitOrdersForSide("BTCUSDT", "SHORT") }, []string{"S-TP"}},
		{"both of the long", func(tr *BitgetTrader) error { return tr.CancelStopOrdersForSide("BTCUSDT", "LONG") }, []string{"L-SL", "L-TP"}},
		{"both of the short", func(tr *BitgetTrader) error { return tr.CancelStopOrdersForSide("BTCUSDT", "SHORT") }, []string{"S-SL", "S-TP"}},
		// no side given = the whole symbol (the Trader interface has no side)
		{"all SL", func(tr *BitgetTrader) error { return tr.CancelStopLossOrders("BTCUSDT") }, []string{"L-SL", "S-SL"}},
		{"all TP", func(tr *BitgetTrader) error { return tr.CancelTakeProfitOrders("BTCUSDT") }, []string{"L-TP", "S-TP"}},
		{"everything", func(tr *BitgetTrader) error { return tr.CancelStopOrders("BTCUSDT") }, []string{"L-SL", "L-TP", "S-SL", "S-TP"}},
		{"empty side = both", func(tr *BitgetTrader) error { return tr.CancelStopOrdersForSide("BTCUSDT", "") }, []string{"L-SL", "L-TP", "S-SL", "S-TP"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitgetHedge(t)
			f.set(bitgetPlanPendingPath, hedgePlanList)
			if err := tc.call(tr); err != nil {
				t.Fatal(err)
			}
			if got := canceledPlanIDs(f); !sameIDs(got, tc.want...) {
				t.Errorf("cancelled %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBitgetHedgeGetOpenOrdersPlanOrders(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetPlanPendingPath, hedgePlanList)
	orders, err := tr.GetOpenOrders("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]OpenOrder{}
	for _, o := range orders {
		got[o.OrderID] = o
	}
	want := map[string]struct{ pos, side, typ string }{
		"L-SL": {"LONG", "SELL", "STOP_MARKET"}, "L-TP": {"LONG", "SELL", "TAKE_PROFIT_MARKET"},
		"S-SL": {"SHORT", "BUY", "STOP_MARKET"}, "S-TP": {"SHORT", "BUY", "TAKE_PROFIT_MARKET"},
	}
	for id, w := range want {
		o, ok := got[id]
		if !ok || o.PositionSide != w.pos || o.Side != w.side || o.Type != w.typ {
			t.Errorf("%s = %+v, want %+v", id, o, w)
		}
	}
	if len(got) != 4 {
		t.Errorf("orders = %+v", orders)
	}
}

func TestBitgetHedgeGetOpenOrdersPendingOrders(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetPendingPath, `{"entrustedList":[
	 {"orderId":"1","symbol":"BTCUSDT","status":"live","side":"buy","posSide":"long","tradeSide":"open","orderType":"limit","price":"40000","size":"0.001","reduceOnly":"NO"},
	 {"orderId":"2","symbol":"BTCUSDT","status":"live","side":"buy","posSide":"long","tradeSide":"close","orderType":"limit","price":"60000","size":"0.001","reduceOnly":"NO"},
	 {"orderId":"3","symbol":"BTCUSDT","status":"live","side":"sell","posSide":"short","tradeSide":"open","orderType":"limit","price":"60000","size":"0.001","reduceOnly":"NO"},
	 {"orderId":"4","symbol":"BTCUSDT","status":"live","side":"sell","posSide":"short","tradeSide":"close","orderType":"limit","price":"40000","size":"0.001","reduceOnly":"NO"}]}`)
	orders, err := tr.GetOpenOrders("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	// hedge `side` is the position direction; report the real order direction like every other exchange
	want := map[string][2]string{"1": {"BUY", "LONG"}, "2": {"SELL", "LONG"}, "3": {"SELL", "SHORT"}, "4": {"BUY", "SHORT"}}
	if len(orders) != 4 {
		t.Fatalf("orders = %+v", orders)
	}
	for _, o := range orders {
		if w := want[o.OrderID]; o.Side != w[0] || o.PositionSide != w[1] || o.Status != "NEW" {
			t.Errorf("%s = %+v, want side %s / %s", o.OrderID, o, w[0], w[1])
		}
	}
}

// ---------------------------------------------------------------- mode handling

func TestBitgetPositionModeNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"": "hedge", "hedge": "hedge", "HEDGE": "hedge", "hedge_mode": "hedge", "garbage": "hedge",
		"one_way": "one_way", "One-Way": "one_way", "oneway": "one_way", "one_way_mode": "one_way", " net ": "one_way",
	} {
		if got := NormalizeBitgetPositionMode(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	for _, ok := range []string{"", "hedge", "one_way", " HEDGE "} {
		if !IsValidBitgetPositionModeSetting(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"hedge_mode", "net", "both", "x"} {
		if IsValidBitgetPositionModeSetting(bad) {
			t.Errorf("%q must be rejected by the API", bad)
		}
	}
}

func TestBitgetTraderDefaultsToHedgeTarget(t *testing.T) {
	tr := newBitgetTraderCore("k", "s", "p", false)
	if tr.TargetPositionMode() != BitgetPositionModeHedge || tr.PositionMode() != "" || !tr.IsHedgeMode() {
		t.Errorf("target %q effective %q hedge=%v", tr.TargetPositionMode(), tr.PositionMode(), tr.IsHedgeMode())
	}
	WithBitgetPositionMode("one_way")(tr)
	if tr.TargetPositionMode() != BitgetPositionModeOneWay || tr.IsHedgeMode() {
		t.Errorf("one_way option not applied: %q", tr.TargetPositionMode())
	}
}

func newFakeForModeTests(t *testing.T, detect string) *fakeBitget {
	f := newFakeBitget(t)
	if detect != "" {
		f.set(bitgetAccountDetailPath, detect)
	}
	f.set(bitgetPositionModePath, `{"posMode":"x"}`)
	return f
}

func TestBitgetEnsurePositionMode(t *testing.T) {
	type check func(t *testing.T, tr *BitgetTrader, f *fakeBitget)
	switchBodies := func(f *fakeBitget) []map[string]interface{} {
		var out []map[string]interface{}
		for _, r := range f.find(bitgetPositionModePath) {
			out = append(out, r.Body)
		}
		return out
	}
	tests := []struct {
		name   string
		detect string // account detail data; "" = endpoint missing (detection fails)
		opts   []BitgetOption
		setup  func(f *fakeBitget)
		check  check
	}{
		{
			name: "default target hedge, account one-way: switches", detect: oneWayAccountDetail,
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				sw := switchBodies(f)
				if len(sw) != 1 || sw[0]["posMode"] != "hedge_mode" || sw[0]["productType"] != "USDT-FUTURES" {
					t.Errorf("switch bodies = %v", sw)
				}
				if tr.PositionMode() != BitgetPositionModeHedge {
					t.Errorf("mode = %q", tr.PositionMode())
				}
			},
		},
		{
			name: "already in the target mode: no switch call", detect: hedgeAccountDetail,
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if n := len(switchBodies(f)); n != 0 {
					t.Errorf("no switch expected, got %d", n)
				}
				if tr.PositionMode() != BitgetPositionModeHedge {
					t.Errorf("mode = %q", tr.PositionMode())
				}
				q := f.last(bitgetAccountDetailPath).Query
				if q["symbol"] != "BTCUSDT" || q["productType"] != "USDT-FUTURES" || q["marginCoin"] != "USDT" {
					t.Errorf("detection query = %v", q)
				}
			},
		},
		{
			name: "one_way option, account hedge: switches to one-way", detect: hedgeAccountDetail,
			opts: []BitgetOption{WithBitgetPositionMode("one_way")},
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				sw := switchBodies(f)
				if len(sw) != 1 || sw[0]["posMode"] != "one_way_mode" {
					t.Errorf("switch bodies = %v", sw)
				}
				if tr.PositionMode() != BitgetPositionModeOneWay || tr.IsHedgeMode() {
					t.Errorf("mode = %q", tr.PositionMode())
				}
			},
		},
		{
			name: "switch refused (positions exist): operate in the detected mode", detect: oneWayAccountDetail,
			setup: func(f *fakeBitget) {
				f.fail(bitgetPositionModePath, "40920", "Position or order exists, the position mode cannot be switched")
			},
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != BitgetPositionModeOneWay || tr.TargetPositionMode() != BitgetPositionModeHedge {
					t.Errorf("effective %q target %q", tr.PositionMode(), tr.TargetPositionMode())
				}
				// ... and orders really are one-way ones
				if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err != nil {
					t.Fatal(err)
				}
				if b := f.last(bitgetOrderPath).Body; b["side"] != "buy" || b["tradeSide"] != nil {
					t.Errorf("one-way order expected: %v", b)
				}
			},
		},
		{
			name: "switch refused while hedge account and one_way wanted", detect: hedgeAccountDetail,
			opts: []BitgetOption{WithBitgetPositionMode("one_way")},
			setup: func(f *fakeBitget) {
				f.fail(bitgetPositionModePath, "40920", "Position or order exists, the position mode cannot be switched")
			},
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != BitgetPositionModeHedge {
					t.Errorf("must keep operating in hedge, got %q", tr.PositionMode())
				}
				if _, err := tr.OpenShort("BTCUSDT", 0.001, 3); err != nil {
					t.Fatal(err)
				}
				if b := f.last(bitgetOrderPath).Body; b["tradeSide"] != "open" || b["side"] != "sell" {
					t.Errorf("hedge order expected: %v", b)
				}
			},
		},
		{
			name: "detection fails but the switch succeeds: target applies", detect: "",
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != BitgetPositionModeHedge {
					t.Errorf("mode = %q", tr.PositionMode())
				}
			},
		},
		{
			name: "detection fails and the switch fails: mode unknown, orders use the target", detect: "",
			setup: func(f *fakeBitget) {
				f.fail(bitgetPositionModePath, "40920", "Position or order exists, the position mode cannot be switched")
			},
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != "" {
					t.Errorf("mode must stay unknown, got %q", tr.PositionMode())
				}
				if !tr.IsHedgeMode() {
					t.Errorf("orders use the target (hedge) while the mode is unknown")
				}
				if n := len(f.find(bitgetAccountDetailPath)); n != 2 {
					t.Errorf("detection must be retried once after the failed switch, got %d calls", n)
				}
			},
		},
		{
			name: "'already in this mode' reply counts as success", detect: oneWayAccountDetail,
			setup: func(f *fakeBitget) {
				f.fail(bitgetPositionModePath, "40920", "The position mode is the same as the current one")
			},
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != BitgetPositionModeHedge {
					t.Errorf("mode = %q", tr.PositionMode())
				}
			},
		},
		{
			name: "garbage posMode is not trusted", detect: `{"posMode":"weird"}`,
			check: func(t *testing.T, tr *BitgetTrader, f *fakeBitget) {
				if tr.PositionMode() != BitgetPositionModeHedge { // switch to the target succeeded
					t.Errorf("mode = %q", tr.PositionMode())
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForModeTests(t, tc.detect)
			if tc.setup != nil {
				tc.setup(f)
			}
			tr := newBitgetTraderAt(f.srv.URL, "test-key", "test-secret", "test-pass", false, tc.opts...)
			tr.orderPollInterval = 1
			tc.check(t, tr, f)
		})
	}
}

// ---------------------------------------------------------------- 40774 refresh + retry

func TestBitgetOrderRetriesOnceAfterModeMismatch(t *testing.T) {
	const msg = "The order type for unilateral position must also be the unilateral position type." // real text, both directions
	t.Run("believed hedge, account one-way", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
		f.set(bitgetAccountDetailPath, oneWayAccountDetail)
		f.failFirstN(bitgetOrderPath, 1, "40774", msg)
		res, err := tr.CloseLong("BTCUSDT", 0.001)
		if err != nil {
			t.Fatalf("must succeed after the refresh: %v", err)
		}
		if res["status"] != "FILLED" {
			t.Errorf("result = %v", res)
		}
		reqs := f.find(bitgetOrderPath)
		if len(reqs) != 2 {
			t.Fatalf("expected exactly one retry, got %d order requests", len(reqs))
		}
		first, second := reqs[0].Body, reqs[1].Body
		if first["tradeSide"] != "close" || first["side"] != "buy" || first["reduceOnly"] != "YES" {
			t.Errorf("first attempt must be a reduce-only hedge close: %v", first)
		}
		if second["tradeSide"] != nil || second["side"] != "sell" || second["reduceOnly"] != "YES" {
			t.Errorf("retry must be a one-way close: %v", second)
		}
		if first["clientOid"] == second["clientOid"] {
			t.Errorf("the retry needs a fresh clientOid")
		}
		if first["size"] != second["size"] || first["symbol"] != second["symbol"] {
			t.Errorf("the retry must be the same order, first %v retry %v", first, second)
		}
		if tr.PositionMode() != BitgetPositionModeOneWay {
			t.Errorf("mode must be refreshed to one_way, got %q", tr.PositionMode())
		}
		// later orders use the refreshed mode without another rejection
		if _, err := tr.OpenShort("BTCUSDT", 0.001, 3); err != nil {
			t.Fatal(err)
		}
		if n := len(f.find(bitgetOrderPath)); n != 3 {
			t.Errorf("no further retries expected, order requests = %d", n)
		}
	})
	t.Run("believed one-way, account hedge", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetAccountDetailPath, hedgeAccountDetail)
		f.failFirstN(bitgetOrderPath, 1, "40774", "The order type for unilateral position must also be the unilateral position type.")
		if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err != nil {
			t.Fatal(err)
		}
		reqs := f.find(bitgetOrderPath)
		if len(reqs) != 2 || reqs[0].Body["tradeSide"] != nil || reqs[1].Body["tradeSide"] != "open" || reqs[1].Body["side"] != "buy" {
			t.Fatalf("requests = %+v", reqs)
		}
		if tr.PositionMode() != BitgetPositionModeHedge {
			t.Errorf("mode = %q", tr.PositionMode())
		}
	})
	t.Run("unknown mode, detection fails: flip to the other mode", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, "") // unknown -> target (hedge)
		f.failFirstN(bitgetOrderPath, 1, "40774", msg)
		// no account endpoint configured: detection fails (40404)
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
	t.Run("still rejected after the retry: error is returned, no loop", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
		f.set(bitgetAccountDetailPath, oneWayAccountDetail)
		f.fail(bitgetOrderPath, "40774", msg)
		_, err := tr.OpenLong("BTCUSDT", 0.001, 3)
		if err == nil || !strings.Contains(err.Error(), "40774") {
			t.Fatalf("expected the 40774 error, got %v", err)
		}
		if n := len(f.find(bitgetOrderPath)); n != 2 {
			t.Errorf("exactly one retry, got %d attempts", n)
		}
	})
	t.Run("mode already correct: 40774 has another cause, no retry", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
		f.set(bitgetAccountDetailPath, hedgeAccountDetail)
		f.fail(bitgetOrderPath, "40774", "something else")
		if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err == nil {
			t.Fatal("expected an error")
		}
		if n := len(f.find(bitgetOrderPath)); n != 1 {
			t.Errorf("no retry expected, got %d attempts", n)
		}
	})
	t.Run("a close failing for any other reason is never resubmitted", func(t *testing.T) {
		// only a definitive 40774 (rejected by validation, never executed) is retried; a system error or
		// an unknown outcome must surface instead of risking a second close/open
		for _, code := range []string{"50000", "40762", "45110"} {
			tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
			f.fail(bitgetOrderPath, code, "system busy")
			if _, err := tr.CloseLong("BTCUSDT", 0.001); err == nil {
				t.Fatalf("%s: expected an error", code)
			}
			reqs := f.find(bitgetOrderPath)
			if len(reqs) != 1 || reqs[0].Body["tradeSide"] != "close" || reqs[0].Body["reduceOnly"] != "YES" {
				t.Errorf("%s: exactly one reduce-only hedge close expected, got %+v", code, reqs)
			}
			if n := len(f.find(bitgetAccountDetailPath)); n != 0 {
				t.Errorf("%s: no mode re-detection expected, got %d", code, n)
			}
		}
	})
	t.Run("other errors never trigger a mode refresh", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
		f.fail(bitgetOrderPath, "40762", "insufficient balance")
		if _, err := tr.OpenLong("BTCUSDT", 0.001, 3); err == nil {
			t.Fatal("expected an error")
		}
		if n := len(f.find(bitgetAccountDetailPath)); n != 0 {
			t.Errorf("no detection expected, got %d", n)
		}
	})
}

func TestBitgetTPSLRetriesOnceAfterModeMismatch(t *testing.T) {
	// real rejections captured on Bitget Demo (code 43011, not 40774)
	cases := []struct {
		name             string
		believed, actual string
		detail           string
		msg              string
		side             string
		firstHold        string
		retryHold        string
	}{
		{"believed hedge, account one-way", bitgetPosModeHedge, bitgetPosModeOneWay, oneWayAccountDetail,
			"The parameter does not meet the specification holdSide error", "SHORT", "short", "sell"},
		{"believed one-way, account hedge", bitgetPosModeOneWay, bitgetPosModeHedge, hedgeAccountDetail,
			"The parameter does not meet the specification d delegateType is error", "LONG", "buy", "long"},
		{"40774 is accepted too", bitgetPosModeHedge, bitgetPosModeOneWay, oneWayAccountDetail, "mode mismatch", "SHORT", "short", "sell"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, f := newTestBitgetMode(t, false, c.believed)
			f.set(bitgetAccountDetailPath, c.detail)
			code := "43011"
			if c.msg == "mode mismatch" {
				code = "40774"
			}
			f.failFirstN(bitgetPlaceTPSLPath, 1, code, c.msg)
			if err := tr.SetStopLoss("BTCUSDT", c.side, 1, map[string]float64{"SHORT": 51000, "LONG": 49000}[c.side]); err != nil {
				t.Fatalf("must succeed after the refresh: %v", err)
			}
			reqs := f.find(bitgetPlaceTPSLPath)
			if len(reqs) != 2 || reqs[0].Body["holdSide"] != c.firstHold || reqs[1].Body["holdSide"] != c.retryHold {
				t.Fatalf("holdSide must switch vocabulary (%s -> %s): %+v", c.firstHold, c.retryHold, reqs)
			}
			if bitgetAPIPosMode(tr.PositionMode()) != c.actual {
				t.Errorf("mode = %q, want %s", tr.PositionMode(), c.actual)
			}
		})
	}
	t.Run("an unrelated 43011 does not refresh the mode", func(t *testing.T) {
		tr, f := newTestBitgetMode(t, false, bitgetPosModeHedge)
		f.fail(bitgetPlaceTPSLPath, "43011", "The parameter does not meet the specification triggerPrice error")
		if err := tr.SetStopLoss("BTCUSDT", "LONG", 1, 49000); err == nil {
			t.Fatal("expected an error")
		}
		if n := len(f.find(bitgetAccountDetailPath)); n != 0 {
			t.Errorf("no detection expected, got %d", n)
		}
		if n := len(f.find(bitgetPlaceTPSLPath)); n != 1 {
			t.Errorf("no retry expected, got %d attempts", n)
		}
	})
}

func TestBitgetModeIsSafeForConcurrentUse(t *testing.T) {
	tr, f := newTestBitgetHedge(t)
	f.set(bitgetAccountDetailPath, oneWayAccountDetail)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			tr.setPosMode(bitgetPosModeOneWay)
			tr.setPosMode(bitgetPosModeHedge)
		}
	}()
	for i := 0; i < 200; i++ {
		_ = tr.PositionMode()
		_ = tr.IsHedgeMode()
		_ = tr.TargetPositionMode()
	}
	<-done
}

// ---------------------------------------------------------------- order sync (hedge fills)

// hedgeFill builds a hedge-mode fill in the exact shape of the real fills (see fills.json): side
// is the POSITION direction, tradeSide open|close, posMode hedge_mode.
func hedgeFill(base map[string]interface{}, tradeID, orderID, side, tradeSide, price, profit string, ms int64) map[string]interface{} {
	return derivedBitgetFill(base, map[string]interface{}{
		"tradeId": tradeID, "orderId": orderID, "side": side, "tradeSide": tradeSide, "posMode": "hedge_mode",
		"price": price, "profit": profit, "cTime": fmt.Sprintf("%d", ms),
	})
}

// A hedge round trip with overlapping legs (open long, open short, close long, close short) is
// classified from tradeSide alone (never ambiguous), stored with the real order side and the
// LONG / SHORT leg, and both legs end up as separate CLOSED positions. Even a break-even close
// (profit 0) must stay a close: hedge fills are never "ambiguous".
func TestSyncOrdersFromBitgetHedgeFills(t *testing.T) {
	for _, breakEven := range []bool{false, true} {
		name := "with profits"
		if breakEven {
			name = "break-even closes"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.New(filepath.Join(t.TempDir(), "hedge-sync.db"))
			if err != nil {
				t.Fatal(err)
			}
			tr, f := newTestBitgetHedge(t)
			base := bitgetFillsFixture(t)[3] // a real fill row to clone
			longPnl, shortPnl := "0.0123", "-0.0045"
			if breakEven {
				longPnl, shortPnl = "0", "0"
			}
			const t0 = int64(1791390000000)
			fills := []map[string]interface{}{ // newest first, as served
				hedgeFill(base, "H4", "O4", "sell", "close", "83410.5", shortPnl, t0+3000), // close short
				hedgeFill(base, "H3", "O3", "buy", "close", "83420.1", longPnl, t0+2000),   // close long
				hedgeFill(base, "H2", "O2", "sell", "open", "83400.0", "0", t0+1000),       // open short
				hedgeFill(base, "H1", "O1", "buy", "open", "83399.9", "0", t0),             // open long
			}
			page, _ := json.Marshal(map[string]interface{}{"fillList": fills, "endId": "H1"})
			f.setSeq(bitgetFillsPath, string(page), `{"fillList":null,"endId":null}`)

			if err := tr.SyncOrdersFromBitget("trader-h", "exch-h", "bitget", st); err != nil {
				t.Fatal(err)
			}
			orders, err := st.Order().GetTraderOrders("trader-h", 50)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]*store.TraderOrder{}
			for _, o := range orders {
				got[o.ExchangeOrderID] = o
			}
			want := map[string]struct{ action, side, posSide string }{
				"H1": {"open_long", "BUY", "LONG"},
				"H2": {"open_short", "SELL", "SHORT"},
				"H3": {"close_long", "SELL", "LONG"},
				"H4": {"close_short", "BUY", "SHORT"},
			}
			if len(got) != len(want) {
				t.Fatalf("orders = %d, want %d", len(got), len(want))
			}
			for id, w := range want {
				o := got[id]
				if o == nil || o.OrderAction != w.action || o.Side != w.side || o.PositionSide != w.posSide {
					t.Errorf("trade %s = %+v, want %+v", id, o, w)
				}
			}
			if open, _ := st.Position().GetOpenPositions("trader-h"); len(open) != 0 {
				t.Errorf("no position may stay open: %+v", open)
			}
			closed, err := st.Position().GetClosedPositions("trader-h", 10)
			if err != nil || len(closed) != 2 {
				t.Fatalf("closed positions = %d (%v), want 2 (one per leg)", len(closed), err)
			}
			bySide := map[string]*store.TraderPosition{}
			for _, p := range closed {
				bySide[p.Side] = p
			}
			if l := bySide["LONG"]; l == nil || l.EntryPrice != 83399.9 || math.Abs(l.ExitPrice-83420.1) > 0.011 || l.Status != "CLOSED" {
				t.Errorf("long position = %+v", l)
			}
			if s := bySide["SHORT"]; s == nil || s.EntryPrice != 83400 || math.Abs(s.ExitPrice-83410.5) > 0.011 || s.Status != "CLOSED" {
				t.Errorf("short position = %+v", s)
			}
			// idempotent re-sync
			if err := tr.SyncOrdersFromBitget("trader-h", "exch-h", "bitget", st); err != nil {
				t.Fatal(err)
			}
			if orders2, _ := st.Order().GetTraderOrders("trader-h", 50); len(orders2) != len(orders) {
				t.Errorf("re-sync must be idempotent: %d -> %d", len(orders), len(orders2))
			}
		})
	}
}

func TestClassifyBitgetHedgeFillsNeverAmbiguous(t *testing.T) {
	for _, c := range []struct {
		side, tradeSide string
		profit          float64
		want            string
	}{
		{"buy", "open", 0, "open_long"},
		{"sell", "open", 0, "open_short"},
		{"buy", "close", 0, "close_long"}, // break-even close stays a close
		{"sell", "close", 0, "close_short"},
		{"buy", "close", 2.29028, "close_long"}, // real fill: buy+close, profit 2.29028 closed a long
		{"sell", "close", -1.5, "close_short"},
		{"buy", "burst_close_long", -4, "close_long"},
		{"sell", "burst_close_short", -4, "close_short"},
		{"buy", "reduce_close_long", 3, "close_long"},
		{"sell", "reduce_close_short", 3, "close_short"},
	} {
		action, ambiguous := classifyBitgetFillDetailed(c.side, c.tradeSide, c.profit)
		if action != c.want || ambiguous {
			t.Errorf("%s/%s profit %v = %s (ambiguous=%v), want %s", c.side, c.tradeSide, c.profit, action, ambiguous, c.want)
		}
		// the position-aware path must agree and must never consult the open positions
		never := func(symbol, side string) (bool, error) {
			t.Errorf("hedge fills must not need a position lookup")
			return false, nil
		}
		if got := classifyBitgetFillWithPositions("BTCUSDT", c.side, c.tradeSide, c.profit, never); got != c.want {
			t.Errorf("with positions: %s/%s = %s, want %s", c.side, c.tradeSide, got, c.want)
		}
	}
}
