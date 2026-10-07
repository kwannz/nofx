package trader

import (
	"nofx/store"
	"path/filepath"
	"testing"
)

// closeReasonHarness is an AutoTrader on a real sqlite store driving a paper account, with the
// matcher's system fills wired to recordPaperSystemFill (as NewAutoTrader does).
type closeReasonHarness struct {
	at    *AutoTrader
	st    *store.Store
	paper *BitgetPaperTrader
	src   *fakePaperSource
}

func newCloseReasonHarness(t *testing.T) *closeReasonHarness {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "close_reason.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, src, _ := newTestPaper(t, 10000)
	at := &AutoTrader{
		id:                    "close-reason-trader-1",
		name:                  "close-reason",
		exchange:              "bitget_paper",
		exchangeID:            "paper-exchange-1",
		store:                 st,
		trader:                p,
		positionFirstSeenTime: map[string]int64{},
	}
	p.SetSystemFillHandler(at.recordPaperSystemFill)
	return &closeReasonHarness{at: at, st: st, paper: p, src: src}
}

// openLong opens a BTCUSDT long on the paper account and records it like the AI cycle does.
func (h *closeReasonHarness) openLong(t *testing.T, qty float64, leverage int) {
	t.Helper()
	order, err := h.paper.OpenLong("BTCUSDT", qty, leverage)
	if err != nil {
		t.Fatal(err)
	}
	h.at.recordAndConfirmOrder(order, "BTCUSDT", "open_long", qty, 100, leverage, 0)
	pos, err := h.st.Position().GetOpenPositionBySymbol(h.at.id, "BTCUSDT", "LONG")
	if err != nil || pos == nil {
		t.Fatalf("open position not recorded: %v %v", pos, err)
	}
}

// onlyClosed returns the single CLOSED position of the trader.
func (h *closeReasonHarness) onlyClosed(t *testing.T) *store.TraderPosition {
	t.Helper()
	if pos, _ := h.st.Position().GetOpenPositionBySymbol(h.at.id, "BTCUSDT", "LONG"); pos != nil {
		t.Fatalf("position must be closed in the store: %+v", pos)
	}
	closed, err := h.st.Position().GetClosedPositions(h.at.id, 10)
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed positions: %v %v", closed, err)
	}
	if closed[0].Status != "CLOSED" {
		t.Fatalf("status = %q, want CLOSED", closed[0].Status)
	}
	return closed[0]
}

// A close the Bitget Paper matcher produces on its own (stop loss, take profit, liquidation) and
// persists through recordPaperSystemFill must carry the matcher's reason as close_reason.
func TestPaperSystemFillRecordsCloseReason(t *testing.T) {
	t.Run("stop_loss", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 2, 5) // liquidation price (~81) stays below the stop at 90
		if err := h.paper.SetStopLoss("BTCUSDT", "LONG", 2, 90); err != nil {
			t.Fatal(err)
		}
		h.src.setMark("BTCUSDT", 89)
		h.paper.Tick()
		if got := h.onlyClosed(t).CloseReason; got != "stop_loss" {
			t.Fatalf("close_reason = %q, want stop_loss", got)
		}
	})

	t.Run("take_profit", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 2, 10)
		if err := h.paper.SetTakeProfit("BTCUSDT", "LONG", 2, 110); err != nil {
			t.Fatal(err)
		}
		h.src.setMark("BTCUSDT", 111)
		h.paper.Tick()
		if got := h.onlyClosed(t).CloseReason; got != "take_profit" {
			t.Fatalf("close_reason = %q, want take_profit", got)
		}
	})

	t.Run("liquidation", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 10, 10)
		liq := mustPositions(t, h.paper)[0]["liquidationPrice"].(float64)
		h.src.setMark("BTCUSDT", liq-0.01)
		h.paper.Tick()
		if got := h.onlyClosed(t).CloseReason; got != "liquidation" {
			t.Fatalf("close_reason = %q, want liquidation", got)
		}
	})
}

// recordPaperSystemFill takes the reason straight from the fill; an empty one must not leave the
// close_reason blank (it falls back to "sync").
func TestRecordPaperSystemFillReasonPassthrough(t *testing.T) {
	h := newCloseReasonHarness(t)
	h.openLong(t, 2, 10)
	h.at.recordPaperSystemFill(PaperFill{
		OrderID:      "paper-fill-1",
		Symbol:       "BTCUSDT",
		Action:       "close_long",
		PositionSide: "LONG",
		Quantity:     2,
		Price:        95,
		Leverage:     10,
		Reason:       "",
	})
	if got := h.onlyClosed(t).CloseReason; got != store.CloseReasonSync {
		t.Fatalf("close_reason = %q, want %q", got, store.CloseReasonSync)
	}
}

// Closes that do not come from the matcher: the AI cycle ("ai"), the one-click close API
// ("manual") and the drawdown monitor ("drawdown").
func TestRecordedCloseReasonBySource(t *testing.T) {
	t.Run("ai", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 2, 10)
		order, err := h.paper.CloseLong("BTCUSDT", 0)
		if err != nil {
			t.Fatal(err)
		}
		h.at.recordAndConfirmOrder(order, "BTCUSDT", "close_long", 2, 100, 0, 100)
		if got := h.onlyClosed(t).CloseReason; got != "ai" {
			t.Fatalf("close_reason = %q, want ai", got)
		}
	})

	t.Run("manual", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 2, 10)
		order, err := h.paper.CloseLong("BTCUSDT", 0)
		if err != nil {
			t.Fatal(err)
		}
		h.at.RecordManualClose("BTCUSDT", "long", 2, 100, order)
		if got := h.onlyClosed(t).CloseReason; got != "manual" {
			t.Fatalf("close_reason = %q, want manual", got)
		}
	})

	t.Run("drawdown", func(t *testing.T) {
		h := newCloseReasonHarness(t)
		h.openLong(t, 2, 10)
		if err := h.at.emergencyClosePosition("BTCUSDT", "long"); err != nil {
			t.Fatal(err)
		}
		if got := h.onlyClosed(t).CloseReason; got != "drawdown" {
			t.Fatalf("close_reason = %q, want drawdown", got)
		}
	})

	t.Run("paper_state_lost", func(t *testing.T) {
		if paperStateLostReason != "paper_state_lost" {
			t.Fatalf("paperStateLostReason = %q", paperStateLostReason)
		}
	})
}

// The paper matcher's reasons are stored verbatim as close_reason, so its vocabulary must stay
// identical to the store constants (and to the frontend badges that translate them).
func TestPaperReasonVocabularyMatchesStore(t *testing.T) {
	for _, c := range []struct{ paper, store string }{
		{paperReasonManual, store.CloseReasonManual},
		{paperReasonStopLoss, store.CloseReasonStopLoss},
		{paperReasonTakeProfit, store.CloseReasonTakeProfit},
		{paperReasonLiquidation, store.CloseReasonLiquidation},
	} {
		if c.paper != c.store {
			t.Errorf("paper reason %q != store close reason %q", c.paper, c.store)
		}
	}
}
