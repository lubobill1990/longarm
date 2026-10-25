import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { PanelApp } from './panel/PanelApp'
import './styles.css'
import { TrayApp } from './tray/TrayApp'

// One bundle, two shells: the hub serves it as the web panel; longarm-tray
// serves it too and opens /tray in its popup window.
const tray = location.pathname === '/tray' || location.pathname.startsWith('/tray/')

createRoot(document.getElementById('root')!).render(<StrictMode>{tray ? <TrayApp /> : <PanelApp />}</StrictMode>)
