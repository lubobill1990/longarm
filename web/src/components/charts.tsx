import { useState } from 'react'
import { cssColor, levelColor } from '../lib/format'

/** Labelled bar: label, track coloured by level, value text. */
export function Bar({ label, pct, value }: { label: string; pct: number; value: string }) {
  const p = Math.min(Math.max(pct || 0, 0), 100)
  return (
    <div className="bar" role="meter" aria-label={label} aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100}>
      <span className="label">{label}</span>
      <span className="track">
        <span className="fill" style={{ display: 'block', width: `${p}%`, background: cssColor(levelColor(p)) }} />
      </span>
      <span className="value">{value}</span>
    </div>
  )
}

/** A small inline bar for tables. */
export function MiniBar({ pct }: { pct: number }) {
  const p = Math.min(Math.max(pct || 0, 0), 100)
  return (
    <span className="minibar" title={`${Math.round(p)}%`}>
      <span style={{ width: `${p}%`, background: cssColor(levelColor(p)) }} />
    </span>
  )
}

export interface Series {
  values: (number | null)[] // null = gap (failed sample)
  color: string
  label?: string
  /** the value as text (the hover tooltip): default a percent */
  fmt?: (v: number) => string
}

/** A legend entry that shows or hides a series (off by default: disk, network). */
export interface SeriesToggle {
  label: string
  color: string
  on: boolean
  onToggle: () => void
  title?: string
}

const pctFmt = (v: number) => `${v.toFixed(v < 10 ? 1 : 0)}%`

/** "now" / "20s ago" / "1m 40s ago": how far back a slot is. */
export function slotAgo(slotsFromRight: number, stepSeconds: number): string {
  const s = Math.round(slotsFromRight * stepSeconds)
  if (s <= 0) return 'now'
  if (s < 60) return `${s}s ago`
  return `${Math.floor(s / 60)}m ${s % 60 ? `${s % 60}s ` : ''}ago`
}

/**
 * Sparkline of series over a fixed number of slots (newest right), like
 * a 5-minute chart. max: the y range top (percent charts: 100;
 * "auto" scales to the data). Gridlines at 25/50/75 %. Hovering shows
 * every series' value at that moment (Series.fmt) and how long ago it was
 * (stepSeconds per slot); toggles add legend entries that show or hide
 * series.
 */
export function Sparkline({
  series,
  slots = 150,
  max = 100,
  height = 46,
  legend,
  right,
  marks,
  toggles,
  stepSeconds = 2,
  fmtMax,
}: {
  series: Series[]
  slots?: number
  max?: number | 'auto'
  height?: number
  legend?: boolean
  right?: string
  marks?: { at: number; color: string }[] // vertical marks (failed probes), slot index from the right
  toggles?: SeriesToggle[]
  stepSeconds?: number
  fmtMax?: (v: number) => string
}) {
  const [hover, setHover] = useState<number | null>(null)
  const W = 400
  const H = height
  let top = typeof max === 'number' ? max : 0
  if (max === 'auto') {
    for (const s of series) for (const v of s.values) if (v !== null && v > top) top = v
    top = niceMax(top)
  }
  const step = W / Math.max(1, slots - 1)
  const y = (v: number) => H - 2 - ((H - 4) * Math.min(Math.max(v, 0), top)) / (top || 1)
  const paths = series.map((s) => {
    const off = slots - s.values.length
    let d = ''
    let pen = false
    s.values.forEach((v, i) => {
      if (v === null || v === undefined) {
        pen = false
        return
      }
      const x = (off + i) * step
      d += `${pen ? 'L' : 'M'}${x.toFixed(1)},${y(v).toFixed(1)}`
      pen = true
    })
    return { d, color: s.color }
  })
  const longest = Math.max(0, ...series.map((s) => s.values.length))
  const firstSlot = slots - longest // no data left of it
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect()
    if (!r.width || !longest) return
    const slot = Math.round(((e.clientX - r.left) / r.width) * (slots - 1))
    setHover(Math.min(slots - 1, Math.max(firstSlot, slot)))
  }
  const at = (s: Series, slot: number) => {
    const v = s.values[slot - (slots - s.values.length)]
    return v === null || v === undefined ? '—' : (s.fmt ?? pctFmt)(v)
  }
  const tipLeft = hover === null ? 0 : (hover / Math.max(1, slots - 1)) * 100
  return (
    <div className="spark-wrap">
      <svg
        className="spark"
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        height={H}
        role="img"
        aria-label="sparkline"
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        {[25, 50, 75].map((g) => (
          <line key={g} x1={1} x2={W - 1} y1={H - (H * g) / 100} y2={H - (H * g) / 100} style={{ stroke: 'var(--chart-grid, #21262d)' }} strokeDasharray="2 4" />
        ))}
        {(marks ?? []).map((m, i) => (
          <line key={`m${i}`} x1={(slots - 1 - m.at) * step} x2={(slots - 1 - m.at) * step} y1={0} y2={H} style={{ stroke: cssColor(m.color) }} strokeOpacity={0.5} />
        ))}
        {paths.map((p, i) => (
          <path key={i} d={p.d} fill="none" style={{ stroke: cssColor(p.color) }} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
        ))}
        {hover !== null && (
          <line x1={hover * step} x2={hover * step} y1={0} y2={H} style={{ stroke: 'var(--text-3, #8b949e)' }} strokeWidth={1} vectorEffect="non-scaling-stroke" />
        )}
      </svg>
      {hover !== null && (
        <div className={`spark-tip ${tipLeft > 55 ? 'left' : ''}`} style={tipLeft > 55 ? { right: `${100 - tipLeft}%` } : { left: `${tipLeft}%` }} data-testid="spark-tip">
          <div className="spark-tip-when">{slotAgo(slots - 1 - hover, stepSeconds)}</div>
          {series.map((s, i) => (
            <div key={i} className="spark-tip-row">
              <span style={{ color: cssColor(s.color) }}>● {s.label ?? ''}</span>
              <span className="v">{at(s, hover)}</span>
            </div>
          ))}
        </div>
      )}
      {(legend || right || toggles?.length) && (
        <div className="spark-legend">
          {legend &&
            series.map((s, i) =>
              // a series a toggle shows has the toggle as its key
              s.label && !toggles?.some((t) => t.on && t.label === s.label) ? (
                <span key={i} className="spark-key" style={{ color: cssColor(s.color) }}>
                  ● {s.label}
                </span>
              ) : null,
            )}
          {(toggles ?? []).map((t) => (
            <button
              key={t.label}
              type="button"
              className={`spark-toggle ${t.on ? 'on' : ''}`}
              style={{ ['--c' as string]: cssColor(t.color) }}
              aria-pressed={t.on}
              title={t.title ?? `${t.on ? 'hide' : 'show'} ${t.label}`}
              onClick={t.onToggle}
            >
              <i className="spark-dot" aria-hidden="true" /> {t.label}
            </button>
          ))}
          <span className="spacer" />
          {max === 'auto' && <span className="faint">max {fmtMax ? fmtMax(top) : Math.round(top)}</span>}
          {right && <span className="faint">{right}</span>}
        </div>
      )}
    </div>
  )
}

/** A round upper bound for auto-scaled charts (RTT ms). */
export function niceMax(v: number): number {
  if (v <= 0) return 10
  // 1-2-5 steps at every power of ten (RTT ms; network bytes/s)
  for (let p = 10; ; p *= 10) {
    for (const m of [1, 2, 5]) if (v <= m * p) return m * p
  }
}
