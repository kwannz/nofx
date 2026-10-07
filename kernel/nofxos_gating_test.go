package kernel

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"

	"nofx/logger"
	"nofx/market"
	"nofx/provider/nofxos"
	"nofx/store"
)

// fakeNofxos is a counting, network-free stand-in for *nofxos.Client.
type fakeNofxos struct {
	mu    sync.Mutex
	calls []string // "GetCoinData:BTCUSDT", "GetOIRanking", ...

	// availableErr is returned by Available() (simulates an open circuit breaker).
	availableErr error
	// fetchErr is returned by every fetch method (simulates the breaker tripping mid-call).
	fetchErr error
}

func (f *fakeNofxos) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeNofxos) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNofxos) Available() error { return f.availableErr }

func (f *fakeNofxos) GetCoinData(symbol, include string) (*nofxos.QuantData, error) {
	f.record("GetCoinData:" + symbol)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return &nofxos.QuantData{Symbol: symbol, Price: 1}, nil
}

func (f *fakeNofxos) GetOIRanking(duration string, limit int) (*nofxos.OIRankingData, error) {
	f.record("GetOIRanking")
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return &nofxos.OIRankingData{Duration: duration}, nil
}

func (f *fakeNofxos) GetNetFlowRanking(duration string, limit int) (*nofxos.NetFlowRankingData, error) {
	f.record("GetNetFlowRanking")
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return &nofxos.NetFlowRankingData{Duration: duration}, nil
}

func (f *fakeNofxos) GetPriceRanking(durations string, limit int) (*nofxos.PriceRankingData, error) {
	f.record("GetPriceRanking")
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return &nofxos.PriceRankingData{Durations: map[string]*nofxos.PriceRankingDuration{"1h": {}}}, nil
}

func (f *fakeNofxos) GetOITopPositions() ([]nofxos.OIPosition, error) {
	f.record("GetOITopPositions")
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return []nofxos.OIPosition{{Symbol: "PEPEUSDT", Rank: 1, OIDeltaPercent: 5}}, nil
}

func (f *fakeNofxos) GetTopRatedCoins(limit int) ([]string, error) {
	f.record("GetTopRatedCoins")
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return []string{"BTCUSDT"}, nil
}

// engineWithFake builds an engine (all NofxOS indicators enabled by the default config) wired
// to a fake NofxOS client.
func engineWithFake(t *testing.T, src market.Source) (*StrategyEngine, *fakeNofxos) {
	t.Helper()
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.Indicators.EnableQuantData = true
	cfg.Indicators.EnableOIRanking = true
	cfg.Indicators.EnableNetFlowRanking = true
	cfg.Indicators.EnablePriceRanking = true
	e := NewStrategyEngine(&cfg)
	fake := &fakeNofxos{}
	e.nofxosClient = fake
	if src != "" {
		e.SetMarketSource(src)
	}
	return e, fake
}

// captureLogs redirects the shared logger into a buffer for the test's duration.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	old := logger.Log.Out
	logger.Log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { logger.Log.SetOutput(old) })
	return &buf
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestBitgetEngineSkipsMarketWideNofxosData(t *testing.T) {
	e, fake := engineWithFake(t, market.SourceBitget)

	if got := e.FetchOIRankingData(); got != nil {
		t.Errorf("OI ranking must be skipped for Bitget, got %+v", got)
	}
	if got := e.FetchNetFlowRankingData(); got != nil {
		t.Errorf("NetFlow ranking must be skipped for Bitget, got %+v", got)
	}
	if got := e.FetchPriceRankingData(); got != nil {
		t.Errorf("Price ranking must be skipped for Bitget, got %+v", got)
	}
	m := e.fetchOITopDataMap()
	if m == nil || len(m) != 0 {
		t.Errorf("OI top map must be empty and non-nil for Bitget, got %v", m)
	}
	if calls := fake.callList(); len(calls) != 0 {
		t.Fatalf("Bitget engine must not call the NofxOS ranking endpoints, got %v", calls)
	}
}

