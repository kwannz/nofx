import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { RiskControlEditor } from './RiskControlEditor'
import {
  effectiveCommodityMaxLeverage,
  effectiveEquityMaxLeverage,
} from './leverageCaps'
import type { RiskControlConfig } from '../../types'

const base: RiskControlConfig = {
  max_positions: 3,
  btc_eth_max_leverage: 5,
  altcoin_max_leverage: 7,
}

const noop = () => {}

describe('effective TradFi leverage caps (mirror of the backend fallback)', () => {
  it('falls back to the altcoin cap when the field is undefined or 0', () => {
    expect(effectiveEquityMaxLeverage(base)).toBe(7)
    expect(effectiveCommodityMaxLeverage(base)).toBe(7)
    expect(
      effectiveEquityMaxLeverage({ ...base, equity_max_leverage: 0 })
    ).toBe(7)
    expect(
      effectiveCommodityMaxLeverage({ ...base, commodity_max_leverage: 0 })
    ).toBe(7)
  })

  it('uses the explicit value when set', () => {
    expect(
      effectiveEquityMaxLeverage({ ...base, equity_max_leverage: 5 })
    ).toBe(5)
    expect(
      effectiveCommodityMaxLeverage({ ...base, commodity_max_leverage: 10 })
    ).toBe(10)
  })

  it('shows 5 when the altcoin cap itself is missing (same as the altcoin slider)', () => {
    const noAlt = { ...base } as Partial<RiskControlConfig>
    delete noAlt.altcoin_max_leverage
    expect(effectiveEquityMaxLeverage(noAlt as RiskControlConfig)).toBe(5)
  })
})

describe('RiskControlEditor TradFi leverage display', () => {
  const sliderValues = (container: HTMLElement) =>
    Array.from(
      container.querySelectorAll<HTMLInputElement>('input[type="range"]')
    ).map((el) => el.value)

  it('shows the effective (altcoin) cap, not 5/10, for configs without the fields', () => {
    const { container } = render(
      <RiskControlEditor config={base} onChange={noop} language="en" />
    )
    // order: btc/eth, altcoin, equity, commodity, ...
    const values = sliderValues(container)
    expect(values[2]).toBe('7')
    expect(values[3]).toBe('7')
    expect(screen.getByTestId('equity-follows-altcoin')).toHaveTextContent(
      'follows the altcoin cap: 7x'
    )
    expect(screen.getByTestId('commodity-follows-altcoin')).toHaveTextContent(
      'follows the altcoin cap: 7x'
    )
  })

  it('has a Chinese hint', () => {
    render(<RiskControlEditor config={base} onChange={noop} language="zh" />)
    expect(screen.getByTestId('equity-follows-altcoin')).toHaveTextContent(
      '跟随山寨币杠杆上限'
    )
    expect(screen.getByTestId('commodity-follows-altcoin')).toHaveTextContent(
      '跟随山寨币杠杆上限'
    )
  })

  it('treats 0 like unset', () => {
    render(
      <RiskControlEditor
        config={{ ...base, equity_max_leverage: 0, commodity_max_leverage: 0 }}
        onChange={noop}
        language="en"
      />
    )
    expect(screen.getByTestId('equity-follows-altcoin')).toBeInTheDocument()
    expect(screen.getByTestId('commodity-follows-altcoin')).toBeInTheDocument()
  })

  it('shows explicit values (backend default config 5/10) without the hint', () => {
    const { container } = render(
      <RiskControlEditor
        config={{ ...base, equity_max_leverage: 5, commodity_max_leverage: 10 }}
        onChange={noop}
        language="en"
      />
    )
    const values = sliderValues(container)
    expect(values[2]).toBe('5')
    expect(values[3]).toBe('10')
    expect(screen.queryByTestId('equity-follows-altcoin')).toBeNull()
    expect(screen.queryByTestId('commodity-follows-altcoin')).toBeNull()
  })
})
