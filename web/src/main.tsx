import { lazy, StrictMode, Suspense } from 'react'
import { createRoot } from 'react-dom/client'
import { PanelApp } from './panel/PanelApp'
import './styles.css'
import { TrayApp } from './tray/TrayApp'
import { applyTheme, loadTheme } from './tray/theme'

// its own chunk: the popup and the panel don't load xterm.js
const SessionsApp = lazy(() => import('./sessions/SessionsApp').then((m) => ({ default: m.SessionsApp })))

// One bundle, three shells: the hub serves it as the web panel; longarm-tray
// serves it too and opens /tray in its popup window, /sessions in its
// Sessions window.
const tray = location.pathname === '/tray' || location.pathname.startsWith('/tray/')
const sessions = location.pathname === '/sessions'

if (tray || sessions) {
  // the tray's design tokens (tray/tokens.css) and its theme before the first paint
  document.documentElement.dataset.app = 'tray'
  applyTheme(loadTheme())
}

createRoot(document.getElementById('root')!).render(<StrictMode>{sessions ? (
      <Suspense fallback={null}>
        <SessionsApp />
      </Suspense>
    ) : tray ? <TrayApp /> : <PanelApp />}</StrictMode>)
