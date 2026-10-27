import { hubApi } from '../api/hub'
import type { PortLink } from '../api/types'
import { ErrorBox, useAction, usePoll } from '../components/common'
import { fmtAgo, fmtBytes, num, until } from '../lib/format'
import { Link } from '../lib/router'

const STATE_CLS: Record<string, string> = { listening: 'ok', pending: 'warn', error: 'bad' }

/** "in 3h" / "expired 5m ago" / "—". */
function fmtExpires(t?: string): string {
  if (!t) return '—'
  const s = until(t)
  return s > 0 ? `in ${fmtAgo(s)}` : `expired ${fmtAgo(-s)} ago`
}

/**
 * Port links (longarm link): 127.0.0.1:<listen port> on one worker forwards
 * to a port on another. Users see their own, administrators everybody's.
 */
export function PortLinksPage({ admin }: { admin: boolean }) {
  const links = usePoll(hubApi.portLinks, 5000)
  const { run, busy } = useAction()
  const rows = links.data ?? []
  const remove = (l: PortLink) =>
    confirm(`Remove link ${l.id}? Connections through it are closed.`) && run(() => hubApi.removePortLink(l.id), `link ${l.id} removed`).then(links.reload)
  return (
    <div>
      <div className="page-title">
        <h1>Port links</h1>
        <span className="muted small">{admin ? "everybody's links" : 'your links'}, as the listening worker last reported them (longarm link ls)</span>
      </div>
      <ErrorBox error={links.error} />
      <table className="grid" data-testid="port-links">
        <thead>
          <tr>
            <th>id</th>
            <th>owner</th>
            <th>on</th>
            <th>target</th>
            <th>state</th>
            <th>connections</th>
            <th>bytes out/in</th>
            <th>expires</th>
            <th>job</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {rows.map((l) => {
            const st = l.status ?? {}
            const state = st.state || 'pending'
            return (
              <tr key={l.id} data-testid={`port-link-${l.id}`}>
                <td className="mono">{l.id}</td>
                <td title={l.created_by}>{l.owner}</td>
                <td className="mono">
                  {l.from} → 127.0.0.1:{l.listen_port || l.port}
                </td>
                <td className="mono">
                  {l.to}:{l.port}
                </td>
                <td>
                  <span className={`pill ${STATE_CLS[state] ?? ''}`}>{state}</span>
                  {st.error && <div className="small" style={{ color: 'var(--red)' }}>{st.error}</div>}
                  {state === 'listening' && st.path && <div className="small muted mono">{st.path}</div>}
                </td>
                <td className="mono" title="accepted / open now / refused (not the owner's)">
                  {num(st.connections)} / {st.active ?? 0} / {num(st.refused)}
                </td>
                <td className="mono">
                  {fmtBytes(st.bytes_sent)} / {fmtBytes(st.bytes_received)}
                </td>
                <td className="small" title={l.expires_at}>
                  {fmtExpires(l.expires_at)}
                </td>
                <td className="mono small">{l.job_id ? <Link to={`/jobs/${encodeURIComponent(l.job_id)}?worker=${encodeURIComponent(l.from ?? '')}`}>{l.job_id.slice(-10)}</Link> : '—'}</td>
                <td>
                  <button className="danger" disabled={busy} onClick={() => remove(l)}>
                    Remove
                  </button>
                </td>
              </tr>
            )
          })}
          {links.data && rows.length === 0 && (
            <tr>
              <td colSpan={10} className="notice" data-testid="port-links-empty">
                No port links. A port link makes a port of one worker reachable on another as 127.0.0.1:&lt;port&gt; (e.g. a database for a job's tests), for a limited time or as long as a job runs. Create one
                with <span className="mono">longarm link create &lt;worker&gt;:&lt;port&gt; --on &lt;worker&gt; [--port N] [--ttl 8h] [--job &lt;id&gt;]</span>.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
