package manager

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"nofx/store"
	"nofx/trader"
)

// TestRemoveTrader tests removing trader from memory
func TestRemoveTrader(t *testing.T) {
	tm := NewTraderManager()

	// Create a mock trader and add it to map
	traderID := "test-trader-123"
	tm.traders[traderID] = nil // Use nil as placeholder, only need to verify deletion logic in test

	// Verify trader exists
	if _, exists := tm.traders[traderID]; !exists {
		t.Fatal("trader should exist in map")
	}

	// Call RemoveTrader
	tm.RemoveTrader(traderID)

	// Verify trader has been removed
	if _, exists := tm.traders[traderID]; exists {
		t.Error("trader should be removed from map")
	}
}

// TestRemoveTrader_NonExistent tests that removing non-existent trader doesn't error
func TestRemoveTrader_NonExistent(t *testing.T) {
	tm := NewTraderManager()

	// Trying to remove non-existent trader should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("removing non-existent trader should not panic: %v", r)
		}
	}()

	tm.RemoveTrader("non-existent-trader")
}

// TestRemoveTrader_Concurrent tests concurrent removal of trader safety
func TestRemoveTrader_Concurrent(t *testing.T) {
	tm := NewTraderManager()
	traderID := "test-trader-concurrent"

	// Add trader
	tm.traders[traderID] = nil

	// Concurrently call RemoveTrader
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			tm.RemoveTrader(traderID)
			done <- true
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify trader has been removed
	if _, exists := tm.traders[traderID]; exists {
		t.Error("trader should be removed from map")
	}
}

// TestGetTrader_AfterRemove tests that getting trader after removal returns error
func TestGetTrader_AfterRemove(t *testing.T) {
	tm := NewTraderManager()
	traderID := "test-trader-get"

	// Add trader
	tm.traders[traderID] = nil

	// Remove trader
	tm.RemoveTrader(traderID)

	// Try to get removed trader
	_, err := tm.GetTrader(traderID)
	if err == nil {
		t.Error("getting removed trader should return error")
	}
}

// TestNilTraderEntriesAreSkipped verifies that bulk operations over the trader
// map tolerate nil entries (as stored by the tests above) instead of panicking.
func TestNilTraderEntriesAreSkipped(t *testing.T) {
	tm := NewTraderManager()
	tm.traders["nil-trader"] = nil

	tm.StartAll()
	tm.StopAll()

	cmp, err := tm.GetComparisonData()
	if err != nil {
		t.Fatalf("GetComparisonData returned error: %v", err)
	}
	if cmp["count"] != 0 {
		t.Errorf("expected 0 comparable traders, got %v", cmp["count"])
	}

	if _, err := tm.GetCompetitionData(); err != nil {
		t.Fatalf("GetCompetitionData returned error: %v", err)
	}

	tm.RemoveTrader("nil-trader")
	if _, exists := tm.traders["nil-trader"]; exists {
		t.Error("nil trader should be removed from map")
	}
}

// newBitgetWiringStore returns a store holding the strategy addTraderFromStore requires.
func newBitgetWiringStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.GetDefaultStrategyConfig("en")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Strategy().Create(&store.Strategy{ID: "strategy-1", UserID: "user-1", Name: "s", Config: string(raw)}); err != nil {
		t.Fatal(err)
	}
	return st
}

func bitgetWiringTraderCfg(id string) *store.Trader {
	return &store.Trader{ID: id, UserID: "user-1", Name: id, AIModelID: "model-1", ExchangeID: "exchange-1",
		StrategyID: "strategy-1", InitialBalance: 5000, ScanIntervalMinutes: 3, IsCrossMargin: true}
}

var bitgetWiringAIModel = &store.AIModel{ID: "model-1", UserID: "user-1", Name: "m", Provider: "deepseek", Enabled: true}

