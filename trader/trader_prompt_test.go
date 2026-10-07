package trader

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"nofx/kernel"
	"nofx/store"
)

func newPromptTestTrader(strategyPrompt string) *AutoTrader {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CustomPrompt = strategyPrompt
	return &AutoTrader{strategyEngine: kernel.NewStrategyEngine(&cfg)}
}

// Both values must reach the engine whichever setter runs first.
func TestAutoTraderPromptSettersAreOrderIndependent(t *testing.T) {
	const strategyText, traderText = "STRATEGY-PROMPT-TEXT", "TRADER-PROMPT-TEXT"
	orders := map[string]func(at *AutoTrader, override bool){
		"prompt then override": func(at *AutoTrader, o bool) { at.SetCustomPrompt(traderText); at.SetOverrideBasePrompt(o) },
		"override then prompt": func(at *AutoTrader, o bool) { at.SetOverrideBasePrompt(o); at.SetCustomPrompt(traderText) },
	}
	for name, apply := range orders {
		for _, override := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/override=%v", name, override), func(t *testing.T) {
				at := newPromptTestTrader(strategyText)
				apply(at, override)
				sp := at.strategyEngine.BuildSystemPrompt(10000, "balanced")
				if !strings.Contains(sp, traderText) {
					t.Fatal("trader prompt did not reach the engine")
				}
				if got := strings.Contains(sp, strategyText); got == override {
					t.Fatalf("override=%v but strategy prompt present=%v", override, got)
				}
			})
		}
	}

	// Toggling override later (API update of an existing prompt) re-renders without resending the text.
	at := newPromptTestTrader(strategyText)
	at.SetCustomPrompt(traderText)
	if !strings.Contains(at.strategyEngine.BuildSystemPrompt(10000, "balanced"), strategyText) {
		t.Fatal("supplement mode keeps the strategy prompt")
	}
	at.SetOverrideBasePrompt(true)
	if strings.Contains(at.strategyEngine.BuildSystemPrompt(10000, "balanced"), strategyText) {
		t.Fatal("override must replace the strategy prompt")
	}
}

// The API handler updates the prompt while the trading loop reads it (run with -race).
func TestAutoTraderPromptSettersConcurrent(t *testing.T) {
	at := newPromptTestTrader("STRATEGY-PROMPT-TEXT")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				at.SetCustomPrompt("p")
				at.SetOverrideBasePrompt(j%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = at.strategyEngine.BuildSystemPrompt(10000, "balanced")
			}
		}()
	}
	wg.Wait()
}
