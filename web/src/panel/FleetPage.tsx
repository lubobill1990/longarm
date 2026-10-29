import { useState } from 'react'
import { hubApi } from '../api/hub'
import type { FleetCounts } from '../api/types'
import { ErrorBox, usePoll } from '../components/common'

/**
 * The administrators' fleet-wide view (Admin.FleetSummary): per user the
 * number of workers and devices and how many workers are online, the
 * workers' versions and - optionally - each user's machines by version.
 * Aggregates only: no names, IDs, OS, last-seen times, labels, CPU or
 * memory, processes or jobs, nothing per machine.
 */
export function FleetPage() {
  const [rows, setRows] = useState(false)
  const sum = usePoll(() => hubApi.fleetSummary(rows), 30000, [rows])
  const s = sum.data
  const cells = (c?: FleetCounts) => (
    <>
      <td>{c?.workers ?? 0}</td>
      <td>{c?.workers_online ?? 0}</td>
      <td>{c?.admin_workers ?? 0}</td>
      <td>{c?.admin_workers_online ?? 0}</td>
      <td>{c?.clients ?? 0}</td>
      <td>{c?.pending ?? 0}</td>
    </>
  )
  return (
    <div>
      <div className="page-title">
        <h1>Fleet</h1>
        <span className="muted small">counts per user, no machine names (every request is audited)</span>
        <span className="spacer" />
        <label className="small">
          <input type="checkbox" checked={rows} onChange={(e) => setRows(e.target.checked)} /> versions per user
        </label>
      </div>
      <ErrorBox error={sum.error} />
      {s && !s.tenancy && (
        <p className="muted small">Multi-tenancy is off: every login sees every worker on the Nodes page.</p>
      )}
      <table className="grid" data-testid="fleet">
        <thead>
          <tr>
            <th>user</th>
            <th>workers</th>
            <th>online</th>
            <th>admin workers</th>
            <th>online</th>
            <th>devices</th>
            <th>pending</th>
          </tr>
        </thead>
        <tbody>
          {(s?.users ?? []).map((u) => (
            <tr key={u.user || '(fleet)'} data-testid={`fleet-${u.user || 'fleet'}`}>
              <td>{u.user || <span className="muted">fleet workers (no owner)</span>}</td>
              {cells(u.counts)}
            </tr>
          ))}
          {s && (
            <tr data-testid="fleet-total">
              <td>
                <b>total</b>
              </td>
              {cells(s.totals)}
            </tr>
          )}
        </tbody>
      </table>
      {(s?.worker_versions ?? []).length > 0 && (
        <>
          <h3 className="muted" style={{ marginTop: 24 }}>
            Worker versions
          </h3>
          <table className="grid" data-testid="fleet-versions">
            <thead>
              <tr>
                <th>version</th>
                <th>workers</th>
                <th>online</th>
              </tr>
            </thead>
            <tbody>
              {(s?.worker_versions ?? []).map((v) => (
                <tr key={v.version || '-'}>
                  <td className="mono">{v.version || '–'}</td>
                  <td>{v.workers ?? 0}</td>
                  <td>{v.online ?? 0}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
      {rows &&
        (s?.users ?? [])
          .filter((u) => (u.versions ?? []).length > 0)
          .map((u) => (
            <div key={u.user || '(fleet)'} style={{ marginTop: 16 }}>
              <h3 className="muted">{u.user || 'fleet workers'}</h3>
              <table className="grid" data-testid={`fleet-rows-${u.user || 'fleet'}`}>
                <thead>
                  <tr>
                    <th>version</th>
                    <th>workers</th>
                    <th>online</th>
                    <th>devices</th>
                  </tr>
                </thead>
                <tbody>
                  {(u.versions ?? []).map((v) => (
                    <tr key={v.version || '-'}>
                      <td className="mono">{v.version || '–'}</td>
                      <td>{v.workers ?? 0}</td>
                      <td>{v.online ?? 0}</td>
                      <td>{v.clients ?? 0}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ))}
    </div>
  )
}
