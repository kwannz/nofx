import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { ExchangeConfigModal } from './ExchangeConfigModal'
import { translations } from '../../i18n/translations'
import type { Exchange } from '../../types'

vi.mock('../../lib/api', () => ({
  api: { getServerIP: vi.fn().mockResolvedValue({ public_ip: '1.2.3.4', message: '' }) },
}))

const baseExchange = {
  id: 'ex-1',
  exchange_type: 'bitget',
  account_name: 'main',
  name: 'Bitget Futures',
  type: 'cex' as const,
  enabled: true,
  apiKey: 'key',
  secretKey: 'secret',
}

function renderModal(
  exchange: Partial<Exchange>,
  language: 'en' | 'zh' = 'en'
) {
  const onSave = vi.fn().mockResolvedValue(undefined)
  const all = [{ ...baseExchange, ...exchange }] as Exchange[]
  render(
    <ExchangeConfigModal
      allExchanges={all}
      editingExchangeId="ex-1"
      onSave={onSave}
      onDelete={vi.fn()}
      onClose={vi.fn()}
      language={language}
    />
  )
  return onSave
}

describe('ExchangeConfigModal Bitget position mode', () => {
  it('has zh and en labels for the selector', () => {
    for (const lang of ['en', 'zh'] as const) {
      const tr = translations[lang] as Record<string, unknown>
      for (const key of [
        'bitgetPositionMode',
        'bitgetPositionModeHedge',
        'bitgetPositionModeOneWay',
        'bitgetPositionModeDescription',
      ]) {
        expect(tr[key], `${lang}.${key}`).toBeTruthy()
      }
    }
  })

  it('defaults to hedge for rows without the field (older data) and sends the choice on save', async () => {
    const onSave = renderModal({ exchange_type: 'bitget' })
    const select = screen.getByLabelText('Position mode') as HTMLSelectElement
    expect(select.value).toBe('hedge')
    expect(screen.getByTestId('bitget-position-mode')).toBeInTheDocument()

    // the passphrase is never pre-filled: enter it so the form is valid
    fireEvent.change(screen.getByPlaceholderText('Enter Passphrase'), {
      target: { value: 'pp' },
    })
    fireEvent.change(select, { target: { value: 'one_way' } })
    expect(select.value).toBe('one_way')
    fireEvent.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    const args = onSave.mock.calls[0]
    expect(args[1]).toBe('bitget')
    expect(args[5]).toBe('pp') // passphrase
    expect(args[args.length - 1]).toBe('one_way')
  })

  it('loads one_way from the stored exchange', () => {
    renderModal({ bitgetPositionMode: 'one_way' })
    expect((screen.getByLabelText('Position mode') as HTMLSelectElement).value).toBe('one_way')
  })

  it('shows the selector for Bitget Paper and submits the chosen mode without keys', async () => {
    const onSave = renderModal({
      exchange_type: 'bitget_paper',
      name: 'Bitget Paper',
      bitgetPositionMode: 'one_way',
    })
    const select = screen.getByLabelText('Position mode') as HTMLSelectElement
    expect(select.value).toBe('one_way')
    fireEvent.change(select, { target: { value: 'hedge' } })
    fireEvent.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    const args = onSave.mock.calls[0]
    expect(args[1]).toBe('bitget_paper')
    expect(args[args.length - 1]).toBe('hedge')
  })

  it('renders Chinese labels', () => {
    renderModal({ exchange_type: 'bitget_paper', name: 'Bitget Paper' }, 'zh')
    expect(screen.getByLabelText('持仓模式')).toBeInTheDocument()
  })

  it('is not shown for other exchanges', async () => {
    renderModal({ exchange_type: 'binance', name: 'Binance Futures' })
    expect(screen.queryByTestId('bitget-position-mode')).toBeNull()
    // let the Binance server-IP lookup settle inside act()
    expect(await screen.findByText('1.2.3.4')).toBeInTheDocument()
  })
})
