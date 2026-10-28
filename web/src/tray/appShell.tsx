import { useEffect, useState } from 'react'
import { trayApi } from '../api/local'

/**
 * The tray popup behaves as an app, not a browser page: links open in the
 * system browser (never inside the popup), files dropped on it do
 * nothing, and the right-click menu is ours - Cut / Copy / Paste / Select
 * all where there is text to copy or a field to edit, nothing elsewhere
 * (the WebView2's own menu - Back, Refresh, Save as, Print, Inspect - is
 * off). Opened in a normal browser (longarm-tray --browser) the browser's
 * menu stays.
 */

/** An external (http/https, other origin) link: open it in the system browser. */
export function externalHref(a: HTMLAnchorElement | null, origin = location.origin): string | undefined {
  if (!a || !a.href) return undefined
  try {
    const u = new URL(a.href, origin)
    if ((u.protocol === 'http:' || u.protocol === 'https:') && u.origin !== origin) return u.href
  } catch {
    // not a URL
  }
  return undefined
}

/** Installs the link and drop handlers (returns the uninstaller). */
export function installAppBehaviour(open: (url: string) => unknown = trayApi.open): () => void {
  const onClick = (e: MouseEvent) => {
    const a = (e.target as Element | null)?.closest?.('a') as HTMLAnchorElement | null
    const href = externalHref(a)
    if (href) {
      e.preventDefault()
      open(href)
    }
  }
  const noDrop = (e: DragEvent) => {
    if (e.dataTransfer?.types?.includes('Files')) {
      e.preventDefault()
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'none'
    }
  }
  const origOpen = window.open
  window.open = ((url?: string | URL) => {
    if (url) open(String(url))
    return null
  }) as typeof window.open
  document.addEventListener('click', onClick, true)
  window.addEventListener('dragover', noDrop)
  window.addEventListener('drop', noDrop)
  return () => {
    document.removeEventListener('click', onClick, true)
    window.removeEventListener('dragover', noDrop)
    window.removeEventListener('drop', noDrop)
    window.open = origOpen
  }
}

/** In longarm-tray's webview (not a normal browser tab). */
export function inWebview() {
  const w = window as unknown as { chrome?: { webview?: unknown }; webkit?: { messageHandlers?: { external?: unknown } } }
  return !!(w.chrome?.webview || w.webkit?.messageHandlers?.external)
}

type Item = { label: string; run: () => void }

function editable(el: Element | null): HTMLInputElement | HTMLTextAreaElement | undefined {
  if (el instanceof HTMLTextAreaElement) return el.readOnly || el.disabled ? undefined : el
  if (el instanceof HTMLInputElement && ['text', 'search', 'url', 'email', 'password', 'number', ''].includes(el.type))
    return el.readOnly || el.disabled ? undefined : el
  return undefined
}

/** The menu items for a right click on target with selection sel. */
export function menuItems(target: Element | null, sel: string): Item[] {
  const field = editable(target)
  const items: Item[] = []
  const selected = field ? field.value.slice(field.selectionStart ?? 0, field.selectionEnd ?? 0) : sel
  if (field && selected) {
    items.push({
      label: 'Cut',
      run: () => {
        navigator.clipboard?.writeText(selected)
        field.setRangeText('', field.selectionStart ?? 0, field.selectionEnd ?? 0, 'end')
        field.dispatchEvent(new Event('input', { bubbles: true }))
      },
    })
  }
  if (selected) items.push({ label: 'Copy', run: () => navigator.clipboard?.writeText(selected) })
  if (field) {
    items.push({
      label: 'Paste',
      run: () =>
        navigator.clipboard?.readText().then((t) => {
          field.focus()
          field.setRangeText(t, field.selectionStart ?? 0, field.selectionEnd ?? 0, 'end')
          field.dispatchEvent(new Event('input', { bubbles: true }))
        }),
    })
    items.push({ label: 'Select all', run: () => field.select() })
  }
  return items
}

/** The popup's own right-click menu (only in the webview). */
export function ContextMenu() {
  const [menu, setMenu] = useState<{ x: number; y: number; items: Item[] }>()
  useEffect(() => {
    if (!inWebview()) return
    const onMenu = (e: MouseEvent) => {
      e.preventDefault()
      const items = menuItems(e.target as Element | null, window.getSelection()?.toString() ?? '')
      setMenu(items.length ? { x: e.clientX, y: e.clientY, items } : undefined)
    }
    const close = () => setMenu(undefined)
    document.addEventListener('contextmenu', onMenu)
    window.addEventListener('blur', close)
    window.addEventListener('scroll', close, true)
    return () => {
      document.removeEventListener('contextmenu', onMenu)
      window.removeEventListener('blur', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [])
  if (!menu) return null
  return (
    <div className="ctx-backdrop" onMouseDown={() => setMenu(undefined)} onContextMenu={(e) => e.preventDefault()}>
      <ul className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }} onMouseDown={(e) => e.stopPropagation()}>
        {menu.items.map((it) => (
          <li key={it.label} role="menuitem">
            <button
              onMouseDown={(e) => e.preventDefault() /* keep the selection */}
              onClick={() => {
                it.run()
                setMenu(undefined)
              }}
            >
              {it.label}
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}
