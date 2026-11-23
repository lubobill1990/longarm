import { useEffect, useRef, useState } from 'react'

/**
 * Confirmations drawn by the page, not the webview: WebView2's native
 * confirm() is a window of its own ("127.0.0.1:7490 says") that does not
 * fit the tray's popup. ask() shows a dialog when a ConfirmHost is mounted
 * (the tray, the Sessions window), else falls back to window.confirm (the
 * panel in a browser, tests); askText() likewise for prompt().
 */
export interface AskOptions {
  /** the confirming button's text (default OK) */
  ok?: string
  /** a destructive action: the button in red */
  danger?: boolean
}

/** askText(): the field's first value and its check. */
export interface AskTextOptions extends AskOptions {
  value?: string
  /** why the value can't be used ('' = it can): shown under the field, OK disabled */
  check?: (v: string) => string
  /** the field's accessible name (default: the message) */
  label?: string
}

interface Pending extends AskTextOptions {
  message: string
  /** a text field (askText) */
  input?: boolean
  resolve: (yes: boolean, value?: string) => void
}

let show: ((p: Pending) => void) | undefined

export function ask(message: string, opts: AskOptions = {}): Promise<boolean> {
  if (!show) return Promise.resolve(window.confirm(message))
  return new Promise((resolve) => show!({ ...opts, message, resolve }))
}

/** Asks for a line of text: the value (trimmed), undefined when canceled. */
export function askText(message: string, opts: AskTextOptions = {}): Promise<string | undefined> {
  if (!show) {
    const v = window.prompt(message, opts.value ?? '')
    return Promise.resolve(v === null ? undefined : v.trim())
  }
  return new Promise((resolve) => show!({ ...opts, message, input: true, resolve: (yes, v) => resolve(yes ? (v ?? '').trim() : undefined) }))
}

/** Renders ask()'s dialog; mount once per window. */
export function ConfirmHost() {
  const [p, setP] = useState<Pending>()
  const [text, setText] = useState('')
  const okRef = useRef<HTMLButtonElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    show = (next) =>
      setP((cur) => {
        cur?.resolve(false) // a newer question replaces an open one
        setText(next.value ?? '')
        return next
      })
    return () => {
      show = undefined
    }
  }, [])
  useEffect(() => {
    if (!p) return
    if (p.input) inputRef.current?.select()
    else okRef.current?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') answer(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [p]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!p) return null
  function answer(yes: boolean) {
    p?.resolve(yes, text)
    setP(undefined)
  }
  const problem = p.input && p.check ? p.check(text.trim()) : ''
  const blocked = !!p.input && (!text.trim() || !!problem)
  return (
    <div className="ask-backdrop" data-testid="ask" onMouseDown={(e) => e.target === e.currentTarget && answer(false)}>
      <form
        className="ask"
        role="alertdialog"
        aria-modal="true"
        aria-describedby="ask-message"
        // Enter in the field (one text field: the form's implicit submission)
        onSubmit={(e) => {
          e.preventDefault()
          if (!blocked) answer(true)
        }}
      >
        <p id="ask-message" className="ask-message">
          {p.message}
        </p>
        {p.input && (
          <div className="ask-field">
            <input
              ref={inputRef}
              type="text"
              aria-label={p.label ?? p.message}
              aria-invalid={!!problem}
              value={text}
              spellCheck={false}
              autoComplete="off"
              onChange={(e) => setText(e.target.value)}
            />
            {problem && (
              <div className="small red" role="alert" data-testid="ask-problem">
                {problem}
              </div>
            )}
          </div>
        )}
        <div className="ask-buttons">
          <button type="button" onClick={() => answer(false)}>
            Cancel
          </button>
          <button type="button" ref={okRef} className={p.danger ? 'danger-solid' : 'primary'} disabled={blocked} onClick={() => answer(true)}>
            {p.ok ?? 'OK'}
          </button>
        </div>
      </form>
    </div>
  )
}
