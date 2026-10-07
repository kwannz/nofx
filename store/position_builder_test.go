package store

import (
	"math"
	"path/filepath"
	"testing"
)

const testTraderID = "close-reason-trader"

func newTestPositionBuilder(t *testing.T) (*PositionBuilder, *PositionStore) {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "position_builder.db"))
	if err != nil {
		t.Fatal(err)
	}
	return NewPositionBuilder(st.Position()), st.Position()
}

func openTestPosition(t *testing.T, pb *PositionBuilder, qty float64) {
	t.Helper()
	if err := pb.ProcessTrade(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "open_long",
		qty, 100, 0.1, 0, 1_000, "open-1"); err != nil {
		t.Fatal(err)
	}
}

func onlyClosedPosition(t *testing.T, ps *PositionStore) *TraderPosition {
	t.Helper()
	if open, err := ps.GetOpenPositions(testTraderID); err != nil || len(open) != 0 {
		t.Fatalf("expected no open positions, got %v (err %v)", open, err)
	}
	closed, err := ps.GetClosedPositions(testTraderID, 10)
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed positions: %v %v", closed, err)
	}
	return closed[0]
}

// open -> close with a reason: the CLOSED row stores that reason.
func TestProcessTradeWithReasonRecordsCloseReason(t *testing.T) {
	for _, reason := range []string{
		CloseReasonAI, CloseReasonManual, CloseReasonStopLoss, CloseReasonTakeProfit,
		CloseReasonLiquidation, CloseReasonDrawdown, CloseReasonSync,
	} {
		t.Run(reason, func(t *testing.T) {
			pb, ps := newTestPositionBuilder(t)
			openTestPosition(t, pb, 2)
			if err := pb.ProcessTradeWithReason(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "close_long",
				2, 110, 0.1, 0, 2_000, "close-1", reason); err != nil {
				t.Fatal(err)
			}
			pos := onlyClosedPosition(t, ps)
			if pos.Status != "CLOSED" || pos.CloseReason != reason {
				t.Fatalf("status/close_reason = %q/%q, want CLOSED/%q", pos.Status, pos.CloseReason, reason)
			}
			if math.Abs(pos.ExitPrice-110) > 1e-9 || math.Abs(pos.RealizedPnL-20) > 1e-9 {
				t.Fatalf("exit/pnl = %v/%v, want 110/20", pos.ExitPrice, pos.RealizedPnL)
			}
		})
	}
}

// ProcessTrade (used by the exchange order syncs) keeps recording "sync", and so does an empty reason.
func TestProcessTradeDefaultsToSyncReason(t *testing.T) {
	pb, ps := newTestPositionBuilder(t)
	openTestPosition(t, pb, 1)
	if err := pb.ProcessTrade(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "close_long",
		1, 105, 0.1, 0, 2_000, "close-1"); err != nil {
		t.Fatal(err)
	}
	if got := onlyClosedPosition(t, ps).CloseReason; got != "sync" {
		t.Fatalf("ProcessTrade close_reason = %q, want sync", got)
	}

	pb, ps = newTestPositionBuilder(t)
	openTestPosition(t, pb, 1)
	if err := pb.ProcessTradeWithReason(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "close_long",
		1, 105, 0.1, 0, 2_000, "close-1", ""); err != nil {
		t.Fatal(err)
	}
	if got := onlyClosedPosition(t, ps).CloseReason; got != "sync" {
		t.Fatalf("empty reason close_reason = %q, want sync", got)
	}
}

// A partial close keeps the position OPEN without a close_reason; the reason of the final close
// is the one that is stored.
func TestProcessTradeWithReasonPartialThenFinalClose(t *testing.T) {
	pb, ps := newTestPositionBuilder(t)
	openTestPosition(t, pb, 2)

	if err := pb.ProcessTradeWithReason(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "close_long",
		1, 110, 0.1, 0, 2_000, "close-1", CloseReasonTakeProfit); err != nil {
		t.Fatal(err)
	}
	open, err := ps.GetOpenPositions(testTraderID)
	if err != nil || len(open) != 1 {
		t.Fatalf("open positions after the partial close: %v %v", open, err)
	}
	if open[0].Status != "OPEN" || open[0].CloseReason != "" || math.Abs(open[0].Quantity-1) > 1e-9 {
		t.Fatalf("partial close must leave an OPEN position without reason, got %+v", open[0])
	}

	if err := pb.ProcessTradeWithReason(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "close_long",
		1, 90, 0.1, 0, 3_000, "close-2", CloseReasonStopLoss); err != nil {
		t.Fatal(err)
	}
	if got := onlyClosedPosition(t, ps).CloseReason; got != "stop_loss" {
		t.Fatalf("final close_reason = %q, want stop_loss", got)
	}
}

// The reason is meaningless for opens and must not be stored on the OPEN row.
func TestProcessTradeWithReasonIgnoredOnOpen(t *testing.T) {
	pb, ps := newTestPositionBuilder(t)
	if err := pb.ProcessTradeWithReason(testTraderID, "ex-1", "bitget_paper", "BTCUSDT", "LONG", "open_long",
		1, 100, 0.1, 0, 1_000, "open-1", CloseReasonStopLoss); err != nil {
		t.Fatal(err)
	}
	open, err := ps.GetOpenPositions(testTraderID)
	if err != nil || len(open) != 1 || open[0].CloseReason != "" {
		t.Fatalf("open position: %+v %v", open, err)
	}
}
