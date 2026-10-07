package trader

// Tests driven by REAL Bitget Demo responses (trader/testdata/bitget/*.json, captured verbatim
// from the Demo environment: one-way mode, USDT-FUTURES, header paptrading:1).

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ---------- positions ----------

func TestBitgetFixturePositions(t *testing.T) {
	tr, f := newTestBitget(t, true)
	f.setFixture(bitgetPositionPath, "positions")

	pos, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 {
		t.Fatalf("positions = %+v", pos)
	}
	p := pos[0]
	// holdSide is "long" even in one-way mode (posMode one_way_mode)
	if p["symbol"] != "BTCUSDT" || p["side"] != "long" {
		t.Errorf("symbol/side = %v/%v", p["symbol"], p["side"])
	}
	want := map[string]float64{
		"positionAmt": 0.0001, "entryPrice": 83343.3, "markPrice": 83344.1,
		"unRealizedProfit": 0.00008, "leverage": 3,
	}
	for k, w := range want {
		if got, ok := p[k].(float64); !ok || !approx(got, w) {
			t.Errorf("%s = %v, want %v", k, p[k], w)
		}
	}
	// The real liquidationPrice is "-279781303.0828121933333334" (a 3x cross long can't be
	// liquidated by price). It must never reach the prompt as a negative number.
	if got, _ := p["liquidationPrice"].(float64); got != 0 {
		t.Errorf("liquidationPrice = %v, want 0 (n/a) for a negative exchange value", p["liquidationPrice"])
	}
	if p["createdTime"].(int64) != 1791388585219 || p["updatedTime"].(int64) != 1791388585219 {
		t.Errorf("times = %v / %v", p["createdTime"], p["updatedTime"])
	}
	if q := f.last(bitgetPositionPath).Query; q["productType"] != "USDT-FUTURES" || q["marginCoin"] != "USDT" {
		t.Errorf("query = %v", q)
	}
	if f.last(bitgetPositionPath).Header.Get("paptrading") != "1" {
		t.Errorf("demo trader must send paptrading: 1")
	}
}

