package trader

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- fake Bitget server ----------

type bitgetRecReq struct {
	Method string
	Path   string
	Query  map[string]string
	Body   map[string]interface{}
	Header http.Header
}

type fakeBitget struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []bitgetRecReq
	data     map[string]string // path -> JSON data
	errCodes map[string][2]string
}

const bitgetTestContracts = `[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","minTradeNum":"0.001","priceEndStep":"5","volumePlace":"3","pricePlace":"1","sizeMultiplier":"0.001","minTradeUSDT":"5","maxLever":"125","symbolStatus":"normal","isRwa":"NO","takerFeeRate":"0.0006","makerFeeRate":"0.0002","fundInterval":"8","maxOrderQty":"1200"}]`

func newFakeBitget(t *testing.T) *fakeBitget {
	f := &fakeBitget{
		t:        t,
		data:     map[string]string{},
		errCodes: map[string][2]string{},
	}
	f.data[bitgetContractsPath] = bitgetTestContracts
	f.data[bitgetTickerPath] = `[{"symbol":"BTCUSDT","lastPr":"50000"}]`
	f.data[bitgetLeveragePath] = `{}`
	f.data[bitgetMarginModePath] = `{}`
	f.data[bitgetOrderPath] = `{"orderId":"111","clientOid":"x"}`
	f.data[bitgetOrderDetailPath] = `{"orderId":"111","state":"filled","priceAvg":"50000.5","baseVolume":"0.001","fee":"-0.03","side":"buy","orderType":"market","cTime":"1700000000000","uTime":"1700000000500"}`
	f.data[bitgetPlanPendingPath] = `{"entrustedList":null}`
	f.data[bitgetPlaceTPSLPath] = `{"orderId":"900"}`
	f.data[bitgetCancelPlanPath] = `{"successList":[],"failureList":[]}`
	f.data[bitgetPendingPath] = `{"entrustedList":null}`
	f.data[bitgetCancelOrderPath] = `{"orderId":"1"}`

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := bitgetRecReq{Method: r.Method, Path: r.URL.Path, Query: map[string]string{}, Header: r.Header.Clone()}
		for k, v := range r.URL.Query() {
			rec.Query[k] = v[0]
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.Body)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		data, ok := f.data[r.URL.Path]
		ec, isErr := f.errCodes[r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isErr {
			_, _ = w.Write([]byte(`{"code":"` + ec[0] + `","msg":"` + ec[1] + `","data":null}`))
			return
		}
		if !ok {
			_, _ = w.Write([]byte(`{"code":"40404","msg":"not found: ` + r.URL.Path + `","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1,"data":` + data + `}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBitget) set(path, data string) {
	f.mu.Lock()
	f.data[path] = data
	f.mu.Unlock()
}

func (f *fakeBitget) fail(path, code, msg string) {
	f.mu.Lock()
	f.errCodes[path] = [2]string{code, msg}
	f.mu.Unlock()
}

// find returns all recorded requests for a path
func (f *fakeBitget) find(path string) []bitgetRecReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bitgetRecReq
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeBitget) last(path string) bitgetRecReq {
	f.t.Helper()
	rs := f.find(path)
	if len(rs) == 0 {
		f.t.Fatalf("no request recorded for %s", path)
	}
	return rs[len(rs)-1]
}

func newTestBitget(t *testing.T, demo bool) (*BitgetTrader, *fakeBitget) {
	f := newFakeBitget(t)
	tr := newBitgetTraderCore("test-key", "test-secret", "test-pass", demo)
	tr.baseURL = f.srv.URL
	tr.orderPollInterval = time.Millisecond
	return tr, f
}

// ---------- order placement ----------

func TestBitgetOrderSides(t *testing.T) {
	tests := []struct {
		name       string
		call       func(tr *BitgetTrader) (map[string]interface{}, error)
		side       string
		reduceOnly bool
	}{
		{"OpenLong", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenLong("btcusdt", 0.0015, 5) }, "buy", false},
		{"OpenShort", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.OpenShort("BTCUSDT", 0.0015, 5) }, "sell", false},
		{"CloseLong", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseLong("BTCUSDT", 0.0015) }, "sell", true},
		{"CloseShort", func(tr *BitgetTrader) (map[string]interface{}, error) { return tr.CloseShort("BTCUSDT", 0.0015) }, "buy", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitget(t, false)
			res, err := tc.call(tr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			req := f.last(bitgetOrderPath)
			if req.Method != "POST" {
				t.Errorf("method = %s", req.Method)
			}
			b := req.Body
			if b["side"] != tc.side {
				t.Errorf("side = %v, want %s", b["side"], tc.side)
			}
			if _, has := b["tradeSide"]; has {
				t.Errorf("tradeSide must not be sent in one-way mode: %v", b)
			}
			ro, hasRO := b["reduceOnly"]
			if tc.reduceOnly && ro != "YES" {
				t.Errorf("reduceOnly = %v, want YES", ro)
			}
			if !tc.reduceOnly && hasRO {
				t.Errorf("reduceOnly must be absent on open, got %v", ro)
			}
			if b["symbol"] != "BTCUSDT" || b["productType"] != "USDT-FUTURES" || b["marginCoin"] != "USDT" ||
				b["orderType"] != "market" || b["marginMode"] != "crossed" {
				t.Errorf("unexpected body: %v", b)
			}
			// 0.0015 floored to 0.001 step, 3 decimals
			if b["size"] != "0.001" {
				t.Errorf("size = %v, want 0.001", b["size"])
			}
			if res["orderId"] != "111" || res["status"] != "FILLED" {
				t.Errorf("result = %v", res)
			}
			if res["avgPrice"].(float64) != 50000.5 || res["executedQty"].(float64) != 0.001 {
				t.Errorf("fill data not propagated: %v", res)
			}
			// OpenLong/Short must not cancel pending orders first
			if len(f.find(bitgetPendingPath)) != 0 || len(f.find(bitgetCancelOrderPath)) != 0 {
				t.Errorf("open must not call CancelAllOrders")
			}
		})
	}
}

func TestBitgetMarginModeFromSetMarginMode(t *testing.T) {
	tr, f := newTestBitget(t, false)
	if err := tr.SetMarginMode("BTCUSDT", false); err != nil {
		t.Fatal(err)
	}
	mm := f.last(bitgetMarginModePath).Body
	if mm["marginMode"] != "isolated" {
		t.Errorf("set-margin-mode body = %v", mm)
	}
	if _, err := tr.OpenLong("BTCUSDT", 0.002, 3); err != nil {
		t.Fatal(err)
	}
	if got := f.last(bitgetOrderPath).Body["marginMode"]; got != "isolated" {
		t.Errorf("order marginMode = %v, want isolated", got)
	}
	// other symbol keeps default
	if tr.marginModeFor("ETHUSDT") != "crossed" {
		t.Errorf("default margin mode should be crossed")
	}
	if _, err := tr.CloseLong("BTCUSDT", 0.002); err != nil {
		t.Fatal(err)
	}
	if got := f.last(bitgetOrderPath).Body["marginMode"]; got != "isolated" {
		t.Errorf("close marginMode = %v, want isolated", got)
	}
}

func TestBitgetSetLeverageFailureAbortsOpen(t *testing.T) {
	for _, name := range []string{"long", "short"} {
		t.Run(name, func(t *testing.T) {
			tr, f := newTestBitget(t, false)
			f.fail(bitgetLeveragePath, "40808", "bad leverage")
			var err error
			if name == "long" {
				_, err = tr.OpenLong("BTCUSDT", 0.002, 200)
			} else {
				_, err = tr.OpenShort("BTCUSDT", 0.002, 200)
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if len(f.find(bitgetOrderPath)) != 0 {
				t.Errorf("no order must be placed when leverage fails")
			}
		})
	}
}

func TestBitgetCloseWithZeroQuantityUsesPosition(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetPositionPath, `[{"symbol":"BTCUSDT","holdSide":"short","openPriceAvg":"50000","markPrice":"49000","total":"0.004","unrealizedPL":"4","leverage":"5","liquidationPrice":"60000","cTime":"1","uTime":"2"}]`)
	if _, err := tr.CloseShort("BTCUSDT", 0); err != nil {
		t.Fatal(err)
	}
	b := f.last(bitgetOrderPath).Body
	if b["side"] != "buy" || b["size"] != "0.004" || b["reduceOnly"] != "YES" {
		t.Errorf("body = %v", b)
	}
	if _, err := tr.CloseLong("BTCUSDT", 0); err == nil {
		t.Errorf("expected error when no long position exists")
	}
}

func TestBitgetOrderBelowMinimumRejected(t *testing.T) {
	tr, f := newTestBitget(t, false)
	if _, err := tr.OpenLong("BTCUSDT", 0.0004, 3); err == nil {
		t.Fatal("expected min size error")
	}
	if len(f.find(bitgetOrderPath)) != 0 {
		t.Errorf("order must not be sent")
	}
}

func TestBitgetUnfilledOrderStatus(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetOrderDetailPath, `{"orderId":"111","state":"live","priceAvg":"","baseVolume":"0","fee":"0"}`)
	res, err := tr.OpenLong("BTCUSDT", 0.002, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "NEW" {
		t.Errorf("status = %v, want NEW", res["status"])
	}
	if n := len(f.find(bitgetOrderDetailPath)); n != 3 {
		t.Errorf("detail polled %d times, want 3", n)
	}
}

// ---------- TP / SL ----------

func TestBitgetSetStopLossTakeProfit(t *testing.T) {
	tests := []struct {
		name     string
		call     func(tr *BitgetTrader) error
		planType string
		holdSide string
		trigger  string
	}{
		{"SL long", func(tr *BitgetTrader) error { return tr.SetStopLoss("BTCUSDT", "LONG", 0.01, 49123.37) }, "pos_loss", "buy", "49123.5"},
		{"SL short", func(tr *BitgetTrader) error { return tr.SetStopLoss("BTCUSDT", "SHORT", 0.01, 51000.1) }, "pos_loss", "sell", "51000.0"},
		{"TP long", func(tr *BitgetTrader) error { return tr.SetTakeProfit("BTCUSDT", "long", 0.01, 52000.74) }, "pos_profit", "buy", "52000.5"},
		{"TP short", func(tr *BitgetTrader) error { return tr.SetTakeProfit("BTCUSDT", "short", 0.01, 47999.8) }, "pos_profit", "sell", "48000.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, f := newTestBitget(t, false)
			if err := tc.call(tr); err != nil {
				t.Fatal(err)
			}
			req := f.last(bitgetPlaceTPSLPath)
			if req.Method != "POST" {
				t.Errorf("method = %s", req.Method)
			}
			b := req.Body
			if b["planType"] != tc.planType || b["holdSide"] != tc.holdSide || b["triggerType"] != "mark_price" ||
				b["triggerPrice"] != tc.trigger || b["symbol"] != "BTCUSDT" || b["productType"] != "USDT-FUTURES" ||
				b["marginCoin"] != "USDT" {
				t.Errorf("body = %v", b)
			}
			if _, has := b["size"]; has {
				t.Errorf("size must be omitted for pos_* plans")
			}
			if _, has := b["executePrice"]; has {
				t.Errorf("executePrice must be omitted (market)")
			}
		})
	}
}

func TestBitgetSetStopLossReplacesExistingSameType(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetPlanPendingPath, `{"entrustedList":[
		{"orderId":"1","planType":"pos_loss","triggerPrice":"48000","side":"sell","posSide":"net","symbol":"BTCUSDT"},
		{"orderId":"2","planType":"pos_profit","triggerPrice":"52000","side":"sell","posSide":"net","symbol":"BTCUSDT"}]}`)
	if err := tr.SetStopLoss("BTCUSDT", "LONG", 0, 49000); err != nil {
		t.Fatal(err)
	}
	q := f.last(bitgetPlanPendingPath).Query
	if q["planType"] != "profit_loss" || q["symbol"] != "BTCUSDT" || q["productType"] != "USDT-FUTURES" {
		t.Errorf("plan pending query = %v", q)
	}
	cancel := f.last(bitgetCancelPlanPath).Body
	list, _ := cancel["orderIdList"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["orderId"] != "1" {
		t.Errorf("must cancel only the existing SL: %v", cancel)
	}
	if cancel["planType"] != "profit_loss" || cancel["marginCoin"] != "USDT" || cancel["symbol"] != "BTCUSDT" {
		t.Errorf("cancel body = %v", cancel)
	}
	// cancel precedes place
	f.mu.Lock()
	order := []string{}
	for _, r := range f.reqs {
		if r.Path == bitgetCancelPlanPath || r.Path == bitgetPlaceTPSLPath {
			order = append(order, r.Path)
		}
	}
	f.mu.Unlock()
	if len(order) != 2 || order[0] != bitgetCancelPlanPath {
		t.Errorf("call order = %v", order)
	}
}

func TestBitgetCancelFlow(t *testing.T) {
	plans := `{"entrustedList":[
		{"orderId":"1","planType":"pos_loss","symbol":"BTCUSDT"},
		{"orderId":"2","planType":"pos_profit","symbol":"BTCUSDT"},
		{"orderId":"3","planType":"normal_plan","symbol":"BTCUSDT"}]}`

	ids := func(r bitgetRecReq) []string {
		var out []string
		for _, e := range r.Body["orderIdList"].([]interface{}) {
			out = append(out, e.(map[string]interface{})["orderId"].(string))
		}
		return out
	}

	t.Run("stop loss only", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetPlanPendingPath, plans)
		if err := tr.CancelStopLossOrders("BTCUSDT"); err != nil {
			t.Fatal(err)
		}
		if got := ids(f.last(bitgetCancelPlanPath)); len(got) != 1 || got[0] != "1" {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("take profit only", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetPlanPendingPath, plans)
		if err := tr.CancelTakeProfitOrders("BTCUSDT"); err != nil {
			t.Fatal(err)
		}
		if got := ids(f.last(bitgetCancelPlanPath)); len(got) != 1 || got[0] != "2" {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("stop orders", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetPlanPendingPath, plans)
		if err := tr.CancelStopOrders("BTCUSDT"); err != nil {
			t.Fatal(err)
		}
		if got := ids(f.last(bitgetCancelPlanPath)); len(got) != 2 {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("no pending is a noop", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		if err := tr.CancelStopOrders("BTCUSDT"); err != nil {
			t.Fatal(err)
		}
		if len(f.find(bitgetCancelPlanPath)) != 0 {
			t.Errorf("no cancel expected")
		}
	})
	t.Run("failure list surfaces as error", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetPlanPendingPath, plans)
		f.set(bitgetCancelPlanPath, `{"successList":[],"failureList":[{"orderId":"1","errorMsg":"boom"}]}`)
		err := tr.CancelStopLossOrders("BTCUSDT")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("cancel all cancels regular and plan orders and aggregates errors", func(t *testing.T) {
		tr, f := newTestBitget(t, false)
		f.set(bitgetPendingPath, `{"entrustedList":[{"orderId":"10"},{"orderId":"11"}]}`)
		f.set(bitgetPlanPendingPath, plans)
		f.fail(bitgetCancelOrderPath, "43001", "order not exist")
		err := tr.CancelAllOrders("BTCUSDT")
		if err == nil || !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "11") {
			t.Errorf("expected aggregated error, got %v", err)
		}
		if n := len(f.find(bitgetCancelOrderPath)); n != 2 {
			t.Errorf("cancel-order calls = %d", n)
		}
		if len(f.find(bitgetCancelPlanPath)) != 1 {
			t.Errorf("plan orders must still be cancelled")
		}
	})
}

// ---------- headers / signing ----------

func TestBitgetHeaders(t *testing.T) {
	for _, demo := range []bool{true, false} {
		tr, f := newTestBitget(t, demo)
		if _, err := tr.GetMarketPrice("BTCUSDT"); err != nil {
			t.Fatal(err)
		}
		h := f.last(bitgetTickerPath).Header
		for _, k := range []string{"ACCESS-KEY", "ACCESS-SIGN", "ACCESS-TIMESTAMP", "ACCESS-PASSPHRASE"} {
			if h.Get(k) == "" {
				t.Errorf("demo=%v missing header %s", demo, k)
			}
		}
		if h.Get("ACCESS-KEY") != "test-key" || h.Get("ACCESS-PASSPHRASE") != "test-pass" {
			t.Errorf("wrong credentials headers")
		}
		got := h.Get("paptrading")
		if demo && got != "1" {
			t.Errorf("paptrading = %q, want 1", got)
		}
		if !demo && got != "" {
			t.Errorf("paptrading must be absent for live, got %q", got)
		}
	}
}

func TestBitgetSignature(t *testing.T) {
	tr := newBitgetTraderCore("k", "secret", "p", false)
	a := tr.sign("1700000000000", "GET", "/x?a=1", "")
	b := tr.sign("1700000000000", "GET", "/x?a=1", "")
	if a == "" || a != b {
		t.Errorf("signature must be deterministic and non-empty")
	}
	if a == tr.sign("1700000000001", "GET", "/x?a=1", "") {
		t.Errorf("signature must depend on timestamp")
	}
}

func TestBitgetAPIErrorHints(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.fail(bitgetOrderPath, "40774", "The order type for unilateral position must also be the unilateral position type.")
	_, err := tr.OpenLong("BTCUSDT", 0.002, 3)
	if err == nil || !strings.Contains(err.Error(), "40774") || !strings.Contains(err.Error(), "one-way") {
		t.Errorf("err = %v", err)
	}
	for _, c := range []string{"40774", "45110", "40762", "43012", "40808", "40917", "45122", "22002"} {
		if bitgetErrorHint(c) == "" {
			t.Errorf("no hint for %s", c)
		}
	}
}

// ---------- queries ----------

func TestBitgetGetOpenOrders(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetPendingPath, `{"entrustedList":[
		{"orderId":"20","symbol":"BTCUSDT","status":"live","side":"buy","posSide":"net","tradeSide":"","orderType":"limit","price":"49000","size":"0.01","reduceOnly":"NO"},
		{"orderId":"21","symbol":"BTCUSDT","status":"partially_filled","side":"sell","posSide":"net","orderType":"limit","price":"53000","size":"0.02","reduceOnly":"YES"}]}`)
	f.set(bitgetPlanPendingPath, `{"entrustedList":[
		{"orderId":"30","planType":"pos_loss","triggerPrice":"48000","side":"buy","posSide":"long","size":"0.01","symbol":"BTCUSDT","cTime":"1"},
		{"orderId":"31","planType":"pos_profit","triggerPrice":"47000","side":"sell","posSide":"short","size":"0.01","symbol":"BTCUSDT","cTime":"1"},
		{"orderId":"32","planType":"normal_plan","triggerPrice":"1","side":"buy","symbol":"BTCUSDT"}]}`)

	orders, err := tr.GetOpenOrders("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 4 {
		t.Fatalf("got %d orders: %+v", len(orders), orders)
	}
	byID := map[string]OpenOrder{}
	for _, o := range orders {
		byID[o.OrderID] = o
	}
	if o := byID["20"]; o.Side != "BUY" || o.PositionSide != "LONG" || o.Type != "LIMIT" || o.Price != 49000 || o.Quantity != 0.01 || o.Status != "NEW" {
		t.Errorf("order 20 = %+v", o)
	}
	if o := byID["21"]; o.Side != "SELL" || o.PositionSide != "LONG" || o.Status != "PARTIALLY_FILLED" {
		t.Errorf("order 21 = %+v", o)
	}
	if o := byID["30"]; o.Type != "STOP_MARKET" || o.StopPrice != 48000 || o.PositionSide != "LONG" || o.Side != "SELL" {
		t.Errorf("order 30 = %+v", o)
	}
	if o := byID["31"]; o.Type != "TAKE_PROFIT_MARKET" || o.StopPrice != 47000 || o.PositionSide != "SHORT" || o.Side != "BUY" {
		t.Errorf("order 31 = %+v", o)
	}
	if q := f.last(bitgetPendingPath).Query; q["symbol"] != "BTCUSDT" || q["productType"] != "USDT-FUTURES" {
		t.Errorf("query = %v", q)
	}
}

func TestBitgetGetOrderStatus(t *testing.T) {
	cases := map[string]string{
		"live": "NEW", "partially_filled": "PARTIALLY_FILLED", "filled": "FILLED",
		"canceled": "CANCELED", "cancelled": "CANCELED",
	}
	for state, want := range cases {
		tr, f := newTestBitget(t, false)
		f.set(bitgetOrderDetailPath, `{"orderId":"9","state":"`+state+`","priceAvg":"123.4","baseVolume":"2","fee":"-0.5","side":"sell","orderType":"market"}`)
		st, err := tr.GetOrderStatus("BTCUSDT", "9")
		if err != nil {
			t.Fatal(err)
		}
		if st["status"] != want {
			t.Errorf("state %s -> %v, want %s", state, st["status"], want)
		}
		if st["avgPrice"].(float64) != 123.4 || st["executedQty"].(float64) != 2 || st["commission"].(float64) != 0.5 {
			t.Errorf("fields = %v", st)
		}
		q := f.last(bitgetOrderDetailPath).Query
		if q["orderId"] != "9" || q["symbol"] != "BTCUSDT" || q["productType"] != "USDT-FUTURES" {
			t.Errorf("query = %v", q)
		}
	}
}

func TestBitgetGetClosedPnL(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetHistoryPosPath, `{"list":[{"positionId":"p1","symbol":"BTCUSDT","holdSide":"long","openAvgPrice":"50000","closeAvgPrice":"51000","openTotalPos":"0.01","closeTotalPos":"0.01","pnl":"10","netProfit":"9.2","totalFunding":"-0.1","openFee":"-0.3","closeFee":"-0.4","cTime":"1700000000000","uTime":"1700003600000"}],"endId":"p1"}`)
	start := time.UnixMilli(1699990000000)
	recs, err := tr.GetClosedPnL(start, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("len = %d", len(recs))
	}
	r := recs[0]
	if r.Symbol != "BTCUSDT" || r.Side != "long" || r.EntryPrice != 50000 || r.ExitPrice != 51000 ||
		r.Quantity != 0.01 || r.RealizedPnL != 10 || math.Abs(r.Fee-0.7) > 1e-9 || r.ExchangeID != "p1" {
		t.Errorf("record = %+v", r)
	}
	if r.EntryTime.UnixMilli() != 1700000000000 || r.ExitTime.UnixMilli() != 1700003600000 {
		t.Errorf("times = %v %v", r.EntryTime, r.ExitTime)
	}
	q := f.last(bitgetHistoryPosPath).Query
	if q["limit"] != "100" || q["startTime"] != "1699990000000" || q["productType"] != "USDT-FUTURES" || q["endTime"] == "" {
		t.Errorf("query = %v", q)
	}
}

func TestBitgetGetBalanceAndPositionsKeys(t *testing.T) {
	tr, f := newTestBitget(t, false)
	f.set(bitgetAccountPath, `[{"marginCoin":"USDT","available":"900","accountEquity":"1010","unrealizedPL":"10"}]`)
	f.set(bitgetPositionPath, `[{"symbol":"BTCUSDT","holdSide":"long","openPriceAvg":"50000","markPrice":"50100","total":"0.01","unrealizedPL":"1","leverage":"5","liquidationPrice":"40000","cTime":"1","uTime":"2"}]`)
	bal, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"totalWalletBalance", "availableBalance", "totalUnrealizedProfit", "totalEquity"} {
		if _, ok := bal[k].(float64); !ok {
			t.Errorf("balance key %s missing/not float64", k)
		}
	}
	pos, err := tr.GetPositions()
	if err != nil || len(pos) != 1 {
		t.Fatalf("pos=%v err=%v", pos, err)
	}
	for _, k := range []string{"positionAmt", "entryPrice", "markPrice", "unRealizedProfit", "leverage", "liquidationPrice"} {
		if _, ok := pos[0][k].(float64); !ok {
			t.Errorf("position key %s missing/not float64", k)
		}
	}
	if pos[0]["side"] != "long" {
		t.Errorf("side = %v", pos[0]["side"])
	}
}

// ---------- precision ----------

func TestBitgetFormatQuantity(t *testing.T) {
	tr, f := newTestBitget(t, false)
	tests := []struct {
		qty     float64
		want    string
		wantErr bool
	}{
		{0.0019999, "0.001", false},
		{0.0021, "0.002", false},
		{0.003, "0.003", false}, // float epsilon: 0.003/0.001 = 2.9999999999999996
		{1.23456, "1.234", false},
		{0.0009, "", true},
		{0, "", true},
	}
	for _, tc := range tests {
		got, err := tr.FormatQuantity("BTCUSDT", tc.qty)
		if tc.wantErr {
			if err == nil {
				t.Errorf("qty %v: expected error, got %q", tc.qty, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("qty %v: got %q err %v, want %q", tc.qty, got, err, tc.want)
		}
	}

	// below minTradeUSDT: 0.001 * 1000 = 1 USDT < 5
	f.set(bitgetTickerPath, `[{"symbol":"BTCUSDT","lastPr":"1000"}]`)
	if _, err := tr.FormatQuantity("BTCUSDT", 0.001); err == nil {
		t.Errorf("expected min notional error")
	}

	// unknown contract is an error, not a silent fallback
	f.set(bitgetContractsPath, `[]`)
	if _, err := tr.FormatQuantity("NOPEUSDT", 1); err == nil {
		t.Errorf("expected contract-not-found error")
	}
}

func TestBitgetContractInfoAndCache(t *testing.T) {
	tr, f := newTestBitget(t, false)
	c, err := tr.GetContractInfo("btcusdt")
	if err != nil {
		t.Fatal(err)
	}
	if c.MinTradeNum != 0.001 || c.SizeMultiplier != 0.001 || c.PricePlace != 1 || c.PriceEndStep != 5 ||
		c.VolumePlace != 3 || c.MinTradeUSDT != 5 || c.MaxLever != 125 || c.SymbolStatus != "normal" || c.IsRwa ||
		c.TakerFeeRate != 0.0006 || c.MakerFeeRate != 0.0002 {
		t.Errorf("contract = %+v", c)
	}
	if math.Abs(c.PriceTick()-0.5) > 1e-12 {
		t.Errorf("tick = %v", c.PriceTick())
	}
	_, _ = tr.GetContractInfo("BTCUSDT")
	if n := len(f.find(bitgetContractsPath)); n != 1 {
		t.Errorf("contract fetched %d times, want cached", n)
	}
}

func TestBitgetContractParsesRealisticRwaResponse(t *testing.T) {
	// Shape taken from the live NVDAUSDT contract response
	tr, f := newTestBitget(t, false)
	f.set(bitgetContractsPath, `[{"symbol":"NVDAUSDT","baseCoin":"NVDA","quoteCoin":"USDT","buyLimitPriceRatio":"0.02","makerFeeRate":"0.0002","takerFeeRate":"0.0006","supportMarginCoins":["USDT"],"minTradeNum":"0.01","priceEndStep":"1","volumePlace":"2","pricePlace":"2","sizeMultiplier":"0.01","symbolType":"perpetual","minTradeUSDT":"5","symbolStatus":"normal","fundInterval":"8","minLever":"1","maxLever":"100","maxMarketOrderQty":"9500","maxOrderQty":"52000","isRwa":"YES"}]`)
	c, err := tr.GetContractInfo("NVDAUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRwa || c.MaxLever != 100 || c.PricePlace != 2 || math.Abs(c.PriceTick()-0.01) > 1e-12 || c.MaxTradeNum != 52000 {
		t.Errorf("contract = %+v", c)
	}
	if got := roundBitgetPrice(c, 123.456); got != "123.46" {
		t.Errorf("rounded = %s", got)
	}
}

// ---------- order sync ----------

func TestClassifyBitgetFill(t *testing.T) {
	tests := []struct {
		name      string
		side      string
		tradeSide string
		profit    float64
		want      string
	}{
		{"hedge open long", "buy", "open", 0, "open_long"},
		{"hedge open short", "sell", "open", 0, "open_short"},
		{"hedge close long", "sell", "close", 5, "close_long"},
		{"hedge close short", "buy", "close", -5, "close_short"},
		{"one-way buy open", "buy", "buy_single", 0, "open_long"},
		{"one-way sell open", "sell", "sell_single", 0, "open_short"},
		{"one-way sell close", "sell", "sell_single", 12.5, "close_long"},
		{"one-way buy close", "buy", "buy_single", -3, "close_short"},
		{"reduce close long", "sell", "reduce_close_long", 0, "close_long"},
		{"reduce close short", "buy", "reduce_close_short", 0, "close_short"},
		{"burst close long", "sell", "burst_close_long", -100, "close_long"},
		{"burst close short", "buy", "burst_close_short", -100, "close_short"},
		{"offset close long", "sell", "offset_close_long", 0, "close_long"},
		{"reduce one-way no suffix", "buy", "reduce_buy_single", 0, "close_short"},
		{"uppercase", "SELL", "SELL_SINGLE", 1, "close_long"},
		{"empty tradeSide with profit", "sell", "", 2, "close_long"},
		{"empty tradeSide no profit", "buy", "", 0, "open_long"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBitgetFill(tc.side, tc.tradeSide, tc.profit); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBitgetGetTradesFeeAndPagination(t *testing.T) {
	tr, f := newTestBitget(t, false)

	var mu sync.Mutex
	pages := 0
	// Override the fill-history path with a paging handler by wrapping data per call.
	pageData := []string{
		`{"fillList":[
			{"tradeId":"t3","orderId":"o3","symbol":"NVDAUSDT","side":"sell","price":"130","baseVolume":"1","profit":"4.5","tradeSide":"sell_single","cTime":"1700000003000","feeDetail":[{"feeCoin":"USDT","totalFee":"-0.0780"}]},
			{"tradeId":"t2","orderId":"o2","symbol":"NVDAUSDT","side":"buy","price":"126","baseVolume":"1","profit":"0","tradeSide":"buy_single","cTime":"1700000002000","feeDetail":[{"feeCoin":"USDT","totalFee":"-0.05"},{"feeCoin":"BGB","totalFee":"-0.01"}]}],
		  "endId":"t2"}`,
		`{"fillList":[
			{"tradeId":"t1","orderId":"o1","symbol":"BTCUSDT","side":"buy","price":"50000","baseVolume":"0.01","profit":"0","tradeSide":"open","cTime":"1700000001000","feeDetail":[{"feeCoin":"USDT","totalFee":"-0.3"}]}],
		  "endId":""}`,
	}
	f.mu.Lock()
	delete(f.data, bitgetFillHistoryPath)
	f.mu.Unlock()
	// replace server with paging behaviour
	paging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("idLessThan") == "" {
			pages = 0
		}
		idx := pages
		if idx >= len(pageData) {
			idx = len(pageData) - 1
		}
		pages++
		_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":` + pageData[idx] + `}`))
	}))
	defer paging.Close()
	tr.baseURL = paging.URL

	trades, err := tr.GetTrades(time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 3 {
		t.Fatalf("trades = %d", len(trades))
	}
	mu.Lock()
	if pages != 2 {
		t.Errorf("pages fetched = %d, want 2 (stop on empty endId)", pages)
	}
	mu.Unlock()

	byID := map[string]BitgetTrade{}
	for _, tt := range trades {
		byID[tt.TradeID] = tt
	}
	if tt := byID["t3"]; tt.OrderAction != "close_long" || math.Abs(tt.Fee-0.078) > 1e-9 || tt.ProfitLoss != 4.5 || tt.Symbol != "NVDAUSDT" {
		t.Errorf("t3 = %+v", tt)
	}
	if tt := byID["t2"]; tt.OrderAction != "open_long" || math.Abs(tt.Fee-0.06) > 1e-9 {
		t.Errorf("t2 = %+v", tt)
	}
	if tt := byID["t1"]; tt.OrderAction != "open_long" || tt.Fee != 0.3 || tt.FeeAsset != "USDT" {
		t.Errorf("t1 = %+v", tt)
	}
}

