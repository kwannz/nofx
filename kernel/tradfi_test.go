package kernel

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/provider/bitget"
	"nofx/store"
)

var testContracts = map[string]bitget.Contract{
	"BTCUSDT":    {Symbol: "BTCUSDT", BaseCoin: "BTC", MaxLever: 125, SymbolStatus: "normal"},
	"SOLUSDT":    {Symbol: "SOLUSDT", BaseCoin: "SOL", MaxLever: 75, SymbolStatus: "normal"},
	"NVDAUSDT":   {Symbol: "NVDAUSDT", BaseCoin: "NVDA", MaxLever: 100, SymbolStatus: "normal", IsRwa: true},
	"LOWUSDT":    {Symbol: "LOWUSDT", BaseCoin: "LOW", MaxLever: 3, SymbolStatus: "normal", IsRwa: true},
	"XAUUSDT":    {Symbol: "XAUUSDT", BaseCoin: "XAU", MaxLever: 100, SymbolStatus: "normal", IsRwa: true},
	"EURUSDUSDT": {Symbol: "EURUSDUSDT", BaseCoin: "EURUSD", MaxLever: 100, SymbolStatus: "normal", IsRwa: true},
	"HALTUSDT":   {Symbol: "HALTUSDT", BaseCoin: "HALT", MaxLever: 10, SymbolStatus: "maintain", IsRwa: true},
}

// withEnv injects a fake clock and contract metadata.
func withEnv(t *testing.T, now time.Time) {
	t.Helper()
	oldNow, oldLookup := nowFunc, bitgetContractLookup
	nowFunc = func() time.Time { return now }
	bitgetContractLookup = func(sym string) (*bitget.Contract, error) {
		c, ok := testContracts[sym]
		if !ok {
			return nil, fmt.Errorf("contract %s not found", sym)
		}
		return &c, nil
	}
	t.Cleanup(func() { nowFunc, bitgetContractLookup = oldNow, oldLookup })
}

func et(y int, m time.Month, d, h int) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(y, m, d, h, 0, 0, 0, loc)
}

func tradFiEnv() *validationEnv {
	return &validationEnv{Bitget: true, EquityLeverage: 5, CommodityLeverage: 10}
}

func openDecision(sym string, lev int) *Decision {
	return &Decision{Symbol: sym, Action: "open_long", Leverage: lev, PositionSizeUSD: 100, StopLoss: 90, TakeProfit: 150, Confidence: 80}
}

func TestValidateDecisionLeverageByAssetClass(t *testing.T) {
	// Wednesday 11:00 ET: everything open.
	withEnv(t, et(2026, 1, 7, 11))
	tests := []struct {
		symbol  string
		lev     int
		wantLev int
	}{
		{"BTCUSDT", 50, 5},     // btc/eth cap (btcEth=5)
		{"SOLUSDT", 50, 4},     // altcoin cap (4)
		{"NVDAUSDT", 50, 5},    // equity cap
		{"XAUUSDT", 50, 10},    // commodity cap
		{"EURUSDUSDT", 50, 10}, // fx uses commodity cap
		{"NVDAUSDT", 3, 3},     // below cap untouched
		{"LOWUSDT", 5, 3},      // contract maxLever 3 < equity cap 5
	}
	for _, tt := range tests {
		d := openDecision(tt.symbol, tt.lev)
		if err := validateDecisionEnv(d, 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
			t.Errorf("%s: unexpected error %v", tt.symbol, err)
			continue
		}
		if d.Leverage != tt.wantLev {
			t.Errorf("%s: leverage = %d, want %d", tt.symbol, d.Leverage, tt.wantLev)
		}
	}
}

