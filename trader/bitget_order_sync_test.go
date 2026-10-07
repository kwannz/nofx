package trader

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"
)

// openSet builds an injectable position lookup from "SYMBOL:SIDE" keys.
func openSet(keys ...string) bitgetOpenPositionFunc {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	return func(symbol, side string) (bool, error) { return set[symbol+":"+side], nil }
}

func TestClassifyBitgetFillWithPositions(t *testing.T) {
	failing := func(string, string) (bool, error) { return false, errors.New("db down") }
	tests := []struct {
		name      string
		side      string
		tradeSide string
		profit    float64
		open      bitgetOpenPositionFunc
		want      string
	}{
		// the bug: break-even closes of one-way positions
		{"break-even sell closes open long", "sell", "sell_single", 0, openSet("BTCUSDT:LONG"), "close_long"},
		{"break-even buy closes open short", "buy", "buy_single", 0, openSet("BTCUSDT:SHORT"), "close_short"},
		{"uppercase break-even", "SELL", "SELL_SINGLE", 0, openSet("BTCUSDT:LONG"), "close_long"},
		// genuine opens
		{"buy with nothing open opens long", "buy", "buy_single", 0, openSet(), "open_long"},
		{"sell with nothing open opens short", "sell", "sell_single", 0, openSet(), "open_short"},
		{"buy adds to an existing long", "buy", "buy_single", 0, openSet("BTCUSDT:LONG"), "open_long"},
		{"sell adds to an existing short", "sell", "sell_single", 0, openSet("BTCUSDT:SHORT"), "open_short"},
		{"other symbol's position is irrelevant", "sell", "sell_single", 0, openSet("ETHUSDT:LONG"), "open_short"},
		// profit != 0 and explicit tradeSides never consult the store
		{"profit still means close", "sell", "sell_single", 3, openSet(), "close_long"},
		{"loss still means close", "buy", "buy_single", -3, openSet(), "close_short"},
		{"hedge open stays open even with opposite position", "sell", "open", 0, openSet("BTCUSDT:LONG"), "open_short"},
		{"hedge close", "sell", "close", 0, openSet(), "close_long"},
		{"reduce_ prefix", "buy", "reduce_close_short", 0, openSet(), "close_short"},
		{"burst_ prefix", "sell", "burst_close_long", 0, openSet(), "close_long"},
		{"offset_ prefix", "sell", "offset_close_long", 0, openSet(), "close_long"},
		// degraded lookups fall back to the position-blind answer
		{"nil lookup", "sell", "sell_single", 0, nil, "open_short"},
		{"failing lookup", "sell", "sell_single", 0, failing, "open_short"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyBitgetFillWithPositions("BTCUSDT", tc.side, tc.tradeSide, tc.profit, tc.open); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestClassifyBitgetFillAmbiguityFlag(t *testing.T) {
	tests := []struct {
		side, tradeSide string
		profit          float64
		wantAction      string
		wantAmbiguous   bool
	}{
		{"sell", "sell_single", 0, "open_short", true},
		{"buy", "buy_single", 0, "open_long", true},
		{"buy", "", 0, "open_long", true},
		{"sell", "sell_single", 1, "close_long", false},
		{"buy", "open", 0, "open_long", false},
		{"sell", "close", 0, "close_long", false},
		{"sell", "reduce_close_long", 0, "close_long", false},
	}
	for _, tc := range tests {
		a, amb := classifyBitgetFillDetailed(tc.side, tc.tradeSide, tc.profit)
		if a != tc.wantAction || amb != tc.wantAmbiguous {
			t.Errorf("%s/%s/%v: got (%s,%v), want (%s,%v)", tc.side, tc.tradeSide, tc.profit, a, amb, tc.wantAction, tc.wantAmbiguous)
		}
	}
}

// A break-even one-way close must close the position recorded by the same sync run instead of
// opening a phantom opposite position.
func TestSyncOrdersFromBitgetBreakEvenClose(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr, f := newTestBitget(t, false)

	base := time.Now().Add(-time.Hour).UnixMilli()
	fill := func(id, side, tradeSide, qty, profit string, offsetMs int64) string {
		return fmt.Sprintf(`{"tradeId":"%s","orderId":"o-%s","symbol":"BTCUSDT","side":"%s","tradeSide":"%s","price":"50000","baseVolume":"%s","profit":"%s","cTime":"%d","feeDetail":[{"feeCoin":"USDT","totalFee":"-0.03"}]}`,
			id, id, side, tradeSide, qty, profit, base+offsetMs)
	}
	// Bitget returns newest first.
	f.set(bitgetFillHistoryPath, `{"fillList":[`+
		fill("t3", "sell", "sell_single", "0.5", "0", 3000)+`,`+ // genuine new short (long already closed)
		fill("t2", "sell", "sell_single", "0.2", "0", 2000)+`,`+ // break-even close of the long
		fill("t1", "buy", "buy_single", "0.2", "0", 1000)+ // open long
		`],"endId":""}`)

	if err := tr.SyncOrdersFromBitget("trader-1", "exch-1", "bitget", st); err != nil {
		t.Fatal(err)
	}

	orders, err := st.Order().GetTraderOrders("trader-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, o := range orders {
		actions[o.ExchangeOrderID] = o.OrderAction
	}
	want := map[string]string{"t1": "open_long", "t2": "close_long", "t3": "open_short"}
	for id, w := range want {
		if actions[id] != w {
			t.Errorf("trade %s action = %q, want %q (all: %v)", id, actions[id], w, actions)
		}
	}

	if pos, _ := st.Position().GetOpenPositionBySymbol("trader-1", "BTCUSDT", "LONG"); pos != nil {
		t.Errorf("long must have been closed by the break-even fill: %+v", pos)
	}
	short, _ := st.Position().GetOpenPositionBySymbol("trader-1", "BTCUSDT", "SHORT")
	if short == nil || short.Quantity != 0.5 {
		t.Errorf("only the genuine open (0.5) may be an open short, got %+v", short)
	}
	closed, _ := st.Position().GetClosedPositions("trader-1", 10)
	if len(closed) != 1 || closed[0].Side != "LONG" {
		t.Errorf("expected one closed LONG, got %+v", closed)
	}
}