// The same calls DO go out for the legacy (Binance) source, so the test above is meaningful.
func TestDefaultEngineStillFetchesMarketWideNofxosData(t *testing.T) {
	e, fake := engineWithFake(t, "")

	if e.FetchOIRankingData() == nil || e.FetchNetFlowRankingData() == nil || e.FetchPriceRankingData() == nil {
		t.Fatal("legacy source must still return ranking data")
	}
	if m := e.fetchOITopDataMap(); len(m) != 1 || m["PEPEUSDT"] == nil || m["PEPEUSDT"].Rank != 1 {
		t.Fatalf("legacy source must still build the OI top map, got %v", m)
	}
	want := []string{"GetOIRanking", "GetNetFlowRanking", "GetPriceRanking", "GetOITopPositions"}
	if got := fake.callList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestBitgetQuantDataSkipsTradFiSymbols(t *testing.T) {
	withEnv(t, et(2026, 1, 7, 11))
	symbols := []string{"BTCUSDT", "NVDAUSDT", "SOLUSDT", "XAUUSDT", "EURUSDUSDT"}

	e, fake := engineWithFake(t, market.SourceBitget)
	got := e.FetchQuantDataBatch(symbols)

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"BTCUSDT", "SOLUSDT"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("quant data keys = %v, want only the crypto symbols %v", keys, want)
	}
	if want := []string{"GetCoinData:BTCUSDT", "GetCoinData:SOLUSDT"}; !reflect.DeepEqual(fake.callList(), want) {
		t.Fatalf("NofxOS was called for non-crypto symbols: %v", fake.callList())
	}

	// A single TradFi lookup is a clean no-op, not an error.
	if d, err := e.FetchQuantData("NVDAUSDT"); d != nil || err != nil {
		t.Fatalf("FetchQuantData(NVDAUSDT) = %v, %v; want nil, nil", d, err)
	}

	// Legacy source: behaviour unchanged (every symbol is queried).
	e2, fake2 := engineWithFake(t, "")
	if got := e2.FetchQuantDataBatch(symbols); len(got) != len(symbols) {
		t.Fatalf("legacy source should query all %d symbols, got %d", len(symbols), len(got))
	}
	if len(fake2.callList()) != len(symbols) {
		t.Fatalf("legacy source calls = %v", fake2.callList())
	}
}

// A tripped breaker is handled without any per-call noise and without further requests.
func TestEngineTreatsNofxosUnavailableQuietly(t *testing.T) {
	logs := captureLogs(t)
	unavailable := fmt.Errorf("request failed: %w", &nofxos.UnavailableError{Reason: "key deprecated"})

	t.Run("breaker already open", func(t *testing.T) {
		e, fake := engineWithFake(t, "")
		fake.availableErr = unavailable

		if e.FetchOIRankingData() != nil || e.FetchNetFlowRankingData() != nil || e.FetchPriceRankingData() != nil {
			t.Error("rankings must be nil while unavailable")
		}
		if got := e.FetchQuantDataBatch([]string{"BTCUSDT", "ETHUSDT", "SOLUSDT"}); len(got) != 0 {
			t.Errorf("quant data must be empty while unavailable, got %v", got)
		}
		if m := e.fetchOITopDataMap(); m == nil || len(m) != 0 {
			t.Errorf("OI top map must be empty and non-nil, got %v", m)
		}
		if calls := fake.callList(); len(calls) != 0 {
			t.Errorf("no fetch call expected while unavailable, got %v", calls)
		}
	})

	t.Run("breaker trips mid-batch", func(t *testing.T) {
		e, fake := engineWithFake(t, "")
		fake.fetchErr = unavailable

		if got := e.FetchQuantDataBatch([]string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT"}); len(got) != 0 {
			t.Errorf("quant data must be empty, got %v", got)
		}
		if calls := fake.callList(); len(calls) != 1 {
			t.Errorf("batch must stop at the first ErrUnavailable, got %v", calls)
		}
		if e.FetchOIRankingData() != nil || e.FetchNetFlowRankingData() != nil || e.FetchPriceRankingData() != nil {
			t.Error("rankings must be nil")
		}
		if m := e.fetchOITopDataMap(); len(m) != 0 {
			t.Errorf("OI top map must be empty, got %v", m)
		}
	})

	t.Run("candidate coins keep static symbols", func(t *testing.T) {
		e, fake := engineWithFake(t, "")
		fake.fetchErr = unavailable
		e.config.CoinSource.SourceType = "mixed"
		e.config.CoinSource.UseAI500 = true
		e.config.CoinSource.UseOITop = true
		e.config.CoinSource.StaticCoins = []string{"BTCUSDT"}

		coins, err := e.GetCandidateCoins()
		if err != nil {
			t.Fatalf("GetCandidateCoins: %v", err)
		}
		if len(coins) != 1 || coins[0].Symbol != "BTCUSDT" {
			t.Fatalf("want the static coin only, got %+v", coins)
		}
	})

	// Nothing above may log at info or warning level: the client already warned once when the
	// breaker tripped, and "unavailable" is a debug-level event for callers.
	for _, noisy := range []string{"Failed", "⚠️", "WARN"} {
		if bytes.Contains(logs.Bytes(), []byte(noisy)) {
			t.Errorf("unexpected %q in logs while NofxOS is unavailable:\n%s", noisy, logs.String())
		}
	}
}

