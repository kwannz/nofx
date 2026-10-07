package kernel

import (
	"strings"
	"testing"
)

// TestLeverageFallback tests automatic correction when leverage exceeds limit
func TestLeverageFallback(t *testing.T) {
	tests := []struct {
		name            string
		decision        Decision
		accountEquity   float64
		btcEthLeverage  int
		altcoinLeverage int
		wantLeverage    int // Expected leverage after correction
		wantError       bool
	}{
		{
			name: "Altcoin leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5, // Limit 5x
			wantLeverage:    5, // Should be corrected to 5
			wantError:       false,
		},
		{
			name: "BTC leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "BTCUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 1000,
				StopLoss:        90000,
				TakeProfit:      110000,
			},
			accountEquity:   100,
			btcEthLeverage:  10, // Limit 10x
			altcoinLeverage: 5,
			wantLeverage:    10, // Should be corrected to 10
			wantError:       false,
		},
		{
			name: "Leverage within limit - no correction",
			decision: Decision{
				Symbol:          "ETHUSDT",
				Action:          "open_short",
				Leverage:        5, // Not exceeded
				PositionSizeUSD: 500,
				StopLoss:        4000,
				TakeProfit:      3000,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    5, // Stays unchanged
			wantError:       false,
		},
		{
			name: "Leverage is 0 - should error",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        0, // Invalid
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    0,
			wantError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use default position value ratios for testing (10x for BTC/ETH, 1.5x for altcoins)
			err := validateDecision(&tt.decision, tt.accountEquity, tt.btcEthLeverage, tt.altcoinLeverage, 10.0, 1.5)

			// Check error status
			if (err != nil) != tt.wantError {
				t.Errorf("validateDecision() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// If shouldn't error, check if leverage was correctly corrected
			if !tt.wantError && tt.decision.Leverage != tt.wantLeverage {
				t.Errorf("Leverage not corrected: got %d, want %d", tt.decision.Leverage, tt.wantLeverage)
			}
		})
	}
}


// contains checks if string contains substring (helper function)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// openDecisionOf builds a long open with a valid SL/TP (risk/reward 4:1) for the position-size tests.
func openDecisionOf(symbol string, size, riskUSD float64) Decision {
	return Decision{
		Symbol: symbol, Action: "open_long", Leverage: 3, PositionSizeUSD: size,
		StopLoss: 90, TakeProfit: 150, Confidence: 80, RiskUSD: riskUSD,
	}
}

// TestPositionSizeClamp: an oversized position_size_usd is clamped to the limit (like leverage)
// instead of failing validation.
func TestPositionSizeClamp(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		equity   float64
		wantSize float64
		wantRisk float64
	}{
		{
			// the observed production case: NVDA asked for 25000 with 9978 equity at 1.0x
			name:     "altcoin over the limit is clamped and risk_usd scaled",
			decision: openDecisionOf("NVDAUSDT", 25000, 500),
			equity:   9978, wantSize: 9978, wantRisk: 500 * 9978.0 / 25000.0,
		},
		{
			name:     "BTC over the limit is clamped",
			decision: openDecisionOf("BTCUSDT", 200000, 0),
			equity:   10000, wantSize: 100000, wantRisk: 0, // 10x ratio
		},
		{
			name:     "clamp floors to cents",
			decision: openDecisionOf("SOLUSDT", 50000, 100),
			equity:   1234.5678, wantSize: 1234.56, wantRisk: 100 * 1234.56 / 50000.0,
		},
		{
			name:     "within 1% tolerance is left alone",
			decision: openDecisionOf("SOLUSDT", 10050, 100),
			equity:   10000, wantSize: 10050, wantRisk: 100,
		},
		{
			name:     "just beyond the tolerance is clamped",
			decision: openDecisionOf("SOLUSDT", 10101, 100),
			equity:   10000, wantSize: 10000, wantRisk: 100 * 10000.0 / 10101.0,
		},
		{
			name:     "below the limit is untouched",
			decision: openDecisionOf("SOLUSDT", 5000, 80),
			equity:   10000, wantSize: 5000, wantRisk: 80,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.decision
			// BTC/ETH ratio 10x, altcoin ratio 1x
			if err := validateDecision(&d, tt.equity, 10, 5, 10.0, 1.0); err != nil {
				t.Fatalf("oversized position must be clamped, got error: %v", err)
			}
			near := func(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }
			if !near(d.PositionSizeUSD, tt.wantSize) {
				t.Errorf("position size = %v, want %v", d.PositionSizeUSD, tt.wantSize)
			}
			if !near(d.RiskUSD, tt.wantRisk) {
				t.Errorf("risk_usd = %v, want %v", d.RiskUSD, tt.wantRisk)
			}
		})
	}
}

