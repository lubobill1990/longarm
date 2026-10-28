import { useEffect, useState } from 'react'
import { trayApi, type TrayUpdate } from '../api/local'
import { useAction } from '../components/common'
import { Banner } from './ui'

/**
 * "Update to X": the hub has a newer release. The button has longarm-tray
 * download the MSI, check its sha256 and signature, and run it; a failed
 * check is shown and nothing runs.
 */
export function UpdateBanner() {
  const [u, setU] = useState<TrayUpdate>()
  const { run, busy } = useAction()
  const busyState = u?.state === 'downloading'
  useEffect(() => {
    let alive = true
    const load = () =>
      trayApi
        .update()
        .then((r) => alive && setU(r))
        .catch(() => {}) // an older tray or no hub: no banner
    load()
    const t = setInterval(load, busyState ? 2000 : 60000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [busyState])
  if (!u || (!u.available && u.state !== 'failed')) return null
  if (u.state === 'installing') {
    return (
      <Banner tone="info" testId="update-banner">
        Installing Longarm {u.latest}… (the installer may ask for permission)
      </Banner>
    )
  }
  const latest = u.latest ?? ''
  return (
    <>
      <Banner
        tone="info"
        testId="update-banner"
        action={
          <button className="primary" disabled={busy || busyState || !latest} onClick={() => run(() => trayApi.installUpdate(latest).then(() => trayApi.update().then(setU)))}>
            {busyState ? 'Downloading…' : `Update to ${latest}`}
          </button>
        }
      >
        Longarm {latest} is available <span className="muted small">(you have {u.current})</span>
      </Banner>
      {u.state === 'failed' && (
        <Banner tone="error" testId="update-error">
          Not installed: {u.error}
        </Banner>
      )}
    </>
  )
}
