import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { PanelApp } from './panel/PanelApp'
import './styles.css'
import { TrayApp } from './tray/TrayApp'
import { applyTheme, loadTheme } from './tray/theme'

// One bundle, two shells: the hub serves it as the web panel; longarm-tray
// serves it too and opens /tray in its popup window.
const tray = location.pathname === '/tray' || location.pathname.startsWith('/tray/')

if (tray) {
  // the tray's design tokens (tray/tokens.css) and its theme before the first paint
  document.documentElement.dataset.app = 'tray'
  applyTheme(loadTheme())
}

createRoot(document.getElementById('root')!).render(<StrictMode>{tray ? <TrayApp /> : <PanelApp />}</StrictMode>)