// TestPositionSizeClampBelowMinimumStillErrors: when the allowed size is smaller than the exchange
// minimum there is nothing sensible to clamp to, so the decision is rejected as before.
func TestPositionSizeClampBelowMinimumStillErrors(t *testing.T) {
	d := openDecisionOf("SOLUSDT", 500, 10)
	err := validateDecision(&d, 5, 10, 5, 10.0, 1.0) // limit 5 USDT < minimum 12 USDT
	if err == nil || !strings.Contains(err.Error(), "below the minimum") {
		t.Fatalf("want below-minimum error, got %v", err)
	}
}

// TestValidateDecisionsOversizeDoesNotVoidCycle reproduces the production failure: one oversized
// open used to fail the whole batch, discarding the hold and the close next to it. The clamp must
// be visible in the slice that validateDecisions was given (it is what gets executed).
func TestValidateDecisionsOversizeDoesNotVoidCycle(t *testing.T) {
	ds := []Decision{
		{Symbol: "BTCUSDT", Action: "hold", Reasoning: "keep the short"},
		openDecisionOf("NVDAUSDT", 25000, 400),
		{Symbol: "ETHUSDT", Action: "close_long"},
	}
	if err := validateDecisions(ds, 9978, 10, 5, 10.0, 1.0, nil); err != nil {
		t.Fatalf("batch must validate: %v", err)
	}
	if ds[1].PositionSizeUSD != 9978 {
		t.Errorf("clamp not written back to the decision: %v", ds[1].PositionSizeUSD)
	}
	if ds[0].Action != "hold" || ds[2].Action != "close_long" {
		t.Errorf("other decisions must be untouched: %+v", ds)
	}
}

// TestValidateDecisionsOtherErrorsStillFail: only the size cap is lenient; a genuinely invalid
// decision (here a long with stop loss above take profit) still fails the batch.
func TestValidateDecisionsOtherErrorsStillFail(t *testing.T) {
	bad := openDecisionOf("SOLUSDT", 500, 10)
	bad.StopLoss, bad.TakeProfit = 150, 90
	ds := []Decision{openDecisionOf("NVDAUSDT", 25000, 400), bad}
	err := validateDecisions(ds, 9978, 10, 5, 10.0, 1.0, nil)
	if err == nil || !strings.Contains(err.Error(), "decision #2") {
		t.Fatalf("want decision #2 validation error, got %v", err)
	}
}

// TestParseFullDecisionResponseClampsOversize drives the real parse path with the model output that
// used to fail with "altcoin single coin position value cannot exceed".
func TestParseFullDecisionResponseClampsOversize(t *testing.T) {
	resp := "<reasoning>BTC short stays, add NVDA</reasoning>\n<decision>\n```json\n" +
		`[{"symbol":"BTCUSDT","action":"hold","reasoning":"keep"},` +
		`{"symbol":"NVDAUSDT","action":"open_long","leverage":3,"position_size_usd":25000,"stop_loss":90,"take_profit":150,"confidence":80,"risk_usd":500,"reasoning":"x"}]` +
		"\n```\n</decision>"
	fd, err := parseFullDecisionResponse(resp, 9978, 10, 5, 10.0, 1.0, nil)
	if err != nil {
		t.Fatalf("parse must succeed: %v", err)
	}
	if len(fd.Decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(fd.Decisions))
	}
	if got := fd.Decisions[1].PositionSizeUSD; got != 9978 {
		t.Errorf("executed position size = %v, want clamped 9978", got)
	}
}