func TestValidateDecisionClosedSession(t *testing.T) {
	saturday := et(2026, 1, 3, 12)
	withEnv(t, saturday)

	for _, sym := range []string{"NVDAUSDT", "XAUUSDT", "EURUSDUSDT"} {
		for _, act := range []string{"open_long", "open_short"} {
			d := openDecision(sym, 3)
			d.Action = act
			if act == "open_short" {
				d.StopLoss, d.TakeProfit = 150, 90
			}
			err := validateDecisionEnv(d, 10000, 5, 4, 5, 1, tradFiEnv())
			if err == nil || !strings.Contains(err.Error(), "market closed") {
				t.Errorf("%s %s on Saturday: want market closed error, got %v", sym, act, err)
			}
		}
		// closing is always allowed
		for _, act := range []string{"close_long", "close_short", "hold", "wait"} {
			d := &Decision{Symbol: sym, Action: act}
			if err := validateDecisionEnv(d, 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
				t.Errorf("%s %s on Saturday should be allowed: %v", sym, act, err)
			}
		}
	}

	// Crypto still trades on Saturday.
	if err := validateDecisionEnv(openDecision("BTCUSDT", 3), 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
		t.Errorf("BTCUSDT should open on Saturday: %v", err)
	}
}

func TestValidateDecisionHaltedContractAndMissingMetadata(t *testing.T) {
	withEnv(t, et(2026, 1, 7, 11))
	err := validateDecisionEnv(openDecision("HALTUSDT", 3), 10000, 5, 4, 5, 1, tradFiEnv())
	if err == nil || !strings.Contains(err.Error(), "market closed") {
		t.Errorf("halted contract should be rejected, got %v", err)
	}
	err = validateDecisionEnv(openDecision("GHOSTUSDT", 3), 10000, 5, 4, 5, 1, tradFiEnv())
	if err == nil || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Errorf("unknown contract should be rejected, got %v", err)
	}
}

func TestValidateDecisionLegacyEnvUnchanged(t *testing.T) {
	// nil env must not consult metadata or the clock: Saturday NVDA passes as before.
	withEnv(t, et(2026, 1, 3, 12))
	bitgetContractLookup = func(string) (*bitget.Contract, error) {
		t.Fatal("legacy path must not look up Bitget contracts")
		return nil, nil
	}
	d := openDecision("NVDAUSDT", 50)
	if err := validateDecisionEnv(d, 10000, 5, 4, 5, 1, nil); err != nil {
		t.Fatal(err)
	}
	if d.Leverage != 4 {
		t.Errorf("legacy leverage = %d, want altcoin cap 4", d.Leverage)
	}
}

func TestValidateDecisionsClampPersists(t *testing.T) {
	withEnv(t, et(2026, 1, 7, 11))
	ds := []Decision{*openDecision("NVDAUSDT", 50)}
	if err := validateDecisions(ds, 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
		t.Fatal(err)
	}
	if ds[0].Leverage != 5 {
		t.Errorf("clamped leverage not written back: %d", ds[0].Leverage)
	}
}

func TestRiskControlLeverageFallback(t *testing.T) {
	rc := store.RiskControlConfig{AltcoinMaxLeverage: 7}
	if rc.EffectiveEquityMaxLeverage() != 7 || rc.EffectiveCommodityMaxLeverage() != 7 {
		t.Error("zero values must fall back to altcoin leverage")
	}
	rc.EquityMaxLeverage, rc.CommodityMaxLeverage = 3, 12
	if rc.EffectiveEquityMaxLeverage() != 3 || rc.EffectiveCommodityMaxLeverage() != 12 {
		t.Error("explicit values must be used")
	}
	def := store.GetDefaultStrategyConfig("en").RiskControl
	if def.EquityMaxLeverage != 5 || def.CommodityMaxLeverage != 10 {
		t.Errorf("defaults = %d/%d, want 5/10", def.EquityMaxLeverage, def.CommodityMaxLeverage)
	}
}

