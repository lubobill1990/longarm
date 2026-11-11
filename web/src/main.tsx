import { lazy, StrictMode, Suspense } from 'react'
import { createRoot } from 'react-dom/client'
import { TooltipHost } from './components/Tooltip'
import { PanelApp } from './panel/PanelApp'
import './styles.css'
import { TrayApp } from './tray/TrayApp'
import { applyTheme, loadTheme } from './tray/theme'

// their own chunks: the popup and the panel don't load xterm.js
const SessionsApp = lazy(() => import('./sessions/SessionsApp').then((m) => ({ default: m.SessionsApp })))
const UpdateProgressApp = lazy(() => import('./tray/UpdateProgress').then((m) => ({ default: m.UpdateProgressApp })))

// One bundle, four shells: the hub serves it as the web panel; longarm-tray
// serves it too and opens /tray in its popup window, /sessions in its
// Sessions window; /update is its update progress window (served by that
// window itself: no tray settings, the theme from ?theme=).
const tray = location.pathname === '/tray' || location.pathname.startsWith('/tray/')
const sessions = location.pathname === '/sessions'
const progress = location.pathname === '/update'

if (tray || sessions || progress) {
  // the tray's design tokens (tray/tokens.css) and its theme before the first paint
  document.documentElement.dataset.app = 'tray'
  if (progress) document.documentElement.dataset.theme = new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark'
  else applyTheme(loadTheme())
}

// TooltipHost: every title="..." in any of them shows in our tooltip
createRoot(document.getElementById('root')!).render(<StrictMode>{sessions || progress ? (
      <Suspense fallback={null}>
        {sessions ? <SessionsApp /> : <UpdateProgressApp />}
      </Suspense>
    ) : tray ? <TrayApp /> : <PanelApp />}<TooltipHost /></StrictMode>)
