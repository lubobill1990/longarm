import { hubApi } from '../api/hub'
import { ErrorBox, usePoll } from '../components/common'
import { AMBER, fmtAgo, fmtRTT, GREEN, isRelayed, RED, since } from '../lib/format'

/** Per client -> worker pair: the legs and the path each one takes. */
export function LinksPage() {
  const links = usePoll(hubApi.links, 5000)
  const rows = (links.data ?? []).flatMap((r) => (r.links ?? []).map((l) => ({ client: r.client ?? '', time: r.time, l })))
  return (
    <div>
      <div className="page-title">
        <h1>Links</h1>
        <span className="muted small">reported every 10 s by each client's longarm daemon (longarm admin links)</span>
      </div>
      <ErrorBox error={links.error} />
      <table className="grid" data-testid="links">
        <thead>
          <tr>
            <th>client</th>
            <th>worker</th>
            <th>state</th>
            <th>active leg</th>
            <th>RTT</th>
            <th>all legs</th>
            <th>reported</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(({ client, time, l }) => (
            <tr key={`${client}/${l.worker}`}>
              <td>{client}</td>
              <td>{l.worker}</td>
              <td style={{ color: l.state === 'online' ? GREEN : RED }}>{l.state}</td>
              <td className="mono">{l.active}</td>
              <td className="mono">{fmtRTT(l.rtt_ms)}</td>
              <td className="small">
                {(l.legs ?? []).map((g, i) => (
                  <div key={i} className="mono" style={{ color: !g.up ? RED : isRelayed(g.kind) ? AMBER : undefined }} title={g.error || g.detail}>
                    {g.active ? '▸ ' : '  '}
                    {g.label || g.transport} {g.up ? fmtRTT(g.rtt_ms) : 'down'}
                    {g.nat ? ` nat ${g.nat}` : ''}
                    {g.relay_since ? ` (relayed ${fmtAgo(since(g.relay_since))})` : ''}
                  </div>
                ))}
              </td>
              <td className="small muted">{fmtAgo(since(time))} ago</td>
            </tr>
          ))}
          {rows.length === 0 && (
            <tr>
              <td colSpan={7} className="notice">
                No client has reported yet (clients report while <span className="mono">longarm daemon</span> runs, e.g. under longarm-tray).
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
