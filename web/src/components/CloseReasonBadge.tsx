import { t } from '../i18n/translations'
import type { Language } from '../i18n/translations'

// close_reason values written by the backend (store.CloseReason* in Go).
const CLOSE_REASON_COLORS: Record<string, string> = {
  ai: '#F0B90B',
  manual: '#9B7BFF',
  stop_loss: '#F6465D',
  take_profit: '#0ECB81',
  liquidation: '#FF6B35',
  drawdown: '#FFA500',
  sync: '#848E9C',
  paper_state_lost: '#5E6673',
}

const NEUTRAL_COLOR = '#848E9C'

interface CloseReasonBadgeProps {
  reason?: string | null
  language: Language
}

// Small badge showing why a position was closed. Unknown reasons are shown verbatim, an empty
// reason (positions closed before close_reason existed) renders a dash.
export function CloseReasonBadge({ reason, language }: CloseReasonBadgeProps) {
  if (!reason) {
    return <span style={{ color: NEUTRAL_COLOR }}>-</span>
  }
  const known = Object.prototype.hasOwnProperty.call(CLOSE_REASON_COLORS, reason)
  const color = known ? CLOSE_REASON_COLORS[reason] : NEUTRAL_COLOR
  const label = known ? t(`positionHistory.closeReasons.${reason}`, language) : reason
  return (
    <span
      data-testid="close-reason-badge"
      data-reason={reason}
      className="px-2 py-0.5 rounded text-xs font-semibold whitespace-nowrap"
      style={{
        background: `${color}22`,
        color,
        border: `1px solid ${color}44`,
      }}
    >
      {label}
    </span>
  )
}
