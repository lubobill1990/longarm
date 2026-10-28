import type { ReactNode } from 'react'
import { IconAlert, IconCheck, IconInfo } from '../components/icons'

/**
 * The tray's small building blocks (tray.css): banners, empty states,
 * skeletons, sections. Other tabs and banners (updates, ...) use them too.
 */

export type Tone = 'info' | 'warn' | 'error' | 'ok'

const TONE_ICON: Record<Tone, (p: { size?: number }) => ReactNode> = {
  info: IconInfo,
  warn: IconAlert,
  error: IconAlert,
  ok: IconCheck,
}

/** A calm, tinted message across the content: an icon, the text, an action on the right. */
export function Banner({ tone = 'info', children, action, testId }: { tone?: Tone; children: ReactNode; action?: ReactNode; testId?: string }) {
  const Icon = TONE_ICON[tone]
  return (
    <div className={`banner banner-${tone}`} data-testid={testId} role={tone === 'error' ? 'alert' : undefined}>
      <span className="banner-icon">
        <Icon size={16} />
      </span>
      <div className="banner-text">{children}</div>
      {action && <div className="banner-action">{action}</div>}
    </div>
  )
}

/** Nothing to show yet: an icon tile, a title, a line of help, an action. */
export function EmptyState({
  icon,
  title,
  children,
  action,
  testId,
  compact,
}: {
  icon?: ReactNode
  title: ReactNode
  children?: ReactNode
  action?: ReactNode
  testId?: string
  compact?: boolean
}) {
  return (
    <div className={`empty ${compact ? 'compact' : ''}`} data-testid={testId}>
      {icon && <div className="empty-icon">{icon}</div>}
      <div className="empty-title">{title}</div>
      {children && <div className="empty-text">{children}</div>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  )
}

/** Shimmering placeholder lines while data loads; label is read out (and found by tests). */
export function Skeleton({ lines = 3, label, card }: { lines?: number; label: string; card?: boolean }) {
  const widths = ['w80', 'w60', 'w40', 'w70']
  return (
    <div className={`skeleton ${card ? 'skeleton-card' : ''}`} aria-busy="true">
      <span className="sr-only">{label}</span>
      {Array.from({ length: lines }, (_, i) => (
        <span key={i} className={`skeleton-line ${widths[i % widths.length]}`} aria-hidden="true" />
      ))}
    </div>
  )
}

/** A titled group of settings or facts. */
export function Section({ title, aside, children, testId, className }: { title: ReactNode; aside?: ReactNode; children: ReactNode; testId?: string; className?: string }) {
  return (
    <section className={`group ${className ?? ''}`} data-testid={testId}>
      <div className="group-head">
        <h3>{title}</h3>
        {aside && <div className="group-aside">{aside}</div>}
      </div>
      {children}
    </section>
  )
}
