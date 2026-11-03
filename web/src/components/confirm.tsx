import { useEffect, useRef, useState } from 'react'

/**
 * Confirmations drawn by the page, not the webview: WebView2's native
 * confirm() is a window of its own ("127.0.0.1:7490 says") that does not
 * fit the tray's popup. ask() shows a dialog when a ConfirmHost is mounted
 * (the tray, the Sessions window), else falls back to window.confirm (the
 * panel in a browser, tests).
 */
export interface AskOptions {
  /** the confirming button's text (default OK) */
  ok?: string
  /** a destructive action: the button in red */
  danger?: boolean
}

interface Pending extends AskOptions {
  message: string
  resolve: (yes: boolean) => void
}

let show: ((p: Pending) => void) | undefined

export function ask(message: string, opts: AskOptions = {}): Promise<boolean> {
  if (!show) return Promise.resolve(window.confirm(message))
  return new Promise((resolve) => show!({ ...opts, message, resolve }))
}

/** Renders ask()'s dialog; mount once per window. */
export function ConfirmHost() {
  const [p, setP] = useState<Pending>()
  const okRef = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    show = (next) =>
      setP((cur) => {
        cur?.resolve(false) // a newer question replaces an open one
        return next
      })
    return () => {
      show = undefined
    }
  }, [])
  useEffect(() => {
    if (!p) return
    okRef.current?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') answer(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [p]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!p) return null
  function answer(yes: boolean) {
    p?.resolve(yes)
    setP(undefined)
  }
  return (
    <div className="ask-backdrop" data-testid="ask" onMouseDown={(e) => e.target === e.currentTarget && answer(false)}>
      <div className="ask" role="alertdialog" aria-modal="true" aria-describedby="ask-message">
        <p id="ask-message" className="ask-message">
          {p.message}
        </p>
        <div className="ask-buttons">
          <button type="button" onClick={() => answer(false)}>
            Cancel
          </button>
          <button type="button" ref={okRef} className={p.danger ? 'danger-solid' : 'primary'} onClick={() => answer(true)}>
            {p.ok ?? 'OK'}
          </button>
        </div>
      </div>
    </div>
  )
}