// TestAddTraderFromStoreCopiesBitgetPositionMode: the exchange account's position mode reaches
// AutoTraderConfig for bitget and bitget_paper (normalized: empty / unknown = hedge), and other
// exchanges do not carry it.
func TestAddTraderFromStoreCopiesBitgetPositionMode(t *testing.T) {
	st := newBitgetWiringStore(t)

	var got trader.AutoTraderConfig
	orig := newAutoTrader
	t.Cleanup(func() { newAutoTrader = orig })
	errStop := errors.New("stop before building a real trader")
	newAutoTrader = func(cfg trader.AutoTraderConfig, _ *store.Store, _ string) (*trader.AutoTrader, error) {
		got = cfg
		return nil, errStop
	}

	for _, exchangeType := range []string{"bitget", "bitget_paper"} {
		for _, tc := range []struct{ stored, want string }{
			{"one_way", store.BitgetPositionModeOneWay},
			{"hedge", store.BitgetPositionModeHedge},
			{"", store.BitgetPositionModeHedge},
			{"bogus", store.BitgetPositionModeHedge},
		} {
			t.Run(exchangeType+"/"+tc.stored, func(t *testing.T) {
				tm := NewTraderManager()
				exchange := &store.Exchange{ID: "exchange-1", UserID: "user-1", ExchangeType: exchangeType, Enabled: true,
					APIKey: "k", SecretKey: "s", Passphrase: "p", BitgetPositionMode: tc.stored}
				got = trader.AutoTraderConfig{}
				err := tm.addTraderFromStore(bitgetWiringTraderCfg("t-"+exchangeType), bitgetWiringAIModel, exchange, st)
				if !errors.Is(err, errStop) {
					t.Fatalf("expected the stubbed constructor error, got %v", err)
				}
				if got.Exchange != exchangeType || got.BitgetPositionMode != tc.want {
					t.Errorf("AutoTraderConfig exchange %q BitgetPositionMode %q, want %q / %q", got.Exchange, got.BitgetPositionMode, exchangeType, tc.want)
				}
				if exchangeType == "bitget" && (got.BitgetAPIKey != "k" || got.BitgetSecretKey != "s" || got.BitgetPassphrase != "p") {
					t.Errorf("live Bitget credentials not copied: %+v", got)
				}
				if len(tm.traders) != 0 {
					t.Errorf("a failed trader must not be registered, got %d", len(tm.traders))
				}
			})
		}
	}

	t.Run("other exchanges do not carry the Bitget mode", func(t *testing.T) {
		tm := NewTraderManager()
		exchange := &store.Exchange{ID: "exchange-1", UserID: "user-1", ExchangeType: "binance", Enabled: true,
			APIKey: "k", SecretKey: "s", BitgetPositionMode: "one_way"}
		got = trader.AutoTraderConfig{}
		if err := tm.addTraderFromStore(bitgetWiringTraderCfg("t-binance"), bitgetWiringAIModel, exchange, st); !errors.Is(err, errStop) {
			t.Fatal(err)
		}
		if got.BitgetPositionMode != "" {
			t.Errorf("binance config carries BitgetPositionMode %q", got.BitgetPositionMode)
		}
	})
}

// TestAddTraderFromStorePaperPositionModeEndToEnd goes through the real AutoTrader factory: the
// stored mode of a bitget_paper exchange ends up as the paper account's position mode.
func TestAddTraderFromStorePaperPositionModeEndToEnd(t *testing.T) {
	st := newBitgetWiringStore(t)
	for _, tc := range []struct{ id, stored, want string }{
		{"manager-paper-one-way", "one_way", trader.BitgetPositionModeOneWay},
		{"manager-paper-hedge", "hedge", trader.BitgetPositionModeHedge},
		{"manager-paper-empty", "", trader.BitgetPositionModeHedge},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			defer trader.ReleaseBitgetPaperTrader(tc.id)
			tm := NewTraderManager()
			exchange := &store.Exchange{ID: "exchange-1", UserID: "user-1", ExchangeType: "bitget_paper", Enabled: true, BitgetPositionMode: tc.stored}
			if err := tm.addTraderFromStore(bitgetWiringTraderCfg(tc.id), bitgetWiringAIModel, exchange, st); err != nil {
				t.Fatalf("addTraderFromStore: %v", err)
			}
			at := tm.traders[tc.id]
			if at == nil {
				t.Fatal("trader not registered")
			}
			paper, ok := at.UnderlyingTrader().(*trader.BitgetPaperTrader)
			if !ok {
				t.Fatalf("underlying trader is %T", at.UnderlyingTrader())
			}
			if paper.PositionMode() != tc.want {
				t.Errorf("paper position mode %q, want %q", paper.PositionMode(), tc.want)
			}
		})
	}
}
