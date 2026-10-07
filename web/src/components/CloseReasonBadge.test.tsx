import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { CloseReasonBadge } from './CloseReasonBadge'
import { translations } from '../i18n/translations'

const REASONS = [
  'ai',
  'manual',
  'stop_loss',
  'take_profit',
  'liquidation',
  'drawdown',
  'sync',
  'paper_state_lost',
] as const

describe('CloseReasonBadge', () => {
  it('has a zh and en label for every close reason', () => {
    for (const lang of ['en', 'zh'] as const) {
      const labels = translations[lang].positionHistory.closeReasons as Record<string, string>
      for (const reason of REASONS) {
        expect(labels[reason], `${lang}.${reason}`).toBeTruthy()
      }
    }
  })

  it('shows the English label', () => {
    render(<CloseReasonBadge reason="stop_loss" language="en" />)
    expect(screen.getByTestId('close-reason-badge').textContent).toBe('Stop Loss')
  })

  it('shows the Chinese label, sync as 同步', () => {
    const { rerender } = render(<CloseReasonBadge reason="sync" language="zh" />)
    expect(screen.getByTestId('close-reason-badge').textContent).toBe('同步')
    rerender(<CloseReasonBadge reason="take_profit" language="zh" />)
    expect(screen.getByTestId('close-reason-badge').textContent).toBe('止盈')
    rerender(<CloseReasonBadge reason="liquidation" language="zh" />)
    expect(screen.getByTestId('close-reason-badge').textContent).toBe('强平')
  })

  it('shows an unknown reason verbatim', () => {
    render(<CloseReasonBadge reason="something_new" language="en" />)
    expect(screen.getByTestId('close-reason-badge').textContent).toBe('something_new')
  })

  it('renders a dash for an empty reason', () => {
    const { container } = render(<CloseReasonBadge reason="" language="en" />)
    expect(screen.queryByTestId('close-reason-badge')).toBeNull()
    expect(container.textContent).toBe('-')
  })
})
