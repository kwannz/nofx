package kernel

import (
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/provider/bitget"
	"nofx/store"
)

// Package-level seams so tests never hit the network or the wall clock.
var (
	// nowFunc returns the current time used for trading-session checks.
	nowFunc = time.Now
	// bitgetContractLookup returns Bitget contract metadata for a symbol.
	bitgetContractLookup = func(symbol string) (*bitget.Contract, error) {
		return bitget.GetContract(symbol)
	}
)

// SetMarketSource selects where market data (and TradFi contract metadata)
// comes from. The zero value keeps the legacy behaviour.
func (e *StrategyEngine) SetMarketSource(src market.Source) {
	e.marketSource = src
}

// MarketSource returns the configured market data source.
func (e *StrategyEngine) MarketSource() market.Source {
	return e.marketSource
}

// normalize normalizes a symbol according to the engine's market source.
func (e *StrategyEngine) normalize(symbol string) string {
	return market.NormalizeForSource(symbol, e.marketSource)
}

// validationEnv carries the venue-specific context validateDecision needs.
// A nil env means "legacy behaviour": no asset-class caps, no session checks.
type validationEnv struct {
	// Bitget enables contract metadata lookups (asset class, maxLever, status).
	Bitget bool
	// EquityLeverage / CommodityLeverage are the effective caps for those classes.
	EquityLeverage    int
	CommodityLeverage int
}

// validationEnv builds the env for the engine's current source (nil for default).
func (e *StrategyEngine) validationEnv() *validationEnv {
	if e.marketSource != market.SourceBitget {
		return nil
	}
	rc := e.config.RiskControl
	return &validationEnv{
		Bitget:            true,
		EquityLeverage:    rc.EffectiveEquityMaxLeverage(),
		CommodityLeverage: rc.EffectiveCommodityMaxLeverage(),
	}
}

// symbolInfo is the resolved TradFi view of a symbol.
type symbolInfo struct {
	Contract   *bitget.Contract
	AssetClass string
	Open       bool // session open AND contract tradable
	Regular    bool
	Note       string
	MaxLever   int // 0 = unknown/unlimited by venue
}

// resolveSymbol fetches Bitget metadata and evaluates the trading session.
func resolveSymbol(symbol string, now time.Time) (*symbolInfo, error) {
	c, err := bitgetContractLookup(symbol)
	if err != nil {
		return nil, err
	}
	cls := bitget.AssetClass(*c)
	open, regular, note := market.MarketSession(cls, now)
	if !c.Tradable() {
		open = false
		note = fmt.Sprintf("contract status is %q (not normal): trading halted", c.SymbolStatus)
	}
	return &symbolInfo{
		Contract:   c,
		AssetClass: cls,
		Open:       open,
		Regular:    regular,
		Note:       note,
		MaxLever:   int(c.MaxLever),
	}, nil
}

// leverageCapFor returns the strategy leverage cap for the asset class.
func (env *validationEnv) leverageCapFor(cls string, symbol string, btcEth, altcoin int) int {
	switch cls {
	case bitget.ClassEquity:
		return env.EquityLeverage
	case bitget.ClassCommodity, bitget.ClassFX:
		return env.CommodityLeverage
	default:
		if symbol == "BTCUSDT" || symbol == "ETHUSDT" {
			return btcEth
		}
		return altcoin
	}
}

// tradFiPromptLine returns a one-line per-symbol summary for the user prompt
// (asset class, session state, effective leverage cap). Empty for non-Bitget sources.
func (e *StrategyEngine) tradFiPromptLine(symbol string) string {
	if e.marketSource != market.SourceBitget {
		return ""
	}
	info, err := resolveSymbol(symbol, nowFunc())
	if err != nil {
		return fmt.Sprintf("Instrument: contract metadata unavailable (%v)\n", err)
	}
	rc := e.config.RiskControl
	env := e.validationEnv()
	levCap := env.leverageCapFor(info.AssetClass, strings.ToUpper(symbol), rc.BTCETHMaxLeverage, rc.AltcoinMaxLeverage)
	if info.MaxLever > 0 && (levCap <= 0 || info.MaxLever < levCap) {
		levCap = info.MaxLever
	}
	status := "OPEN"
	if !info.Open {
		status = "CLOSED (new open_long/open_short will be rejected; closing is allowed)"
	}
	return fmt.Sprintf("Instrument: class=%s | session=%s | %s | max leverage allowed=%dx\n",
		info.AssetClass, status, info.Note, levCap)
}

// tradFiSystemNote is appended to the system prompt for Bitget sources.
func tradFiSystemNote(rc store.RiskControlConfig) string {
	return fmt.Sprintf("\n## Note on Instruments (Bitget)\n"+
		"Symbols may be crypto perps, or US stock/ETF, commodity and FX perps quoted in USDT (e.g. NVDAUSDT, XAUUSDT). "+
		"TradFi perps trade 5x24 (Mon 00:00 - Sat 00:00 ET) and cannot be opened when closed. "+
		"Leverage caps by class: BTC/ETH %dx | altcoins %dx | equity/ETF %dx | commodity/FX %dx (also limited by the contract's max leverage). "+
		"Use the per-symbol Instrument line in the user prompt; the leverage and prices in your examples must be adapted to the symbol you trade.\n\n",
		rc.BTCETHMaxLeverage, rc.AltcoinMaxLeverage, rc.EffectiveEquityMaxLeverage(), rc.EffectiveCommodityMaxLeverage())
}

// isNonCrypto reports whether the symbol is a TradFi (non-crypto) Bitget contract.
// On lookup failure it falls back to the static xyz asset list.
func (e *StrategyEngine) isNonCrypto(symbol string) bool {
	c, err := bitgetContractLookup(symbol)
	if err != nil {
		return market.IsXyzDexAsset(symbol)
	}
	return bitget.AssetClass(*c) != bitget.ClassCrypto
}

// dropUntradableOpens removes open_long/open_short decisions for instruments that cannot be
// opened right now: market closed or halted (Bitget TradFi perps outside their 5x24 session)
// or contract metadata that cannot be resolved (unknown/delisted symbol, network error).
// validateDecisionEnv rejects both cases, and one rejected decision fails the whole batch, so
// dropping them individually keeps the rest of the cycle's decisions (e.g. closes on other
// symbols) executable.
func dropUntradableOpens(decisions []Decision, env *validationEnv) []Decision {
	if env == nil || !env.Bitget {
		return decisions
	}
	kept := make([]Decision, 0, len(decisions)) // fresh slice: never rewrite the caller's backing array
	for _, d := range decisions {
		if d.Action == "open_long" || d.Action == "open_short" {
			info, err := resolveSymbol(d.Symbol, nowFunc())
			if err != nil {
				logger.Warnf("⚠️  Dropping %s %s: contract metadata unavailable (%v)", d.Action, d.Symbol, err)
				continue
			}
			if !info.Open {
				logger.Infof("⏸️  Dropping %s %s: market closed (%s, %s)", d.Action, d.Symbol, info.AssetClass, info.Note)
				continue
			}
		}
		kept = append(kept, d)
	}
	return kept
}
