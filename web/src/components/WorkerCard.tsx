import type { LegStatus, MetricsSnapshot, PoolWorker, ProcGroup } from '../api/types'
import {
  AMBER,
  CPU_C,
  fmtAgo,
  fmtBytes,
  fmtRTT,
  fmtUptime,
  gb,
  GREEN,
  isRelayed,
  MEM_C,
  memPct,
  num,
  RED,
  RTT_C,
  since,
} from '../lib/format'
import { Bar, Sparkline } from './charts'

const AGENTS: [string, string, string][] = [
  ['claude', 'Claude Code', 'var(--claude)'],
  ['copilot', 'Copilot CLI', 'var(--copilot)'],
]

export interface CardActions {
  attach?: (w: string) => void
  ssh?: (w: string) => void
  panel?: (w: string) => void
  drain?: (w: string, on: boolean) => void
}

/**
 * One worker in a dark card layout: status line, then this
 * client's link (active leg, path, NAT, RTT + 5-min RTT sparkline), CPU and
 * memory bars with the 5-min sparkline, Claude / Copilot sessions, info
 * line and the top processes by CPU and memory.
 */
export function WorkerCard({ w, actions, now = Date.now() }: { w: PoolWorker; actions?: CardActions; now?: number }) {
  const state = w.state ?? 'connecting'
  const dot = state === 'online' ? GREEN : state === 'offline' ? RED : AMBER
  const m = w.state === 'online' || w.source === 'hub' ? w.metrics : undefined
  const sinceS = since(w.since, now)
  let sub: string
  if (state === 'online') sub = `online ${fmtAgo(sinceS)}`
  else if (state === 'offline') sub = `offline ${fmtAgo(sinceS)}, reconnecting`
  else sub = 'connecting…'
  const draining = w.node?.draining
  return (
    <div className="wcard" data-testid={`worker-${w.name}`} data-state={state}>
      <div className="head">
        <span className="dot" style={{ background: dot }} />
        <span className="name">{w.name}</span>
        <span className="sub">{sub}</span>
        {draining && (
          <span className="pill warn" title={w.node?.drain_reason}>
            draining
          </span>
        )}
        {w.source && w.source !== 'none' && (
          <span className="pill" title="where these metrics come from">
            {w.source}
          </span>
        )}
        <span className="actions">
          {actions?.attach && <button onClick={() => actions.attach!(w.name)}>attach</button>}
          {actions?.ssh && <button onClick={() => actions.ssh!(w.name)}>ssh ↗</button>}
          {actions?.panel && <button onClick={() => actions.panel!(w.name)}>hub ↗</button>}
          {actions?.drain && (
            <button title="longarm admin drain / undrain (admin certificate)" onClick={() => actions.drain!(w.name, !w.node?.admin_drain)}>
              {w.node?.admin_drain ? 'resume' : 'drain'}
            </button>
          )}
        </span>
      </div>
      <LinkRows w={w} now={now} />
      {!m ? (
        <div className={`msg ${state === 'offline' ? 'red' : 'muted'}`}>
          {(w.error || (state === 'connecting' ? 'waiting for the first sample…' : 'unreachable')).slice(0, 120)}
        </div>
      ) : (
        <Metrics m={m} w={w} />
      )}
    </div>
  )
}

function LinkRows({ w, now }: { w: PoolWorker; now: number }) {
  const legs = w.link?.legs ?? []
  if (!legs.length) return null
  const active = legs.find((l) => l.active) ?? legs[0]
  const hist = (active.history ?? []).map((s) => (s.rtt_ms !== undefined && s.rtt_ms >= 0 ? s.rtt_ms : null))
  const fails = (active.history ?? [])
    .map((s, i, a) => ({ i: a.length - 1 - i, bad: (s.rtt_ms ?? 0) < 0 }))
    .filter((x) => x.bad)
    .map((x) => ({ at: x.i, color: RED }))
  const relayed = active.up && isRelayed(active.kind)
  const relayFor = relayed && active.relay_since ? since(active.relay_since, now) : 0
  return (
    <div data-testid="link">
      <div className="linkrow">
        <span className="muted small" style={{ width: 36, fontWeight: 700 }}>
          LINK
        </span>
        <span className="path" style={{ color: !active.up ? RED : relayed ? AMBER : GREEN }} data-testid="link-path">
          {active.label || active.transport}
        </span>
        {active.nat && <span className="pill">nat {active.nat}</span>}
        {relayed && relayFor > 0 && <span className="small amber">relayed {fmtAgo(relayFor)}</span>}
        <span className="spacer" />
        <span className="mono" data-testid="link-rtt">
          {active.up ? fmtRTT(active.rtt_ms) : 'down'}
        </span>
      </div>
      <Legs legs={legs} />
      <Sparkline series={[{ values: hist, color: RTT_C, label: 'RTT' }]} max="auto" height={30} legend right="5 min" marks={fails} />
    </div>
  )
}

