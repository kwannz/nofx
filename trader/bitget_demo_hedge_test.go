package trader

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"nofx/store"
)

// demoHedgeFlow is the hedge-mode half of TestBitgetDemoIntegration (env-gated, Bitget DEMO):
// the flat account is switched to hedge_mode, a long and a short of the same symbol are open at
// the same time, each side has its own SL/TP, each side is closed independently and the fills are
// synced and classified (open/close from tradeSide, never ambiguous).
func demoHedgeFlow(t *testing.T, creds demoCreds, symbol string) {
	tr := creds.trader(WithBitgetPositionMode(BitgetPositionModeHedge))
	if tr.PositionMode() != BitgetPositionModeHedge {
		t.Fatalf("the flat demo account must have been switched to hedge mode, got %q", tr.PositionMode())
	}
	if mode, err := tr.detectPositionMode(); err != nil || mode != bitgetPosModeHedge {
		t.Fatalf("exchange reports posMode %q (err %v), want hedge_mode", mode, err)
	}
	demoRegisterFlatCleanup(t, tr, symbol)

	contract, err := tr.GetContractInfo(symbol)
	if err != nil {
		t.Fatalf("GetContractInfo: %v", err)
	}
	price, err := tr.GetMarketPrice(symbol)
	if err != nil || price <= 0 {
		t.Fatalf("GetMarketPrice: %v (price %v)", err, price)
	}
	qty := math.Max(contract.MinTradeNum, math.Ceil(contract.MinTradeUSDT*1.2/price/contract.SizeMultiplier)*contract.SizeMultiplier)
	t.Logf("price=%v qty=%v (min %v / step %v)", price, qty, contract.MinTradeNum, contract.SizeMultiplier)

	if err := tr.SetMarginMode(symbol, true); err != nil {
		t.Errorf("SetMarginMode(crossed) on a flat account must succeed: %v", err)
	}

	type placed struct {
		label, orderID, action string
	}
	var orders []placed

	// ---------------- open LONG, with a deliberately wrong mode belief: 40774 -> refresh -> retry
	tr.setPosMode(bitgetPosModeOneWay)
	open, err := tr.OpenLong(symbol, qty, 3)
	if err != nil {
		t.Fatalf("OpenLong must self-correct after a 40774 rejection: %v", err)
	}
	if tr.PositionMode() != BitgetPositionModeHedge {
		t.Errorf("the mode belief must be refreshed to hedge after the rejection, got %q", tr.PositionMode())
	}
	checkFilled(t, "OpenLong", open, qty)
	orders = append(orders, placed{"open long", open["orderId"].(string), "open_long"})

	// ---------------- open SHORT at the same time (same symbol, other side)
	openS, err := tr.OpenShort(symbol, qty, 3)
	if err != nil {
		t.Fatalf("OpenShort while the long is open: %v", err)
	}
	checkFilled(t, "OpenShort", openS, qty)
	orders = append(orders, placed{"open short", openS["orderId"].(string), "open_short"})

	pos, err := demoPositions(tr, symbol)
	if err != nil || len(pos) != 2 {
		t.Fatalf("expected a long AND a short position, got %v (err=%v)", pos, err)
	}
	bySide := map[string]map[string]interface{}{}
	for _, p := range pos {
		bySide[p["side"].(string)] = p
	}
	if bySide["long"] == nil || bySide["short"] == nil {
		t.Fatalf("both sides expected: %v", pos)
	}
	checkPosition(t, bySide["long"], "long", qty, 3)
	checkPosition(t, bySide["short"], "short", qty, 3)

	// ---------------- SL / TP on each side
	slL, tpL := price*0.90, price*1.10
	slS, tpS := price*1.10, price*0.90
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"SL long", func() error { return tr.SetStopLoss(symbol, "LONG", qty, slL) }},
		{"TP long", func() error { return tr.SetTakeProfit(symbol, "LONG", qty, tpL) }},
		{"SL short", func() error { return tr.SetStopLoss(symbol, "SHORT", qty, slS) }},
		{"TP short", func() error { return tr.SetTakeProfit(symbol, "SHORT", qty, tpS) }},
	} {
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	plans := demoHedgePlanOrders(t, tr, symbol)
	wantTrig := map[string]float64{
		"LONG/SL": roundedPrice(contract, slL), "LONG/TP": roundedPrice(contract, tpL),
		"SHORT/SL": roundedPrice(contract, slS), "SHORT/TP": roundedPrice(contract, tpS),
	}
	for k, want := range wantTrig {
		if got := plans[k].StopPrice; got != want {
			t.Errorf("%s trigger = %v, want %v", k, got, want)
		}
	}

	// upsert: replacing the long SL keeps the orderId, changes only that trigger, leaves the short alone
	slL2 := price * 0.92
	if err := tr.SetStopLoss(symbol, "LONG", qty, slL2); err != nil {
		t.Fatalf("SetStopLoss (replace): %v", err)
	}
	plans2 := demoHedgePlanOrders(t, tr, symbol)
	if plans2["LONG/SL"].StopPrice != roundedPrice(contract, slL2) {
		t.Errorf("replaced long SL trigger = %v, want %v", plans2["LONG/SL"].StopPrice, roundedPrice(contract, slL2))
	}
	t.Logf("long SL replace: orderId %s -> %s (Bitget upserts per holdSide)", plans["LONG/SL"].OrderID, plans2["LONG/SL"].OrderID)
	for _, k := range []string{"LONG/TP", "SHORT/SL", "SHORT/TP"} {
		if plans2[k].OrderID != plans[k].OrderID || plans2[k].StopPrice != plans[k].StopPrice {
			t.Errorf("%s must be untouched by the long SL replace: %+v -> %+v", k, plans[k], plans2[k])
		}
	}

	// a rejected SL (wrong side of the mark) leaves the live one intact, per side
	if err := tr.SetStopLoss(symbol, "LONG", qty, price*1.05); err == nil {
		t.Errorf("a long SL above the mark price must be rejected")
	}
	if err := tr.SetStopLoss(symbol, "SHORT", qty, price*0.95); err == nil {
		t.Errorf("a short SL below the mark price must be rejected")
	}
	plans3 := demoHedgePlanOrders(t, tr, symbol)
	for k, p := range plans2 {
		if plans3[k].StopPrice != p.StopPrice || plans3[k].OrderID != p.OrderID {
			t.Errorf("%s changed by a rejected placement: %+v -> %+v", k, p, plans3[k])
		}
	}

	// cancelling one side's stop loss touches nothing else
	if err := tr.CancelStopLossOrdersForSide(symbol, "LONG"); err != nil {
		t.Fatalf("CancelStopLossOrdersForSide: %v", err)
	}
	waitFor(t, 8*time.Second, "only the long SL to disappear", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		got := map[string]bool{}
		for _, x := range o {
			got[x.PositionSide+"/"+map[string]string{"STOP_MARKET": "SL", "TAKE_PROFIT_MARKET": "TP"}[x.Type]] = true
		}
		return err == nil && len(o) == 3 && !got["LONG/SL"] && got["LONG/TP"] && got["SHORT/SL"] && got["SHORT/TP"], fmt.Sprintf("orders=%+v err=%v", o, err)
	})
	if err := tr.SetStopLoss(symbol, "LONG", qty, slL); err != nil { // back to four
		t.Fatalf("SetStopLoss (re-place): %v", err)
	}
	demoHedgePlanOrders(t, tr, symbol)

	// ---------------- close the LONG only (quantity 0 = "its whole position")
	closeLong, err := tr.CloseLong(symbol, 0)
	if err != nil {
		t.Fatalf("CloseLong: %v", err)
	}
	checkFilled(t, "CloseLong", closeLong, qty)
	orders = append(orders, placed{"close long", closeLong["orderId"].(string), "close_long"})
	pos, err = demoPositions(tr, symbol)
	if err != nil || len(pos) != 1 || pos[0]["side"] != "short" {
		t.Fatalf("only the short must remain after CloseLong, got %v (err=%v)", pos, err)
	}
	waitFor(t, 15*time.Second, "the long's TP/SL to vanish while the short's stay", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		ok := err == nil && len(o) == 2
		for _, x := range o {
			ok = ok && x.PositionSide == "SHORT"
		}
		return ok, fmt.Sprintf("orders=%+v err=%v", o, err)
	})

	// ---------------- close the SHORT
	closeShort, err := tr.CloseShort(symbol, 0)
	if err != nil {
		t.Fatalf("CloseShort: %v", err)
	}
	checkFilled(t, "CloseShort", closeShort, qty)
	orders = append(orders, placed{"close short", closeShort["orderId"].(string), "close_short"})
	waitFor(t, 15*time.Second, "all TP/SL to disappear after both sides were closed", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		return err == nil && len(o) == 0, fmt.Sprintf("orders=%+v err=%v", o, err)
	})
	if pos, err := demoPositions(tr, symbol); err != nil || len(pos) != 0 {
		t.Fatalf("flat expected after closing both sides: %v (err=%v)", pos, err)
	}

	// ---------------- a regular pending order in hedge mode (open long limit far below the market)
	// size so that the order value clears the 5 USDT minimum even at half the market price
	limitPrice := roundedPrice(contract, price*0.5)
	limitQty := math.Ceil(contract.MinTradeUSDT*1.6/limitPrice/contract.SizeMultiplier) * contract.SizeMultiplier
	limitBody := map[string]interface{}{
		"symbol": symbol, "productType": bitgetProductType, "marginMode": "crossed", "marginCoin": "USDT",
		"side": "buy", "tradeSide": "open", "orderType": "limit", "force": "gtc",
		"price": roundBitgetPrice(contract, limitPrice), "size": strconv.FormatFloat(limitQty, 'f', contract.VolumePlace, 64), "clientOid": genBitgetClientOid(),
	}
	if _, err := tr.doRequest("POST", bitgetOrderPath, limitBody); err != nil {
		t.Fatalf("place a far-away hedge limit order: %v", err)
	}
	var limit OpenOrder
	waitFor(t, 8*time.Second, "the limit order to be listed", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		if err == nil && len(o) == 1 {
			limit = o[0]
		}
		return err == nil && len(o) == 1, fmt.Sprintf("orders=%+v err=%v", o, err)
	})
	if limit.Side != "BUY" || limit.PositionSide != "LONG" || limit.Type != "LIMIT" || limit.Status != "NEW" {
		t.Errorf("hedge open-long limit order must be reported BUY / LONG / LIMIT / NEW: %+v", limit)
	}
	if err := tr.CancelAllOrders(symbol); err != nil {
		t.Errorf("CancelAllOrders: %v", err)
	}

	// ---------------- fills: hedge fills say open|close, never ambiguous
	var trades []BitgetTrade
	waitFor(t, 20*time.Second, "all four fills to appear in /fills", func() (bool, string) {
		trades, err = tr.GetTrades(time.Now().Add(-30*time.Minute), 100)
		if err != nil {
			return false, err.Error()
		}
		n := 0
		for _, o := range orders {
			for _, tt := range trades {
				if tt.OrderID == o.orderID {
					n++
					break
				}
			}
		}
		return n == len(orders), fmt.Sprintf("found %d/%d of our orders among %d fills", n, len(orders), len(trades))
	})
	if err != nil {
		t.Fatalf("GetTrades: %v", err)
	}
	tradeIDByOrder := map[string]string{}
	for _, o := range orders {
		var found *BitgetTrade
		for i := range trades {
			if trades[i].OrderID == o.orderID {
				found = &trades[i]
			}
		}
		if found == nil {
			t.Errorf("%s: no fill for order %s", o.label, o.orderID)
			continue
		}
		tradeIDByOrder[o.orderID] = found.TradeID
		t.Logf("fill %-11s trade=%s side=%s tradeSide=%s posMode=%s price=%v qty=%v profit=%v fee=%v -> %s (ambiguous=%v)",
			o.label, found.TradeID, found.Side, found.TradeSide, found.PosMode, found.FillPrice, found.FillQty, found.ProfitLoss, found.Fee, found.OrderAction, found.ActionAmbiguous)
		wantTS := "open"
		if o.action[:5] == "close" {
			wantTS = "close"
		}
		if found.TradeSide != wantTS || found.PosMode != bitgetPosModeHedge {
			t.Errorf("%s: hedge fill must have tradeSide=%s / posMode=hedge_mode: %+v", o.label, wantTS, found)
		}
		if found.OrderAction != o.action || found.ActionAmbiguous {
			t.Errorf("%s: classified %s (ambiguous=%v), want %s exactly", o.label, found.OrderAction, found.ActionAmbiguous, o.action)
		}
		// the fill side is the POSITION direction: long legs buy, short legs sell
		wantSide := "buy"
		if o.action == "open_short" || o.action == "close_short" {
			wantSide = "sell"
		}
		if found.Side != wantSide {
			t.Errorf("%s: hedge fill side = %s, want the position direction %s", o.label, found.Side, wantSide)
		}
		if found.Symbol != symbol || found.FillQty <= 0 || found.FillPrice <= 0 || found.Fee <= 0 || found.ExecTime.IsZero() {
			t.Errorf("%s: incomplete fill %+v", o.label, found)
		}
	}

	// ---------------- sync into a scratch store
	sdb, err := store.New(filepath.Join(t.TempDir(), "demo-hedge-sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SyncOrdersFromBitget("demo-trader", "demo-exch", "bitget", sdb); err != nil {
		t.Fatalf("SyncOrdersFromBitget: %v", err)
	}
	stored, err := sdb.Order().GetTraderOrders("demo-trader", 500)
	if err != nil {
		t.Fatal(err)
	}
	byTrade := map[string]*store.TraderOrder{}
	for _, o := range stored {
		byTrade[o.ExchangeOrderID] = o
	}
	for _, o := range orders {
		so := byTrade[tradeIDByOrder[o.orderID]]
		if so == nil {
			t.Errorf("%s: not synced", o.label)
			continue
		}
		wantPos := "LONG"
		if o.action == "open_short" || o.action == "close_short" {
			wantPos = "SHORT"
		}
		wantSide := map[string]string{"open_long": "BUY", "close_long": "SELL", "open_short": "SELL", "close_short": "BUY"}[o.action]
		if so.OrderAction != o.action || so.PositionSide != wantPos || so.Side != wantSide {
			t.Errorf("synced %s = action %q side %q positionSide %q, want %s / %s / %s", o.label, so.OrderAction, so.Side, so.PositionSide, o.action, wantSide, wantPos)
		}
	}
	closedPos, err := sdb.Position().GetClosedPositions("demo-trader", 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ label, side, openOrder string }{
		{"long", "LONG", open["orderId"].(string)},
		{"short", "SHORT", openS["orderId"].(string)},
	} {
		var hit *store.TraderPosition
		for _, p := range closedPos {
			if p.EntryOrderID == tradeIDByOrder[want.openOrder] {
				hit = p
			}
		}
		if hit == nil || hit.Side != want.side || hit.Status != "CLOSED" {
			t.Errorf("synced %s position not closed correctly: %+v", want.label, hit)
		}
	}
	if openPos, err := sdb.Position().GetOpenPositions("demo-trader"); err != nil || len(openPos) != 0 {
		t.Errorf("no position may stay OPEN after syncing a fully closed hedge round trip: %v (err %v)", openPos, err)
	}
}

