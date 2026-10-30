import { useEffect, useRef } from 'react'
import type { AgentSession } from '../api/sessions'
import { IconClose } from '../components/icons'
import { AgentIcon, stateMark } from './SessionList'

export interface Tab {
  /** the key the tab was opened with (stable while the list's keys change) */
  id: string
  item: AgentSession
  /** new output or a waiting agent while in the background */
  badge?: boolean
}

/** The tab a shortcut goes to: Ctrl+Tab / Ctrl+Shift+Tab cycle, Ctrl+1..9 (9 = the last). */
export function shortcutTarget(e: Pick<KeyboardEvent, 'ctrlKey' | 'shiftKey' | 'key'>, ids: string[], active?: string): string | undefined {
  if (!e.ctrlKey || !ids.length) return undefined
  const i = active ? ids.indexOf(active) : -1
  if (e.key === 'Tab') return ids[(i + (e.shiftKey ? -1 : 1) + ids.length) % ids.length]
  if (/^[1-9]$/.test(e.key)) {
    const n = Number(e.key)
    return n === 9 ? ids[ids.length - 1] : ids[n - 1]
  }
  return undefined
}

/** One tab per open session; closing a tab detaches (the session keeps running). */
export function SessionTabs({ tabs, active, onSelect, onClose }: { tabs: Tab[]; active?: string; onSelect: (id: string) => void; onClose: (id: string) => void }) {
  const ref = useRef({ tabs, active, onSelect })
  ref.current = { tabs, active, onSelect }
  useEffect(() => {
    // capture: before the terminal (xterm.js) takes the keys
    const onKey = (e: KeyboardEvent) => {
      const { tabs, active, onSelect } = ref.current
      const to = shortcutTarget(e, tabs.map((t) => t.id), active)
      if (!to) return
      e.preventDefault()
      e.stopPropagation()
      onSelect(to)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [])
  if (!tabs.length) return null
  return (
    <div className="stabs" role="tablist" data-testid="session-tabs">
      {tabs.map((t, i) => {
        const st = stateMark(t.item)
        const on = t.id === active
        return (
          <div key={t.id} className={`stab ${on ? 'active' : ''}`} data-testid={`tab-${t.id}`}>
            <button
              role="tab"
              className="stab-main"
              aria-selected={on}
              title={`${t.item.worker}: ${t.item.title || t.item.session || ''}${i < 9 ? ` (Ctrl+${i + 1})` : ''}`}
              onClick={() => onSelect(t.id)}
            >
              <span className={`sdot ${st.cls}`} aria-hidden="true">
                {st.mark}
              </span>
              <AgentIcon agent={t.item.agent} />
              <span className="stab-title">{t.item.title || t.item.session || t.item.id}</span>
              {t.badge && !on && <span className="stab-badge" data-testid="tab-badge" aria-label="new activity" />}
            </button>
            <button className="stab-close icon-btn" aria-label={`close ${t.item.title || t.id}`} title="close (detaches; the session keeps running)" onClick={() => onClose(t.id)}>
              <IconClose size={12} />
            </button>
          </div>
        )
      })}
    </div>
  )
}