func TestBitgetGetTradesPaginationParams(t *testing.T) {
	tr, f := newTestBitget(t, false)
	var queries []map[string]string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		mu.Lock()
		queries = append(queries, q)
		n := len(queries)
		mu.Unlock()
		// always return a cursor -> must be capped at bitgetFillMaxPages
		id := "c" + string(rune('a'+n))
		_, _ = w.Write([]byte(`{"code":"00000","data":{"fillList":[{"tradeId":"` + id + `","symbol":"BTCUSDT","side":"buy","price":"1","baseVolume":"1","tradeSide":"open","cTime":"1"}],"endId":"` + id + `"}}`))
	}))
	defer srv.Close()
	_ = f
	tr.baseURL = srv.URL
	trades, err := tr.GetTrades(time.Now().Add(-time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != bitgetFillMaxPages || len(trades) != bitgetFillMaxPages {
		t.Errorf("queries=%d trades=%d, want %d", len(queries), len(trades), bitgetFillMaxPages)
	}
	if queries[0]["limit"] != "100" || queries[0]["idLessThan"] != "" || queries[1]["idLessThan"] != "cb" {
		t.Errorf("queries = %v", queries[:2])
	}
}

// ---------- demo integration (needs real demo credentials) ----------

func TestBitgetDemoIntegration(t *testing.T) {
	key := os.Getenv("BITGET_DEMO_API_KEY")
	secret := os.Getenv("BITGET_DEMO_SECRET_KEY")
	pass := os.Getenv("BITGET_DEMO_PASSPHRASE")
	if key == "" || secret == "" || pass == "" {
		t.Skip("BITGET_DEMO_API_KEY / BITGET_DEMO_SECRET_KEY / BITGET_DEMO_PASSPHRASE not set")
	}

	tr := NewBitgetTraderWithOptions(key, secret, pass, true)
	const symbol = "BTCUSDT"

	bal, err := tr.GetBalance()
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	t.Logf("balance: %v", bal)

	contract, err := tr.GetContractInfo(symbol)
	if err != nil {
		t.Fatalf("GetContractInfo: %v", err)
	}
	price, err := tr.GetMarketPrice(symbol)
	if err != nil {
		t.Fatalf("GetMarketPrice: %v", err)
	}
	// smallest order that satisfies both min size and min notional
	qty := math.Max(contract.MinTradeNum, math.Ceil(contract.MinTradeUSDT*1.2/price/contract.SizeMultiplier)*contract.SizeMultiplier)

	if err := tr.SetMarginMode(symbol, true); err != nil {
		t.Logf("SetMarginMode: %v", err)
	}
	res, err := tr.OpenLong(symbol, qty, 3)
	if err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	t.Logf("open: %v", res)
	// always try to clean up
	defer func() {
		_ = tr.CancelAllOrders(symbol)
		if _, err := tr.CloseLong(symbol, 0); err != nil {
			t.Logf("cleanup CloseLong: %v", err)
		}
	}()

	if err := tr.SetStopLoss(symbol, "LONG", qty, price*0.9); err != nil {
		t.Fatalf("SetStopLoss: %v", err)
	}
	if err := tr.SetTakeProfit(symbol, "LONG", qty, price*1.1); err != nil {
		t.Fatalf("SetTakeProfit: %v", err)
	}
	orders, err := tr.GetOpenOrders(symbol)
	if err != nil {
		t.Fatalf("GetOpenOrders: %v", err)
	}
	var sl, tp bool
	for _, o := range orders {
		t.Logf("open order: %+v", o)
		sl = sl || o.Type == "STOP_MARKET"
		tp = tp || o.Type == "TAKE_PROFIT_MARKET"
	}
	if !sl || !tp {
		t.Errorf("expected SL and TP in open orders (sl=%v tp=%v)", sl, tp)
	}

	if _, err := tr.CloseLong(symbol, 0); err != nil {
		t.Fatalf("CloseLong: %v", err)
	}
	if err := tr.CancelStopOrders(symbol); err != nil {
		t.Errorf("CancelStopOrders: %v", err)
	}

	time.Sleep(2 * time.Second)
	recs, err := tr.GetClosedPnL(time.Now().Add(-10*time.Minute), 20)
	if err != nil {
		t.Fatalf("GetClosedPnL: %v", err)
	}
	t.Logf("closed pnl records: %+v", recs)
}