func TestTradFiPromptLine(t *testing.T) {
	withEnv(t, et(2026, 1, 3, 12)) // Saturday
	cfg := store.GetDefaultStrategyConfig("en")
	e := NewStrategyEngine(&cfg)
	if e.tradFiPromptLine("NVDAUSDT") != "" {
		t.Error("default source must not emit instrument lines")
	}
	e.SetMarketSource(market.SourceBitget)
	line := e.tradFiPromptLine("NVDAUSDT")
	for _, want := range []string{"class=equity", "CLOSED", "max leverage allowed=5x"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
	if l := e.tradFiPromptLine("XAUUSDT"); !strings.Contains(l, "class=commodity") || !strings.Contains(l, "10x") {
		t.Errorf("bad commodity line: %s", l)
	}
	if l := e.tradFiPromptLine("BTCUSDT"); !strings.Contains(l, "class=crypto") || !strings.Contains(l, "OPEN") {
		t.Errorf("bad crypto line: %s", l)
	}
	if l := e.tradFiPromptLine("LOWUSDT"); !strings.Contains(l, "3x") {
		t.Errorf("contract maxLever must cap displayed leverage: %s", l)
	}
	if !e.isNonCrypto("NVDAUSDT") || e.isNonCrypto("BTCUSDT") {
		t.Error("isNonCrypto misclassified")
	}
}

func TestDropUntradableOpensKeepsOtherDecisions(t *testing.T) {
	withEnv(t, et(2026, 1, 3, 12)) // Saturday: TradFi closed, crypto open

	ds := []Decision{
		*openDecision("NVDAUSDT", 3),
		{Symbol: "XAUUSDT", Action: "close_long"},
		*openDecision("BTCUSDT", 3),
	}
	got := dropUntradableOpens(ds, tradFiEnv())
	if len(got) != 2 || got[0].Symbol != "XAUUSDT" || got[1].Symbol != "BTCUSDT" {
		t.Fatalf("want closed-session open dropped and others kept, got %+v", got)
	}
	if err := validateDecisions(got, 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
		t.Fatalf("remaining decisions should validate: %v", err)
	}

	// Legacy env: untouched.
	legacy := []Decision{*openDecision("NVDAUSDT", 3)}
	if out := dropUntradableOpens(legacy, nil); len(out) != 1 {
		t.Fatalf("nil env must not drop decisions")
	}
}

func TestDropUntradableOpensDropsUnknownSymbol(t *testing.T) {
	withEnv(t, et(2026, 1, 7, 11)) // Wednesday: everything open

	ds := []Decision{
		{Symbol: "BTCUSDT", Action: "close_long"},
		*openDecision("FOOUSDT", 3), // not in the contract list: lookup fails
		*openDecision("SOLUSDT", 3),
	}
	got := dropUntradableOpens(ds, tradFiEnv())
	if len(got) != 2 || got[0].Symbol != "BTCUSDT" || got[0].Action != "close_long" || got[1].Symbol != "SOLUSDT" {
		t.Fatalf("unknown-symbol open must be dropped, others kept: %+v", got)
	}
	if err := validateDecisions(got, 10000, 5, 4, 5, 1, tradFiEnv()); err != nil {
		t.Fatalf("remaining decisions should validate: %v", err)
	}

	// Without the drop the unknown open would reject the whole batch.
	if err := validateDecisions(ds, 10000, 5, 4, 5, 1, tradFiEnv()); err == nil {
		t.Fatal("sanity: the unfiltered batch is expected to fail validation")
	}

	// A lookup (network) error is treated the same way.
	bitgetContractLookup = func(string) (*bitget.Contract, error) { return nil, fmt.Errorf("network down") }
	only := []Decision{{Symbol: "BTCUSDT", Action: "close_long"}, *openDecision("BTCUSDT", 3)}
	if out := dropUntradableOpens(only, tradFiEnv()); len(out) != 1 || out[0].Action != "close_long" {
		t.Fatalf("open with failing lookup must be dropped, close kept: %+v", out)
	}
}

func TestBuildSystemPromptIncludesTraderPrompt(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	e := NewStrategyEngine(&cfg)
	if strings.Contains(e.BuildSystemPrompt(10000, "balanced"), "Trader-Specific Instructions") {
		t.Fatal("no trader prompt set: section must be absent")
	}
	e.SetTraderPrompt("  keep at most 2 positions  ", false)
	sp := e.BuildSystemPrompt(10000, "balanced")
	if !strings.Contains(sp, "Trader-Specific Instructions") || !strings.Contains(sp, "keep at most 2 positions") {
		t.Fatal("trader prompt must be included in the system prompt")
	}
}