function Legs({ legs }: { legs: LegStatus[] }) {
  if (legs.length < 2) return null
  return (
    <div className="legs">
      {legs.map((l, i) => (
        <span key={i} className={`leg ${l.active ? 'active' : ''} ${l.up ? '' : 'down'}`} title={l.error || l.detail || l.endpoint}>
          {l.label || l.transport} {l.up ? fmtRTT(l.rtt_ms) : ''}
        </span>
      ))}
    </div>
  )
}

function Metrics({ m, w }: { m: MetricsSnapshot; w: PoolWorker }) {
  const cpu = m.cpu_percent ?? 0
  const mp = memPct(m)
  const hist = w.history ?? []
  const diskFree = gb(m.disk_free_bytes)
  return (
    <div style={{ marginTop: 6 }}>
      <Bar label="CPU" pct={cpu} value={`${cpu.toFixed(0)}%`} />
      <Bar label="MEM" pct={mp} value={`${gb(m.mem_used_bytes).toFixed(1)} / ${gb(m.mem_total_bytes).toFixed(0)} GB`} />
      <div style={{ marginTop: 4 }}>
        <Sparkline
          series={[
            { values: hist.map((p) => p.mem ?? 0), color: MEM_C, label: 'MEM' },
            { values: hist.map((p) => p.cpu ?? 0), color: CPU_C, label: 'CPU' },
          ]}
          legend
          right="5 min"
        />
      </div>
      <div className="agents">
        {AGENTS.map(([key, label, color]) => {
          const a = m.agents?.[key]
          const n = a?.sessions ?? 0
          return (
            <div key={key} className="agent" style={{ ['--c' as string]: n ? color : undefined }} data-testid={`agent-${key}`}>
              <span style={{ color: n ? 'var(--fg)' : 'var(--muted)' }}>{label}</span>
              <span className="n" style={{ color: n ? color : 'var(--faint)' }}>
                {n}
              </span>
              <span className="m">{n ? `${gb(a?.mem_bytes).toFixed(1)}G` : '—'}</span>
            </div>
          )
        })}
      </div>
      <div className="info">
        {m.threads ?? '?'} threads · {m.nproc ?? '?'} procs · longarm jobs {m.jobs_running ?? 0}
        {w.my_jobs ? ` (mine ${w.my_jobs})` : ''} · up {fmtUptime(num(m.uptime_seconds))}
        {num(m.disk_total_bytes) > 0 && ` · disk ${diskFree.toFixed(0)} GB free`}
      </div>
      <TopProcs m={m} />
    </div>
  )
}

function TopProcs({ m }: { m: MetricsSnapshot }) {
  const cols: [ProcGroup[], string, (p: ProcGroup) => string][] = [
    [(m.top_cpu ?? []).slice(0, 3), 'TOP CPU', (p) => `${(p.cpu_percent ?? 0).toFixed(0)}%`],
    [(m.top_mem ?? []).slice(0, 3), 'TOP MEMORY', (p) => fmtBytes(p.mem_bytes)],
  ]
  return (
    <div className="tops">
      {cols.map(([items, title, val]) => (
        <div key={title}>
          <div className="title">{title}</div>
          {items.map((p, i) => (
            <div className="p" key={i}>
              <span>
                {p.name.slice(0, 16)}
                {(p.count ?? 1) > 1 ? ` ×${p.count}` : ''}
              </span>
              <span className="v">{val(p)}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  )
}
