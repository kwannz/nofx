package trader

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// TestBitgetDemoIntegration drives the live trader against the Bitget DEMO environment
// (one-way mode, USDT-FUTURES, header paptrading: 1). It trades the minimum BTCUSDT size
// (long leg, then short leg), checks every response shape the trader depends on and MUST leave
// the account flat: cleanup closes anything left, cancels all orders and then asserts that no
// position and no pending / TP-SL order remains.
//
//	source bitget_demo.env   # BITGET_DEMO_API_KEY / BITGET_DEMO_SECRET_KEY / BITGET_DEMO_PASSPHRASE
//	go test ./trader -run TestBitgetDemoIntegration -count=1 -v
func TestBitgetDemoIntegration(t *testing.T) {
	key := os.Getenv("BITGET_DEMO_API_KEY")
	secret := os.Getenv("BITGET_DEMO_SECRET_KEY")
	pass := os.Getenv("BITGET_DEMO_PASSPHRASE")
	if key == "" || secret == "" || pass == "" {
		t.Skip("BITGET_DEMO_API_KEY / BITGET_DEMO_SECRET_KEY / BITGET_DEMO_PASSPHRASE not set")
	}

	tr := NewBitgetTraderWithOptions(key, secret, pass, true)
	const symbol = "BTCUSDT"

	// ---- pre-flight: never run on top of a position / orders we did not open ----
	if pos, err := demoPositions(tr, symbol); err != nil || len(pos) != 0 {
		t.Fatalf("demo account is not flat before the test, refusing to trade (err=%v): %v", err, pos)
	}
	if orders, err := tr.GetOpenOrders(symbol); err != nil || len(orders) != 0 {
		t.Fatalf("demo account has pending orders before the test (err=%v): %+v", err, orders)
	}

	// ---- cleanup: leave the account flat, then PROVE it ----
	t.Cleanup(func() {
		if err := tr.CancelAllOrders(symbol); err != nil {
			t.Logf("cleanup CancelAllOrders: %v", err)
		}
		leftover, err := demoPositions(tr, symbol)
		if err != nil {
			t.Errorf("cleanup: GetPositions: %v", err)
		}
		for _, p := range leftover {
			side, _ := p["side"].(string)
			var err error
			if side == "short" {
				_, err = tr.CloseShort(symbol, 0)
			} else {
				_, err = tr.CloseLong(symbol, 0)
			}
			if err != nil {
				t.Errorf("cleanup: closing leftover %s position: %v", side, err)
			}
		}
		// an exchange-side TP/SL disappears with its position; wait for that, then cancel stragglers
		waitFor(t, 10*time.Second, "TP/SL orders to vanish after the position is closed", func() (bool, string) {
			orders, err := tr.GetOpenOrders(symbol)
			return err == nil && len(orders) == 0, fmt.Sprintf("orders=%+v err=%v", orders, err)
		})
		if err := tr.CancelAllOrders(symbol); err != nil {
			t.Logf("cleanup CancelAllOrders (2): %v", err)
		}
		if pos, err := demoPositions(tr, symbol); err != nil || len(pos) != 0 {
			t.Errorf("ACCOUNT NOT FLAT after the test: positions=%v err=%v", pos, err)
		}
		if orders, err := tr.GetOpenOrders(symbol); err != nil || len(orders) != 0 {
			t.Errorf("ACCOUNT NOT FLAT after the test: pending orders=%+v err=%v", orders, err)
		} else {
			t.Logf("flat check OK: no %s position and no pending/TP-SL orders", symbol)
		}
	})

	bal, err := tr.GetBalance()
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	t.Logf("balance: equity=%v available=%v", bal["totalEquity"], bal["availableBalance"])

	contract, err := tr.GetContractInfo(symbol)
	if err != nil {
		t.Fatalf("GetContractInfo: %v", err)
	}
	price, err := tr.GetMarketPrice(symbol)
	if err != nil || price <= 0 {
		t.Fatalf("GetMarketPrice: %v (price %v)", err, price)
	}
	// smallest order that satisfies both min size and min notional
	qty := math.Max(contract.MinTradeNum, math.Ceil(contract.MinTradeUSDT*1.2/price/contract.SizeMultiplier)*contract.SizeMultiplier)
	t.Logf("price=%v qty=%v (min %v / step %v)", price, qty, contract.MinTradeNum, contract.SizeMultiplier)

	if err := tr.SetMarginMode(symbol, true); err != nil {
		t.Errorf("SetMarginMode(crossed) on a flat account must succeed: %v", err)
	}

	// Fills of every order placed below, for the order-sync verification at the end.
	type placed struct {
		label   string
		orderID string
		action  string // expected classification once positions are known
	}
	var orders []placed

	// =========================== LONG leg ===========================
	open, err := tr.OpenLong(symbol, qty, 3)
	if err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	checkFilled(t, "OpenLong", open, qty)
	orders = append(orders, placed{"open long", open["orderId"].(string), "open_long"})

	pos, err := demoPositions(tr, symbol)
	if err != nil || len(pos) != 1 {
		t.Fatalf("expected exactly one position after OpenLong, got %v (err=%v)", pos, err)
	}
	checkPosition(t, pos[0], "long", qty, 3)

	slPrice, tpPrice := price*0.90, price*1.10
	if err := tr.SetStopLoss(symbol, "LONG", qty, slPrice); err != nil {
		t.Fatalf("SetStopLoss: %v", err)
	}
	if err := tr.SetTakeProfit(symbol, "LONG", qty, tpPrice); err != nil {
		t.Fatalf("SetTakeProfit: %v", err)
	}
	sl, tp := demoPlanOrders(t, tr, symbol, "LONG", "SELL")
	if sl.StopPrice != roundedPrice(contract, slPrice) || tp.StopPrice != roundedPrice(contract, tpPrice) {
		t.Errorf("trigger prices: SL %v (want %v), TP %v (want %v)", sl.StopPrice, roundedPrice(contract, slPrice), tp.StopPrice, roundedPrice(contract, tpPrice))
	}

	// replacing the SL is an upsert: still exactly one SL, with the NEW trigger
	slPrice2 := price * 0.92
	if err := tr.SetStopLoss(symbol, "LONG", qty, slPrice2); err != nil {
		t.Fatalf("SetStopLoss (replace): %v", err)
	}
	sl2, tp2 := demoPlanOrders(t, tr, symbol, "LONG", "SELL")
	if sl2.StopPrice != roundedPrice(contract, slPrice2) {
		t.Errorf("replaced SL trigger = %v, want %v", sl2.StopPrice, roundedPrice(contract, slPrice2))
	}
	if tp2.StopPrice != tp.StopPrice {
		t.Errorf("TP must be untouched by an SL replace: %v -> %v", tp.StopPrice, tp2.StopPrice)
	}
	t.Logf("SL replace: orderId %s -> %s (Bitget upserts in place)", sl.OrderID, sl2.OrderID)

	// a rejected SL (above the mark price of a LONG) must leave the existing one intact
	if err := tr.SetStopLoss(symbol, "LONG", qty, price*1.05); err == nil {
		t.Errorf("an SL above the mark price of a long must be rejected")
	} else {
		t.Logf("expected rejection: %v", err)
	}
	sl3, _ := demoPlanOrders(t, tr, symbol, "LONG", "SELL")
	if sl3.StopPrice != sl2.StopPrice || sl3.OrderID != sl2.OrderID {
		t.Errorf("rejected SL changed the live one: %+v -> %+v", sl2, sl3)
	}

	closeLong, err := tr.CloseLong(symbol, 0)
	if err != nil {
		t.Fatalf("CloseLong: %v", err)
	}
	checkFilled(t, "CloseLong", closeLong, qty)
	orders = append(orders, placed{"close long", closeLong["orderId"].(string), "close_long"})

	waitFor(t, 15*time.Second, "TP/SL to disappear after the long was closed", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		return err == nil && len(o) == 0, fmt.Sprintf("orders=%+v err=%v", o, err)
	})

	// =========================== SHORT leg ===========================
	openS, err := tr.OpenShort(symbol, qty, 3)
	if err != nil {
		t.Fatalf("OpenShort: %v", err)
	}
	checkFilled(t, "OpenShort", openS, qty)
	orders = append(orders, placed{"open short", openS["orderId"].(string), "open_short"})

	pos, err = demoPositions(tr, symbol)
	if err != nil || len(pos) != 1 {
		t.Fatalf("expected exactly one position after OpenShort, got %v (err=%v)", pos, err)
	}
	checkPosition(t, pos[0], "short", qty, 3)

	slS, tpS := price*1.10, price*0.90
	if err := tr.SetStopLoss(symbol, "SHORT", qty, slS); err != nil {
		t.Fatalf("SetStopLoss (short): %v", err)
	}
	if err := tr.SetTakeProfit(symbol, "SHORT", qty, tpS); err != nil {
		t.Fatalf("SetTakeProfit (short): %v", err)
	}
	// a short is protected by BUY orders
	slShort, tpShort := demoPlanOrders(t, tr, symbol, "SHORT", "BUY")
	if slShort.StopPrice != roundedPrice(contract, slS) || tpShort.StopPrice != roundedPrice(contract, tpS) {
		t.Errorf("short trigger prices: SL %v (want %v), TP %v (want %v)", slShort.StopPrice, roundedPrice(contract, slS), tpShort.StopPrice, roundedPrice(contract, tpS))
	}
	if err := tr.SetStopLoss(symbol, "SHORT", qty, price*0.95); err == nil {
		t.Errorf("an SL below the mark price of a short must be rejected")
	} else {
		t.Logf("expected rejection: %v", err)
	}
	if slAfter, _ := demoPlanOrders(t, tr, symbol, "SHORT", "BUY"); slAfter.StopPrice != slShort.StopPrice {
		t.Errorf("rejected SL changed the live one: %+v -> %+v", slShort, slAfter)
	}

	closeShort, err := tr.CloseShort(symbol, 0)
	if err != nil {
		t.Fatalf("CloseShort: %v", err)
	}
	checkFilled(t, "CloseShort", closeShort, qty)
	orders = append(orders, placed{"close short", closeShort["orderId"].(string), "close_short"})

	waitFor(t, 15*time.Second, "TP/SL to disappear after the short was closed", func() (bool, string) {
		o, err := tr.GetOpenOrders(symbol)
		return err == nil && len(o) == 0, fmt.Sprintf("orders=%+v err=%v", o, err)
	})

	// GetOrderStatus on a real close order: positive commission, reduce-only fill
	st, err := tr.GetOrderStatus(symbol, closeShort["orderId"].(string))
	if err != nil {
		t.Fatalf("GetOrderStatus: %v", err)
	}
	if st["status"] != "FILLED" || st["commission"].(float64) <= 0 || st["avgPrice"].(float64) <= 0 {
		t.Errorf("order status = %v", st)
	}

	// =========================== fills / order sync ===========================
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
		t.Logf("fill %-11s trade=%s side=%s tradeSide=%s price=%v qty=%v profit=%v fee=%v maker=%v -> %s (ambiguous=%v)",
			o.label, found.TradeID, found.Side, found.TradeSide, found.FillPrice, found.FillQty, found.ProfitLoss, found.Fee, found.IsMaker, found.OrderAction, found.ActionAmbiguous)
		if found.Symbol != symbol || found.FillQty <= 0 || found.FillPrice <= 0 || found.Fee <= 0 || found.FeeAsset == "" || found.ExecTime.IsZero() {
			t.Errorf("%s: incomplete fill %+v", o.label, found)
		}
		// one-way mode: tradeSide never says open/close, an open fill realizes no profit
		if !strings.HasSuffix(found.TradeSide, "_single") {
			t.Errorf("%s: one-way tradeSide (<side>_single) expected: %+v", o.label, found)
		}
		if strings.HasPrefix(o.action, "open_") && found.ProfitLoss != 0 {
			t.Errorf("%s: an open fill must have profit 0: %+v", o.label, found)
		}
		// position-blind classification: profit != 0 closes are exact, everything else is ambiguous
		if found.ProfitLoss != 0 && found.OrderAction != o.action {
			t.Errorf("%s: classified %s, want %s", o.label, found.OrderAction, o.action)
		}
	}

	// the position-aware path: sync the account into a scratch store and check OUR fills
	sdb, err := store.New(filepath.Join(t.TempDir(), "demo-sync.db"))
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
	storedAction := map[string]string{}
	for _, o := range stored {
		storedAction[o.ExchangeOrderID] = o.OrderAction
	}
	for _, o := range orders {
		tid := tradeIDByOrder[o.orderID]
		if got := storedAction[tid]; got != o.action {
			t.Errorf("synced %s (trade %s) action = %q, want %q", o.label, tid, got, o.action)
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
}

// ---------------------------------------------------------------- helpers

// demoPositions returns the live positions of symbol, bypassing the 15 s positions cache.
func demoPositions(tr *BitgetTrader, symbol string) ([]map[string]interface{}, error) {
	tr.clearCache()
	all, err := tr.GetPositions()
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	for _, p := range all {
		if s, _ := p["symbol"].(string); s == symbol {
			out = append(out, p)
		}
	}
	return out, nil
}

func checkFilled(t *testing.T, label string, res map[string]interface{}, qty float64) {
	t.Helper()
	id, _ := res["orderId"].(string)
	avg, _ := res["avgPrice"].(float64)
	exec, _ := res["executedQty"].(float64)
	comm, _ := res["commission"].(float64)
	if id == "" || res["status"] != "FILLED" || avg <= 0 || math.Abs(exec-qty) > 1e-9 || comm <= 0 {
		t.Errorf("%s: unexpected result %v (want FILLED, qty %v, positive avgPrice and commission)", label, res, qty)
		return
	}
	t.Logf("%s: orderId=%s avg=%v qty=%v commission=%v", label, id, avg, exec, comm)
}

func checkPosition(t *testing.T, p map[string]interface{}, side string, qty float64, leverage float64) {
	t.Helper()
	amt, _ := p["positionAmt"].(float64)
	lev, _ := p["leverage"].(float64)
	liq, _ := p["liquidationPrice"].(float64)
	entry, _ := p["entryPrice"].(float64)
	if p["side"] != side || math.Abs(amt-qty) > 1e-9 || lev != leverage || entry <= 0 {
		t.Errorf("position = %v, want %s qty %v lev %vx", p, side, qty, leverage)
	}
	if liq < 0 {
		t.Errorf("liquidationPrice must never be negative (0 = n/a), got %v", liq)
	}
	t.Logf("position: %s qty=%v entry=%v mark=%v lev=%vx liq=%v uPnL=%v", p["side"], amt, entry, p["markPrice"], lev, liq, p["unRealizedProfit"])
}

// demoPlanOrders asserts that GetOpenOrders reports exactly one stop loss and one take profit
// for the position (positionSide LONG/SHORT, closing orderSide SELL/BUY) and returns them.
// It polls briefly because the listing is eventually consistent right after a placement.
func demoPlanOrders(t *testing.T, tr *BitgetTrader, symbol, positionSide, orderSide string) (sl, tp OpenOrder) {
	t.Helper()
	var orders []OpenOrder
	ok := waitFor(t, 8*time.Second, "SL and TP to be listed", func() (bool, string) {
		var err error
		orders, err = tr.GetOpenOrders(symbol)
		if err != nil {
			return false, err.Error()
		}
		n := 0
		for _, o := range orders {
			if o.Type == "STOP_MARKET" || o.Type == "TAKE_PROFIT_MARKET" {
				n++
			}
		}
		return n == 2, fmt.Sprintf("%+v", orders)
	})
	if !ok {
		t.Fatalf("expected exactly one SL and one TP, got %+v", orders)
	}
	for _, o := range orders {
		if o.PositionSide != positionSide || o.Side != orderSide || o.Symbol != symbol || o.Status != "NEW" || o.StopPrice <= 0 {
			t.Errorf("plan order must be PositionSide %s / Side %s: %+v", positionSide, orderSide, o)
		}
		switch o.Type {
		case "STOP_MARKET":
			sl = o
		case "TAKE_PROFIT_MARKET":
			tp = o
		}
	}
	if sl.OrderID == "" || tp.OrderID == "" {
		t.Fatalf("missing SL or TP in %+v", orders)
	}
	t.Logf("%s protected by SL %v (%s) / TP %v (%s)", positionSide, sl.StopPrice, sl.OrderID, tp.StopPrice, tp.OrderID)
	return sl, tp
}

// roundedPrice is the trigger price the trader sends for price, as a float.
func roundedPrice(c *BitgetContract, price float64) float64 {
	v, _ := strconv.ParseFloat(roundBitgetPrice(c, price), 64)
	return v
}

// waitFor polls cond every 500 ms until it holds or the timeout expires (reported via t.Errorf).
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, detail := cond()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			t.Errorf("timed out after %v waiting for %s: %s", timeout, what, detail)
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}
