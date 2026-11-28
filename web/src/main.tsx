import { lazy, StrictMode, Suspense } from 'react'
import { createRoot } from 'react-dom/client'
import { TooltipHost } from './components/Tooltip'
import { PanelApp } from './panel/PanelApp'
import './styles.css'
import { TrayApp } from './tray/TrayApp'
import { applyTheme, loadTheme } from './tray/theme'

// its own chunk: the popup and the panel don't load xterm.js
const SessionsApp = lazy(() => import('./sessions/SessionsApp').then((m) => ({ default: m.SessionsApp })))
const MobileApp = lazy(() => import('./mobile/MobileApp').then((m) => ({ default: m.MobileApp })))

// One bundle, four shells: the hub serves it as the web panel; longarm-tray
// serves it too and opens /tray in its popup window, /sessions in its
// Sessions window; the phone app's core (internal/mobile) opens /m.
const tray = location.pathname === '/tray' || location.pathname.startsWith('/tray/')
const sessions = location.pathname === '/sessions'
const mobile = location.pathname === '/m'

if (tray || sessions || mobile) {
  // the tray's design tokens (tray/tokens.css) and its theme before the first paint
  document.documentElement.dataset.app = 'tray'
  applyTheme(loadTheme())
}

// TooltipHost: every title="..." in any of the three shows in our tooltip
createRoot(document.getElementById('root')!).render(<StrictMode>{mobile ? (
      <Suspense fallback={null}>
        <MobileApp />
      </Suspense>
    ) : sessions ? (
      <Suspense fallback={null}>
        <SessionsApp />
      </Suspense>
    ) : tray ? <TrayApp /> : <PanelApp />}<TooltipHost /></StrictMode>)
