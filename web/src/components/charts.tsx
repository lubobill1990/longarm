import { levelColor } from '../lib/format'

/** Labelled bar: label, track coloured by level, value text. */
export function Bar({ label, pct, value }: { label: string; pct: number; value: string }) {
  const p = Math.min(Math.max(pct || 0, 0), 100)
  return (
    <div className="bar" role="meter" aria-label={label} aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100}>
      <span className="label">{label}</span>
      <span className="track">
        <span className="fill" style={{ display: 'block', width: `${p}%`, background: levelColor(p) }} />
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
      <span style={{ width: `${p}%`, background: levelColor(p) }} />
    </span>
  )
}

export interface Series {
  values: (number | null)[] // null = gap (failed sample)
  color: string
  label?: string
}

/**
 * Sparkline of series over a fixed number of slots (newest right), like
 * a 5-minute chart. max: the y range top (percent charts: 100;
 * "auto" scales to the data). Gridlines at 25/50/75 %.
 */
export function Sparkline({
  series,
  slots = 150,
  max = 100,
  height = 46,
  legend,
  right,
  marks,
}: {
  series: Series[]
  slots?: number
  max?: number | 'auto'
  height?: number
  legend?: boolean
  right?: string
  marks?: { at: number; color: string }[] // vertical marks (failed probes), slot index from the right
}) {
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
  return (
    <div>
      <svg className="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" height={H} role="img" aria-label="sparkline">
        {[25, 50, 75].map((g) => (
          <line key={g} x1={1} x2={W - 1} y1={H - (H * g) / 100} y2={H - (H * g) / 100} stroke="#21262d" strokeDasharray="2 4" />
        ))}
        {(marks ?? []).map((m, i) => (
          <line key={`m${i}`} x1={(slots - 1 - m.at) * step} x2={(slots - 1 - m.at) * step} y1={0} y2={H} stroke={m.color} strokeOpacity={0.5} />
        ))}
        {paths.map((p, i) => (
          <path key={i} d={p.d} fill="none" stroke={p.color} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
        ))}
      </svg>
      {(legend || right) && (
        <div className="spark-legend">
          {legend &&
            series.map((s, i) =>
              s.label ? (
                <span key={i} style={{ color: s.color }}>
                  ● {s.label}
                </span>
              ) : null,
            )}
          <span className="spacer" />
          {max === 'auto' && <span className="faint">max {Math.round(top)}</span>}
          {right && <span className="faint">{right}</span>}
        </div>
      )}
    </div>
  )
}

/** A round upper bound for auto-scaled charts (RTT ms). */
export function niceMax(v: number): number {
  if (v <= 0) return 10
  const steps = [10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000]
  for (const s of steps) if (v <= s) return s
  return Math.ceil(v / 10000) * 10000
}
