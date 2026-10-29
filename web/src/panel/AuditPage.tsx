import { useState } from 'react'
import { hubApi } from '../api/hub'
import { ErrorBox, usePoll } from '../components/common'
import { fmtTime } from '../lib/format'

export function AuditPage() {
  const [node, setNode] = useState('')
  const [open, setOpen] = useState<number>()
  const nodes = usePoll(() => hubApi.nodes(false), 60000)
  const audit = usePoll(() => hubApi.auditView(node, 300), 10000, [node])
  const hidden = audit.data?.hidden_total ?? 0
  return (
    <div>
      <div className="page-title">
        <h1>Audit</h1>
        <select aria-label="node filter" value={node} onChange={(e) => setNode(e.target.value)}>
          <option value="">all workers</option>
          {(nodes.data ?? []).map((n) => (
            <option key={n.name}>{n.name}</option>
          ))}
        </select>
      </div>
      <ErrorBox error={audit.error} />
      {hidden > 0 && (
        <p className="muted small" data-testid="audit-hidden">
          {hidden} records of other users are not shown, only counted:{' '}
          {(audit.data?.hidden ?? []).map((c) => `${(c.method ?? '').replace('/longarm.v1.', '')} ${c.count ?? 0}`).join(', ')}. The
          complete trail is kept by the hub&apos;s operators (longarm-hub audit).
        </p>
      )}
      <table className="grid" data-testid="audit">
        <thead>
          <tr>
            <th>time</th>
            <th>worker</th>
            <th>who</th>
            <th>what</th>
            <th>details</th>
          </tr>
        </thead>
        <tbody>
          {(audit.data?.records ?? []).map((r, i) => {
            let details = ''
            try {
              const o = JSON.parse(r.json ?? '{}')
              delete o.time
              delete o.who
              delete o.method
              details = JSON.stringify(o)
            } catch {
              details = r.json ?? ''
            }
            return (
              <tr key={i} onClick={() => setOpen(open === i ? undefined : i)}>
                <td className="small nowrap">{fmtTime(r.time)}</td>
                <td>{r.node}</td>
                <td>{r.who}</td>
                <td className="mono small">{(r.method ?? '').replace('/longarm.v1.', '')}</td>
                <td className="mono small" style={open === i ? { whiteSpace: 'pre-wrap', wordBreak: 'break-all' } : { maxWidth: 520, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {details}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
