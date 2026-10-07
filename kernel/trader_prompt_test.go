package kernel

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"nofx/store"
)

func promptTestEngine(strategyPrompt string) *StrategyEngine {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CustomPrompt = strategyPrompt
	return NewStrategyEngine(&cfg)
}

const (
	strategyHeading  = "# 📌 Personalized Trading Strategy"
	traderHeading    = "# 📌 Trader-Specific Instructions"
	outputFormatHead = "# Output Format (Strictly Follow)"
)

// Core sections that response parsing and risk control depend on must be present in every mode.
func assertCoreSections(t *testing.T, sp string) {
	t.Helper()
	for _, want := range []string{outputFormatHead, "<reasoning>", "<decision>", "## Field Description", "# 📋 Decision Process"} {
		if !strings.Contains(sp, want) {
			t.Errorf("core section %q missing from the system prompt", want)
		}
	}
}

func TestTraderPromptSupplementsStrategyPrompt(t *testing.T) {
	e := promptTestEngine("STRATEGY-PROMPT-TEXT")
	e.SetTraderPrompt("TRADER-PROMPT-TEXT", false)
	sp := e.BuildSystemPrompt(10000, "balanced")

	for _, want := range []string{strategyHeading, "STRATEGY-PROMPT-TEXT", traderHeading, "TRADER-PROMPT-TEXT"} {
		if !strings.Contains(sp, want) {
			t.Errorf("supplement mode: %q missing", want)
		}
	}
	if strings.Index(sp, "STRATEGY-PROMPT-TEXT") > strings.Index(sp, "TRADER-PROMPT-TEXT") {
		t.Error("trader prompt must come after the strategy prompt")
	}
	assertCoreSections(t, sp)
}

func TestTraderPromptOverrideReplacesStrategyPrompt(t *testing.T) {
	e := promptTestEngine("STRATEGY-PROMPT-TEXT")
	e.SetTraderPrompt("  TRADER-PROMPT-TEXT  ", true)
	sp := e.BuildSystemPrompt(10000, "balanced")

	if strings.Contains(sp, "STRATEGY-PROMPT-TEXT") {
		t.Error("override: the strategy's own CustomPrompt must be replaced")
	}
	if strings.Contains(sp, traderHeading) {
		t.Error("override: the trader prompt takes the strategy section, not a second one")
	}
	if strings.Count(sp, strategyHeading) != 1 || !strings.Contains(sp, strategyHeading+"\n\nTRADER-PROMPT-TEXT\n") {
		t.Errorf("override: trader prompt must sit under the Personalized Trading Strategy heading")
	}
	assertCoreSections(t, sp)
	if !strings.Contains(sp, "cannot violate the basic risk control principles") {
		t.Error("override must keep the risk-control reminder")
	}

	// Without a strategy CustomPrompt the override still yields the section.
	e2 := promptTestEngine("")
	e2.SetTraderPrompt("ONLY-TRADER", true)
	if sp := e2.BuildSystemPrompt(10000, "balanced"); !strings.Contains(sp, strategyHeading+"\n\nONLY-TRADER\n") {
		t.Error("override without a strategy prompt must still render the trader prompt")
	}
}

func TestTraderPromptOverrideWithEmptyPromptKeepsStrategyPrompt(t *testing.T) {
	e := promptTestEngine("STRATEGY-PROMPT-TEXT")
	e.SetTraderPrompt("   ", true)
	sp := e.BuildSystemPrompt(10000, "balanced")
	if !strings.Contains(sp, "STRATEGY-PROMPT-TEXT") || strings.Contains(sp, traderHeading) {
		t.Error("an empty trader prompt has nothing to replace the strategy prompt with")
	}
}

func TestTraderPromptCanBeSwitchedAndCleared(t *testing.T) {
	e := promptTestEngine("STRATEGY-PROMPT-TEXT")
	e.SetTraderPrompt("T1", true)
	e.SetTraderPrompt("T2", false)
	sp := e.BuildSystemPrompt(10000, "balanced")
	if !strings.Contains(sp, "STRATEGY-PROMPT-TEXT") || !strings.Contains(sp, "T2") || strings.Contains(sp, "T1") {
		t.Error("switching from override to supplement must restore the strategy prompt")
	}
	e.SetTraderPrompt("", false)
	sp = e.BuildSystemPrompt(10000, "balanced")
	if strings.Contains(sp, traderHeading) || !strings.Contains(sp, "STRATEGY-PROMPT-TEXT") {
		t.Error("clearing the trader prompt must remove its section")
	}
}

// SetTraderPrompt runs on an HTTP goroutine while the trading loop builds prompts: run with -race.
func TestTraderPromptConcurrentWithBuildSystemPrompt(t *testing.T) {
	e := promptTestEngine("STRATEGY-PROMPT-TEXT")
	e.SetTraderPrompt("TRADER-init", false) // start from a state with a trader prompt set
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.SetTraderPrompt(fmt.Sprintf("TRADER-%d", i), i%2 == 0)
		}
	}()

	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 40; i++ {
				sp := e.BuildSystemPrompt(10000, "balanced")
				// whatever the snapshot, it must be one of the two layouts: supplement (strategy
				// prompt + trader section) or override (neither: the trader prompt took the
				// strategy section)
				hasStrategy := strings.Contains(sp, "STRATEGY-PROMPT-TEXT")
				hasTraderSection := strings.Contains(sp, traderHeading)
				if hasStrategy != hasTraderSection {
					t.Errorf("inconsistent prompt snapshot: strategy=%v traderSection=%v", hasStrategy, hasTraderSection)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	wg.Wait()
}
