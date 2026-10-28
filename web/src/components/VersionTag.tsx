import type { UpdateAvailable } from '../api/types'
import { isOutdated } from '../lib/release'

/**
 * A node's version, marked when the hub has a newer release: "outdated"
 * (latest from /releases/latest.json), with the worker's reason when it
 * can't update itself (update_available).
 */
export function VersionTag({ version, latest, ua, testId }: { version?: string; latest?: string; ua?: UpdateAvailable; testId?: string }) {
  const target = ua?.version || latest
  const old = !!ua?.version || isOutdated(version, latest)
  return (
    <>
      {version}
      {old && target && (
        <span
          className="pill warn"
          style={{ marginLeft: 4 }}
          data-testid={testId}
          title={`Longarm ${target} is available${ua?.reason ? ` - it can't update itself: ${ua.reason}; install the new MSI or run the install script again` : ''}`}
        >
          outdated → {target}
        </span>
      )}
    </>
  )
}
