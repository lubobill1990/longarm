import { traySettingsApi } from '../api/local'
import { ErrorBox, useAction, usePoll } from '../components/common'
import { Section } from './ui'

/**
 * Settings > Startup: "Start Longarm at login" - the tray's own login item
 * (Windows: the Run key, macOS: a LaunchAgent, Linux: an autostart .desktop
 * file), read from the OS each time. Not the worker: a user-session worker
 * starts at logon by its own task whatever this says.
 */
export function StartAtLogin() {
  const st = usePoll(traySettingsApi.get, 0)
  const { run, busy } = useAction()
  const a = st.data?.autostart
  if (!a?.supported) return null
  const help = a.machine_wide
    ? 'Installed to start for every user of this computer: turning it off asks for an administrator and turns it off for everybody'
    : 'Shows the tray icon (your workers, notifications) when you sign in to this computer'
  return (
    <Section card title="Startup" testId="startup">
      <ErrorBox error={st.error || a.error} />
      <label className="switch-row">
        <input
          type="checkbox"
          className="switch"
          aria-label="start Longarm at login"
          data-testid="autostart"
          checked={a.enabled}
          disabled={busy}
          onChange={(e) => {
            const on = e.target.checked
            run(() => traySettingsApi.set({ autostart: on }), on ? 'Longarm starts at login' : 'Longarm no longer starts at login').then(
              // the OS's answer (a declined administrator prompt leaves it as it was)
              (r) => (r ? st.setData(r) : st.reload()),
            )
          }}
        />
        <span>
          <span className="field-label">Start Longarm at login</span>
          <span className="field-help" title={a.where}>
            {help}
          </span>
        </span>
      </label>
    </Section>
  )
}