// A genuine (non-breaker) failure is still reported.
func TestEngineStillWarnsOnOtherNofxosErrors(t *testing.T) {
	logs := captureLogs(t)
	e, fake := engineWithFake(t, "")
	fake.fetchErr = errors.New("boom")

	if e.FetchOIRankingData() != nil {
		t.Fatal("want nil on error")
	}
	if !bytes.Contains(logs.Bytes(), []byte("Failed to fetch OI ranking data")) {
		t.Fatalf("non-breaker errors must still be logged:\n%s", logs.String())
	}
}

// Strategies whose coin source is "oi_top" must keep running while NofxOS is unavailable: the
// candidate list is empty (the pre-breaker behaviour) but there is no error, so the trading
// cycle still completes and the AI keeps managing open positions.
func TestOITopCandidateCoinsSurviveUnavailableNofxos(t *testing.T) {
	logs := captureLogs(t)
	unavailable := fmt.Errorf("request failed: %w", &nofxos.UnavailableError{Reason: "key deprecated"})

	t.Run("fake client returns ErrUnavailable", func(t *testing.T) {
		e, fake := engineWithFake(t, "")
		fake.fetchErr = unavailable
		e.config.CoinSource.SourceType = "oi_top"
		e.config.CoinSource.UseOITop = true
		e.config.CoinSource.StaticCoins = []string{"BTCUSDT"}

		coins, err := e.GetCandidateCoins()
		if err != nil {
			t.Fatalf("GetCandidateCoins must not fail while NofxOS is unavailable: %v", err)
		}
		if len(coins) != 0 {
			t.Fatalf("want an empty candidate list (pre-breaker behaviour), got %+v", coins)
		}
		if calls := fake.callList(); !reflect.DeepEqual(calls, []string{"GetOITopPositions"}) {
			t.Fatalf("calls = %v, want a single GetOITopPositions", calls)
		}
	})

	t.Run("excluded coins filter still applies", func(t *testing.T) {
		e, fake := engineWithFake(t, "")
		fake.fetchErr = unavailable
		e.config.CoinSource.SourceType = "oi_top"
		e.config.CoinSource.UseOITop = true
		e.config.CoinSource.ExcludedCoins = []string{"DOGEUSDT"}
		if coins, err := e.GetCandidateCoins(); err != nil || len(coins) != 0 {
			t.Fatalf("GetCandidateCoins = %+v, %v; want empty, nil", coins, err)
		}
	})

	// "Unavailable" is a debug-level event for callers.
	for _, noisy := range []string{"Failed", "ERROR"} {
		if bytes.Contains(logs.Bytes(), []byte(noisy)) {
			t.Errorf("unexpected %q in logs while NofxOS is unavailable:\n%s", noisy, logs.String())
		}
	}
}

// Only the breaker error is swallowed for "oi_top"; genuine failures still surface.
func TestOITopCandidateCoinsStillFailOnOtherErrors(t *testing.T) {
	e, fake := engineWithFake(t, "")
	fake.fetchErr = errors.New("boom")
	e.config.CoinSource.SourceType = "oi_top"
	e.config.CoinSource.UseOITop = true

	if _, err := e.GetCandidateCoins(); err == nil {
		t.Fatal("a non-breaker NofxOS error must still be returned for oi_top")
	}
}
