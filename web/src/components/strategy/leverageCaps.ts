import type { RiskControlConfig } from '../../types'

// UI-side mirror of store.RiskControlConfig.EffectiveEquityMaxLeverage /
// EffectiveCommodityMaxLeverage: configs saved before the TradFi caps existed leave the field
// unset (undefined or 0) and the backend then enforces the altcoin cap, so that is the value the
// editor has to display. New strategies get their 5/10 defaults from the backend default config.

// Fallback shown for the altcoin cap itself when it is missing (same as the editor's slider).
const DEFAULT_ALTCOIN_LEVERAGE = 5

type LeverageCaps = Pick<
  RiskControlConfig,
  'altcoin_max_leverage' | 'equity_max_leverage' | 'commodity_max_leverage'
>

function altcoinCap(config: LeverageCaps): number {
  const alt = config.altcoin_max_leverage
  return alt && alt > 0 ? alt : DEFAULT_ALTCOIN_LEVERAGE
}

/** True when the backend ignores `value` and falls back to the altcoin cap. */
export function followsAltcoinCap(value: number | undefined | null): boolean {
  return !value || value <= 0
}

export function effectiveEquityMaxLeverage(config: LeverageCaps): number {
  return followsAltcoinCap(config.equity_max_leverage)
    ? altcoinCap(config)
    : (config.equity_max_leverage as number)
}

export function effectiveCommodityMaxLeverage(config: LeverageCaps): number {
  return followsAltcoinCap(config.commodity_max_leverage)
    ? altcoinCap(config)
    : (config.commodity_max_leverage as number)
}