// demoHedgePlanOrders asserts that GetOpenOrders reports exactly four TP/SL plan orders (a stop
// loss and a take profit for the LONG, the same for the SHORT), each on the right position side
// with the right closing order side (long: SELL, short: BUY), and returns them keyed
// "LONG/SL", "LONG/TP", "SHORT/SL", "SHORT/TP". It polls briefly: the listing is eventually
// consistent right after a placement.
func demoHedgePlanOrders(t *testing.T, tr *BitgetTrader, symbol string) map[string]OpenOrder {
	t.Helper()
	var orders []OpenOrder
	ok := waitFor(t, 8*time.Second, "four hedge TP/SL plan orders to be listed", func() (bool, string) {
		var err error
		orders, err = tr.GetOpenOrders(symbol)
		if err != nil {
			return false, err.Error()
		}
		return len(orders) == 4, fmt.Sprintf("%+v", orders)
	})
	if !ok {
		t.Fatalf("expected four plan orders, got %+v", orders)
	}
	out := map[string]OpenOrder{}
	for _, o := range orders {
		kind := map[string]string{"STOP_MARKET": "SL", "TAKE_PROFIT_MARKET": "TP"}[o.Type]
		wantSide := map[string]string{"LONG": "SELL", "SHORT": "BUY"}[o.PositionSide]
		if kind == "" || wantSide == "" || o.Side != wantSide || o.Symbol != symbol || o.Status != "NEW" || o.StopPrice <= 0 {
			t.Errorf("unexpected hedge plan order %+v", o)
			continue
		}
		key := o.PositionSide + "/" + kind
		if _, dup := out[key]; dup {
			t.Errorf("duplicate %s plan order: %+v", key, orders)
		}
		out[key] = o
	}
	for _, k := range []string{"LONG/SL", "LONG/TP", "SHORT/SL", "SHORT/TP"} {
		if out[k].OrderID == "" {
			t.Fatalf("missing %s in %+v", k, orders)
		}
	}
	t.Logf("hedge protection: LONG SL %v / TP %v, SHORT SL %v / TP %v",
		out["LONG/SL"].StopPrice, out["LONG/TP"].StopPrice, out["SHORT/SL"].StopPrice, out["SHORT/TP"].StopPrice)
	return out
}