func TestSanitizeBitgetLiquidationPrice(t *testing.T) {
	tests := map[string]float64{
		"-279781303.0828121933333334": 0,
		"-1":                          0,
		"0":                           0,
		"":                            0,
		"abc":                         0,
		"NaN":                         0,
		"+Inf":                        0,
		"-Inf":                        0,
		"39000.5":                     39000.5,
		" 120 ":                       120,
	}
	for raw, want := range tests {
		if got := sanitizeBitgetLiquidationPrice(raw); got != want {
			t.Errorf("sanitize(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestBitgetGetPositionsOneWayShortAndFlat(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetPositionPath, `[
		{"symbol":"BTCUSDT","holdSide":"short","total":"0.0002","openPriceAvg":"90000","markPrice":"89900","unrealizedPL":"0.02","leverage":"5","liquidationPrice":"108000.5","posMode":"one_way_mode","cTime":"1","uTime":"2"},
		{"symbol":"ETHUSDT","holdSide":"long","total":"0","openPriceAvg":"0","markPrice":"3000","unrealizedPL":"0","leverage":"5","liquidationPrice":"","cTime":"1","uTime":"2"}]`)
	pos, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0]["side"] != "short" || pos[0]["liquidationPrice"].(float64) != 108000.5 {
		t.Errorf("positions = %+v (flat entries must be skipped, positive liq price kept)", pos)
	}

	// no open positions: the exchange returns an empty array
	f.set(bitgetPositionPath, `[]`)
	tr.clearCache()
	pos, err = tr.GetPositions()
	if err != nil || len(pos) != 0 {
		t.Errorf("flat: pos=%v err=%v", pos, err)
	}
}

// ---------- plan (TP/SL) orders ----------

func TestBitgetFixtureGetOpenOrdersOneWayPlans(t *testing.T) {
	t.Run("long position: SL/TP are SELL orders protecting a LONG", func(t *testing.T) {
		tr, f := newTestBitget(t, true)
		f.setFixture(bitgetPendingPath, "orders_pending")
		f.setFixture(bitgetPlanPendingPath, "plan_pending")

		orders, err := tr.GetOpenOrders("BTCUSDT")
		if err != nil {
			t.Fatal(err)
		}
		if len(orders) != 2 {
			t.Fatalf("orders = %+v", orders)
		}
		byID := map[string]OpenOrder{}
		for _, o := range orders {
			byID[o.OrderID] = o
		}
		tp := byID["1491757344492109824"]
		if tp.Type != "TAKE_PROFIT_MARKET" || tp.StopPrice != 92000 || tp.Side != "SELL" || tp.PositionSide != "LONG" ||
			tp.Symbol != "BTCUSDT" || tp.Status != "NEW" {
			t.Errorf("TP = %+v", tp)
		}
		sl := byID["1491757342466260992"]
		if sl.Type != "STOP_MARKET" || sl.StopPrice != 75000 || sl.Side != "SELL" || sl.PositionSide != "LONG" ||
			sl.Symbol != "BTCUSDT" || sl.Status != "NEW" {
			t.Errorf("SL = %+v", sl)
		}
	})

	t.Run("short position: SL is a BUY order protecting a SHORT", func(t *testing.T) {
		tr, f := newTestBitget(t, true)
		f.setFixture(bitgetPlanPendingPath, "plan_pending_short")
		orders, err := tr.GetOpenOrders("BTCUSDT")
		if err != nil {
			t.Fatal(err)
		}
		if len(orders) != 1 {
			t.Fatalf("orders = %+v", orders)
		}
		sl := orders[0]
		if sl.OrderID != "1491757573371084800" || sl.Type != "STOP_MARKET" || sl.StopPrice != 91000 ||
			sl.Side != "BUY" || sl.PositionSide != "SHORT" {
			t.Errorf("SL = %+v", sl)
		}
	})

	t.Run("null entrustedList is an empty, non-nil result", func(t *testing.T) {
		tr, f := newTestBitget(t, true)
		f.setFixture(bitgetPendingPath, "orders_pending")
		f.setFixture(bitgetPlanPendingPath, "plan_pending_after_close")
		orders, err := tr.GetOpenOrders("BTCUSDT")
		if err != nil {
			t.Fatal(err)
		}
		if orders == nil || len(orders) != 0 {
			t.Errorf("orders = %#v, want empty non-nil slice", orders)
		}
	})
}

func TestBitgetPlanOrderDirection(t *testing.T) {
	tests := []struct {
		name string
		o    bitgetPlanOrder
		want string
	}{
		{"one-way long: net + closing side sell", bitgetPlanOrder{PosSide: "net", Side: "sell"}, "long"},
		{"one-way short: net + closing side buy", bitgetPlanOrder{PosSide: "net", Side: "buy"}, "short"},
		{"one-way uppercase", bitgetPlanOrder{PosSide: "NET", Side: "SELL"}, "long"},
		{"hedge long ignores side", bitgetPlanOrder{PosSide: "long", Side: "buy"}, "long"},
		{"hedge long with side sell", bitgetPlanOrder{PosSide: "long", Side: "sell"}, "long"},
		{"hedge short ignores side", bitgetPlanOrder{PosSide: "short", Side: "sell"}, "short"},
		{"hedge short with side buy", bitgetPlanOrder{PosSide: "short", Side: "buy"}, "short"},
		{"legacy holdSide buy = long position", bitgetPlanOrder{HoldSide: "buy"}, "long"},
		{"legacy holdSide sell = short position", bitgetPlanOrder{HoldSide: "sell"}, "short"},
		{"missing posSide falls back to the closing side", bitgetPlanOrder{Side: "sell"}, "long"},
		{"nothing known", bitgetPlanOrder{PosSide: "net"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := planOrderDirection(tc.o); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBitgetFixtureCancelTPSLDirectionMatching(t *testing.T) {
	cancelled := func(f *fakeBitget) []string {
		var out []string
		for _, r := range f.find(bitgetCancelPlanPath) {
			for _, e := range r.Body["orderIdList"].([]interface{}) {
				out = append(out, e.(map[string]interface{})["orderId"].(string))
			}
		}
		return out
	}
	const longSL, longTP, shortSL = "1491757342466260992", "1491757344492109824", "1491757573371084800"

	tests := []struct {
		name      string
		fixture   string
		kinds     bitgetTPSLKind
		direction string
		want      []string
	}{
		// one cancel request per planType, in planType order: pos_loss (SL) before pos_profit (TP)
		{"long plans, long direction", "plan_pending", bitgetKindAll, "long", []string{longSL, longTP}},
		{"long plans, short direction cancels nothing", "plan_pending", bitgetKindAll, "short", nil},
		{"long plans, no direction", "plan_pending", bitgetKindAll, "", []string{longSL, longTP}},
		{"long plans, SL only", "plan_pending", bitgetKindSL, "long", []string{longSL}},
		{"long plans, TP only", "plan_pending", bitgetKindTP, "", []string{longTP}},
		{"short plan, short direction", "plan_pending_short", bitgetKindSL, "short", []string{shortSL}},
		{"short plan, long direction cancels nothing", "plan_pending_short", bitgetKindSL, "long", nil},
		{"after close (null list)", "plan_pending_after_close", bitgetKindAll, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitget(t, true)
			f.setFixture(bitgetPlanPendingPath, tc.fixture)
			if err := tr.cancelTPSL("BTCUSDT", tc.kinds, tc.direction); err != nil {
				t.Fatal(err)
			}
			got := cancelled(f)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("cancelled %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBitgetNullEntrustedListEverywhere(t *testing.T) {
	tr, f := newTestBitget(t, true)
	f.setFixture(bitgetPendingPath, "orders_pending")
	f.setFixture(bitgetPlanPendingPath, "plan_pending_after_close")

	for name, fn := range map[string]func() error{
		"CancelAllOrders":         func() error { return tr.CancelAllOrders("BTCUSDT") },
		"CancelStopOrders":        func() error { return tr.CancelStopOrders("BTCUSDT") },
		"CancelStopLossOrders":    func() error { return tr.CancelStopLossOrders("BTCUSDT") },
		"CancelTakeProfitOrders":  func() error { return tr.CancelTakeProfitOrders("BTCUSDT") },
		"listPlanOrders":          func() error { _, err := tr.listPlanOrders("BTCUSDT"); return err },
		"matchingPlanOrders":      func() error { _, err := tr.matchingPlanOrders("BTCUSDT", bitgetKindAll, "long"); return err },
		"GetOpenOrders (all sym)": func() error { _, err := tr.GetOpenOrders(""); return err },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s with null entrustedList: %v", name, err)
		}
	}
	if n := len(mutatingBitgetRequests(f)); n != 0 {
		t.Errorf("nothing pending, so nothing may be cancelled; mutating requests = %d", n)
	}
}

// ---------- order placement / detail ----------

func TestBitgetFixtureOpenCloseLongFlow(t *testing.T) {
	tr, f := newTestBitget(t, true)
	// BTCUSDT contract as on the real Demo exchange
	f.set(bitgetContractsPath, `[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","minTradeNum":"0.0001","priceEndStep":"1","volumePlace":"4","pricePlace":"1","sizeMultiplier":"0.0001","minTradeUSDT":"5","maxLever":"125","symbolStatus":"normal","isRwa":"NO","takerFeeRate":"0.0006","makerFeeRate":"0.0002","fundInterval":"8"}]`)
	f.set(bitgetTickerPath, `[{"symbol":"BTCUSDT","lastPr":"83344.1"}]`)
	f.setFixture(bitgetOrderDetailPath, "order_detail_close")

	// open long: real place-order response shape {clientOid, orderId}
	f.setFixture(bitgetOrderPath, "place_open_long")
	res, err := tr.OpenLong("BTCUSDT", 0.0001, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res["orderId"] != "1491757336292159489" {
		t.Errorf("open orderId = %v", res["orderId"])
	}
	lev := f.last(bitgetLeveragePath).Body
	if lev["symbol"] != "BTCUSDT" || lev["productType"] != "USDT-FUTURES" || lev["marginCoin"] != "USDT" || lev["leverage"] != "3" {
		t.Errorf("set-leverage body = %v", lev)
	}
	open := f.last(bitgetOrderPath).Body
	if open["side"] != "buy" || open["orderType"] != "market" || open["size"] != "0.0001" || open["marginMode"] != "crossed" ||
		open["marginCoin"] != "USDT" || open["productType"] != "USDT-FUTURES" {
		t.Errorf("open body = %v", open)
	}
	if _, has := open["tradeSide"]; has {
		t.Errorf("tradeSide must not be sent in one-way mode: %v", open)
	}
	if _, has := open["reduceOnly"]; has {
		t.Errorf("opening order must not be reduceOnly: %v", open)
	}
	if q := f.last(bitgetOrderDetailPath).Query; q["orderId"] != "1491757336292159489" {
		t.Errorf("detail must query the placed orderId, query = %v", q)
	}

	// close long: sell + reduceOnly YES
	f.setFixture(bitgetOrderPath, "place_close_long")
	res, err = tr.CloseLong("BTCUSDT", 0.0001)
	if err != nil {
		t.Fatal(err)
	}
	if res["orderId"] != "1491757402453110785" || res["status"] != "FILLED" ||
		res["avgPrice"].(float64) != 83346.1 || res["executedQty"].(float64) != 0.0001 {
		t.Errorf("close result = %v", res)
	}
	if c, _ := res["commission"].(float64); !approx(c, 0.00500076) {
		t.Errorf("commission = %v, want +0.00500076", res["commission"])
	}
	cl := f.last(bitgetOrderPath).Body
	if cl["side"] != "sell" || cl["reduceOnly"] != "YES" || cl["size"] != "0.0001" {
		t.Errorf("close body = %v", cl)
	}
	if _, has := cl["tradeSide"]; has {
		t.Errorf("tradeSide must not be sent in one-way mode: %v", cl)
	}
}

func TestBitgetFixtureGetOrderStatus(t *testing.T) {
	tr, f := newTestBitget(t, true)
	f.setFixture(bitgetOrderDetailPath, "order_detail_close")
	st, err := tr.GetOrderStatus("BTCUSDT", "1491757402453110785")
	if err != nil {
		t.Fatal(err)
	}
	if st["status"] != "FILLED" || st["side"] != "sell" || st["type"] != "market" ||
		st["orderId"] != "1491757402453110785" || st["symbol"] != "BTCUSDT" {
		t.Errorf("status = %v", st)
	}
	if st["avgPrice"].(float64) != 83346.1 || st["executedQty"].(float64) != 0.0001 {
		t.Errorf("avg/qty = %v / %v", st["avgPrice"], st["executedQty"])
	}
	// the exchange reports fee as a NEGATIVE cost (-0.00500076); commission is its absolute value
	if c := st["commission"].(float64); !approx(c, 0.00500076) {
		t.Errorf("commission = %v, want 0.00500076", c)
	}
	if st["time"].(int64) != 1791388600934 || st["updateTime"].(int64) != 1791388600990 {
		t.Errorf("times = %v / %v", st["time"], st["updateTime"])
	}
}

// ---------- history position / closed PnL ----------

func TestBitgetFixtureGetClosedPnL(t *testing.T) {
	tr, f := newTestBitget(t, true)
	f.setFixture(bitgetHistoryPosPath, "history_position")
	recs, err := tr.GetClosedPnL(time.UnixMilli(1790000000000), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	r := recs[0]
	if r.Symbol != "BTCUSDT" || r.Side != "long" || r.ExchangeID != "1491757336539623426" ||
		r.EntryPrice != 83343.3 || r.ExitPrice != 83346.1 || r.Quantity != 0.0001 {
		t.Errorf("record 0 = %+v", r)
	}
	// pnl is the gross price PnL, fees (open + close, both negative on the wire) are separate
	if !approx(r.RealizedPnL, 0.00028) || !approx(r.Fee, 0.00500059+0.00500076) {
		t.Errorf("pnl/fee = %v / %v", r.RealizedPnL, r.Fee)
	}
	// the real payload spells the timestamps lower-case (ctime/utime)
	if r.EntryTime.UnixMilli() != 1791388585219 || r.ExitTime.UnixMilli() != 1791388600991 {
		t.Errorf("times = %v / %v", r.EntryTime.UnixMilli(), r.ExitTime.UnixMilli())
	}
	if r.CloseType != "unknown" {
		t.Errorf("closeType = %q (no reason reported)", r.CloseType)
	}
	if recs[1].RealizedPnL >= 0 || recs[2].Quantity != 0.0123 {
		t.Errorf("records 1/2 = %+v / %+v", recs[1], recs[2])
	}
}

// ---------- fills / order sync ----------

func TestBitgetFixtureGetTrades(t *testing.T) {
	tr, f := newTestBitget(t, true)
	_, _, fills := bitgetFixture(t, "fills")
	// page 2 (idLessThan = endId) is empty: the history is exhausted
	f.setSeq(bitgetFillsPath, string(fills), `{"fillList":null,"endId":null}`)

	trades, err := tr.GetTrades(time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 6 {
		t.Fatalf("trades = %d", len(trades))
	}

	reqs := f.find(bitgetFillsPath)
	if len(reqs) != 2 {
		t.Fatalf("fills requests = %d, want 2 (page + empty page)", len(reqs))
	}
	q := reqs[0].Query
	if q["productType"] != "USDT-FUTURES" || q["limit"] != "100" || q["startTime"] == "" || q["endTime"] == "" || q["idLessThan"] != "" {
		t.Errorf("page 1 query = %v", q)
	}
	if reqs[1].Query["idLessThan"] != "1488104377563721728" {
		t.Errorf("page 2 must page with idLessThan=endId, query = %v", reqs[1].Query)
	}
	if len(f.find("/api/v2/mix/order/fill-history")) != 0 {
		t.Errorf("fill-history does not exist and must never be called")
	}

	byID := map[string]BitgetTrade{}
	for _, tt := range trades {
		byID[tt.TradeID] = tt
	}
	// one-way close of a long: sell_single, profit 0.00028
	cl := byID["1491757402652127232"]
	if cl.Symbol != "BTCUSDT" || cl.OrderID != "1491757402453110785" || cl.Side != "sell" || cl.TradeSide != "sell_single" ||
		cl.FillPrice != 83346.1 || cl.FillQty != 0.0001 || !approx(cl.ProfitLoss, 0.00028) ||
		cl.OrderAction != "close_long" || cl.ActionAmbiguous || cl.IsMaker || cl.FeeAsset != "USDT" ||
		cl.ExecTime.UnixMilli() != 1791388600981 {
		t.Errorf("close fill = %+v", cl)
	}
	if !approx(cl.Fee, 0.00500076) {
		t.Errorf("fee = %v, want |totalFee| = 0.00500076", cl.Fee)
	}
	// one-way open of a long: buy_single, profit 0 -> ambiguous open (resolved against positions in the sync)
	op := byID["1491757336495370240"]
	if op.Side != "buy" || op.TradeSide != "buy_single" || op.OrderAction != "open_long" || !op.ActionAmbiguous || op.ProfitLoss != 0 {
		t.Errorf("open fill = %+v", op)
	}
	// older hedge-mode fills: side is the position direction, so buy+close closes a LONG
	for _, id := range []string{"1488119480577142784", "1488104377563721728"} {
		h := byID[id]
		if h.TradeSide != "close" || h.OrderAction != "close_long" || h.ActionAmbiguous || h.ProfitLoss <= 0 {
			t.Errorf("hedge fill %s = %+v", id, h)
		}
	}
}

// bitgetFillsFixture returns the real fills as generic maps (newest first, as served).
func bitgetFillsFixture(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, _, data := bitgetFixture(t, "fills")
	var resp struct {
		FillList []map[string]interface{} `json:"fillList"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.FillList
}

// derivedBitgetFill clones a real fill and overrides fields (used for the short leg, which the
// captured sequence does not contain; the shape is exactly the real one, see fills.json).
func derivedBitgetFill(base map[string]interface{}, over map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// The real one-way sequence open long -> close long -> open long -> close long, extended with
// open short -> close short (derived from the real shape), pushed through the whole sync into a
// real store. tradeSide never says open/close in one-way mode, so classification relies on
// realized profit and on the positions already in the store.
func TestSyncOrdersFromBitgetRealFillSequence(t *testing.T) {
	for _, breakEven := range []bool{false, true} {
		name := "profits as captured"
		if breakEven {
			name = "all closes break-even (profit 0)"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.New(filepath.Join(t.TempDir(), "sync.db"))
			if err != nil {
				t.Fatal(err)
			}
			tr, f := newTestBitget(t, true)

			rf := bitgetFillsFixture(t) // [close#2, open#2, close#1, open#1, hedge close, hedge close]
			if len(rf) != 6 {
				t.Fatalf("fixture fills = %d", len(rf))
			}
			const (
				shortOpenID  = "9000000000000000001"
				shortCloseID = "9000000000000000002"
			)
			shortOpen := derivedBitgetFill(rf[3], map[string]interface{}{
				"tradeId": shortOpenID, "orderId": "9000000000000000011",
				"side": "sell", "tradeSide": "sell_single", "price": "83350", "profit": "0", "cTime": "1791388610000",
			})
			shortClose := derivedBitgetFill(rf[2], map[string]interface{}{
				"tradeId": shortCloseID, "orderId": "9000000000000000012",
				"side": "buy", "tradeSide": "buy_single", "price": "83340.5", "profit": "0.00095", "cTime": "1791388620000",
			})
			if breakEven {
				rf[0] = derivedBitgetFill(rf[0], map[string]interface{}{"profit": "0"})
				rf[2] = derivedBitgetFill(rf[2], map[string]interface{}{"profit": "0"})
				shortClose = derivedBitgetFill(shortClose, map[string]interface{}{"profit": "0"})
			}
			list := append([]map[string]interface{}{shortClose, shortOpen}, rf...) // newest first
			page, _ := json.Marshal(map[string]interface{}{
				"fillList": list,
				"endId":    rf[5]["tradeId"],
			})
			f.setSeq(bitgetFillsPath, string(page), `{"fillList":null,"endId":null}`)

			if err := tr.SyncOrdersFromBitget("trader-1", "exch-1", "bitget", st); err != nil {
				t.Fatal(err)
			}

			orders, err := st.Order().GetTraderOrders("trader-1", 50)
			if err != nil {
				t.Fatal(err)
			}
			actions := map[string]string{}
			for _, o := range orders {
				actions[o.ExchangeOrderID] = o.OrderAction
			}
			want := map[string]string{
				"1491757237266526208": "open_long",   // open #1
				"1491757246619824128": "close_long",  // close #1
				"1491757336495370240": "open_long",   // open #2
				"1491757402652127232": "close_long",  // close #2
				shortOpenID:           "open_short",  // sell_single, nothing open
				shortCloseID:          "close_short", // buy_single reduces the open short
				"1488104377563721728": "close_long",  // hedge-mode buy+close
				"1488119480577142784": "close_long",
			}
			if len(actions) != len(want) {
				t.Errorf("orders = %d, want %d: %v", len(actions), len(want), actions)
			}
			for id, w := range want {
				if actions[id] != w {
					t.Errorf("trade %s action = %q, want %q", id, actions[id], w)
				}
			}

			// no phantom open positions: everything opened was closed again
			for _, side := range []string{"LONG", "SHORT"} {
				if pos, _ := st.Position().GetOpenPositionBySymbol("trader-1", "BTCUSDT", side); pos != nil {
					t.Errorf("open %s position left over: %+v", side, pos)
				}
			}
			closed, err := st.Position().GetClosedPositions("trader-1", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(closed) != 3 {
				t.Fatalf("closed positions = %d, want 3 (2 long + 1 short): %+v", len(closed), closed)
			}
			byEntry := map[string]*store.TraderPosition{}
			for _, p := range closed {
				byEntry[p.EntryOrderID] = p
			}
			wantPos := []struct {
				entryID, side string
				entry, exit   float64
				pnl           float64
			}{
				{"1491757237266526208", "LONG", 83367, 83362.1, -0.00049},
				{"1491757336495370240", "LONG", 83343.3, 83346.1, 0.00028},
				{shortOpenID, "SHORT", 83350, 83340.5, 0.00095},
			}
			for _, w := range wantPos {
				p := byEntry[w.entryID]
				if p == nil {
					t.Errorf("no closed position for entry %s", w.entryID)
					continue
				}
				if p.Side != w.side || p.Symbol != "BTCUSDT" || p.EntryPrice != w.entry || math.Abs(p.ExitPrice-w.exit) > 0.011 {
					t.Errorf("position %s = %+v", w.entryID, p)
				}
				if !breakEven && math.Abs(p.RealizedPnL-w.pnl) > 1e-9 {
					t.Errorf("position %s pnl = %v, want %v", w.entryID, p.RealizedPnL, w.pnl)
				}
				if p.Status != "CLOSED" || p.Fee <= 0.0099 {
					t.Errorf("position %s status/fee = %s/%v (open fee + close fee expected)", w.entryID, p.Status, p.Fee)
				}
			}

			// fill rows carry real fees / scope; a re-run must be idempotent
			fill, err := st.Order().GetFillByExchangeTradeID("exch-1", "1491757402652127232")
			if err != nil || fill == nil {
				t.Fatalf("fill: %v %v", fill, err)
			}
			if !approx(fill.Commission, 0.00500076) || fill.IsMaker || fill.CommissionAsset != "USDT" || fill.Price != 83346.1 ||
				fill.ExchangeOrderID != "1491757402453110785" {
				t.Errorf("fill = %+v", fill)
			}
			if err := tr.SyncOrdersFromBitget("trader-1", "exch-1", "bitget", st); err != nil {
				t.Fatal(err)
			}
			orders2, _ := st.Order().GetTraderOrders("trader-1", 50)
			closed2, _ := st.Position().GetClosedPositions("trader-1", 10)
			if len(orders2) != len(orders) || len(closed2) != len(closed) {
				t.Errorf("re-sync must be idempotent: orders %d->%d, closed %d->%d", len(orders), len(orders2), len(closed), len(closed2))
			}
			if open, _ := st.Position().GetOpenPositionBySymbol("trader-1", "BTCUSDT", "SHORT"); open != nil {
				t.Errorf("re-sync created a phantom short: %+v", open)
			}
		})
	}
}
