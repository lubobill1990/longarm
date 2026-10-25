import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import type { Stream } from '../api/http'
import type { FirewallStatus, LogChunk } from '../api/types'
import { logDecoder } from '../lib/format'

/** Calls fn now and every ms; returns the latest value / error and a reload. */
export function usePoll<T>(fn: () => Promise<T>, ms: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const load = useCallback(async () => {
    try {
      setData(await fnRef.current())
      setError(undefined)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])
  useEffect(() => {
    load()
    if (ms <= 0) return
    const t = setInterval(load, ms)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [load, ms, ...deps])
  return { data, error, reload: load, setData }
}

// ---------------------------------------------------------------- toasts

interface Toast {
  id: number
  text: string
  error?: boolean
}

const ToastCtx = createContext<(text: string, error?: boolean) => void>(() => {})

export function Toasts({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const seq = useRef(0)
  const push = useCallback((text: string, error = false) => {
    const id = ++seq.current
    setToasts((t) => [...t.slice(-4), { id, text, error }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), error ? 8000 : 4000)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" role="status">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.error ? 'error' : ''}`}>
            {t.text}
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  )
}

export const useToast = () => useContext(ToastCtx)

/** Wraps an async action: toast on error (and on success, if ok is given). */
export function useAction() {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const run = useCallback(
    async <T,>(fn: () => Promise<T>, ok?: string | ((r: T) => string)): Promise<T | undefined> => {
      setBusy(true)
      try {
        const r = await fn()
        if (ok) toast(typeof ok === 'function' ? ok(r) : ok)
        return r
      } catch (e) {
        toast(e instanceof Error ? e.message : String(e), true)
        return undefined
      } finally {
        setBusy(false)
      }
    },
    [toast],
  )
  return { run, busy }
}

// ---------------------------------------------------------------- logs

const MAX_LOG = 2 << 20 // keep the last 2 MiB in the view

/**
 * Follows a job's output (SSE log chunks) with auto-scroll while at the
 * bottom. open() starts the stream.
 */
export function LogView({ open, testId }: { open: (onChunk: (c: LogChunk) => void, onDone: (err?: string) => void) => Stream; testId?: string }) {
  const [text, setText] = useState('')
  const [state, setState] = useState<'following' | 'ended' | 'error'>('following')
  const [err, setErr] = useState<string>()
  const pre = useRef<HTMLPreElement>(null)
  const stick = useRef(true)
  useEffect(() => {
    setText('')
    setState('following')
    const decode = logDecoder()
    const s = open(
      (c) => {
        const t = decode(c.data)
        setText((old) => {
          const n = old + t
          return n.length > MAX_LOG ? n.slice(n.length - MAX_LOG) : n
        })
      },
      (e) => {
        setState(e ? 'error' : 'ended')
        setErr(e)
      },
    )
    return () => s.close()
  }, [open])
  useEffect(() => {
    const el = pre.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [text])
  return (
    <div>
      <div className="row small muted" style={{ marginBottom: 4 }}>
        <span data-testid="log-state">{state === 'following' ? 'following…' : state === 'ended' ? 'end of output' : `error: ${err}`}</span>
        <span className="spacer" />
        <span>{text.length.toLocaleString()} chars</span>
      </div>
      <pre
        className="logview"
        ref={pre}
        data-testid={testId ?? 'log'}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 8
        }}
      >
        {text}
      </pre>
    </div>
  )
}

export function ErrorBox({ error }: { error?: string }) {
  return error ? <div className="error-box">{error}</div> : null
}

export function StatePill({ state }: { state?: string }) {
  const s = (state ?? '').replace('JOB_STATE_', '').toLowerCase()
  const cls = s === 'done' || s === 'approved' || s === 'ok' ? 'ok' : s === 'running' || s === 'preparing' ? 'info' : s === 'failed' || s === 'lost' || s === 'revoked' || s === 'halted' ? 'bad' : s === 'killed' || s === 'pending' || s === 'canary' || s === 'applying' ? 'warn' : ''
  return <span className={`pill ${cls}`}>{s || '—'}</span>
}

/**
 * A Windows Firewall problem (a Block rule left by a dismissed prompt, or
 * no allow rule) for the executable that does the hole punching: direct
 * connections fail and legs stay relayed. Shows the exact fix command.
 */
export function FirewallWarning({ fw, relayed }: { fw?: FirewallStatus; relayed?: string[] }) {
  const toast = useToast()
  if (!fw || fw.ok) return null
  return (
    <div className="warn-box" data-testid="firewall-warning">
      <div>
        <b>Windows Firewall:</b> {fw.problem}.
        {relayed && relayed.length > 0 && <> Relayed now: {relayed.join(', ')}.</>}
      </div>
      {fw.fix && (
        <div className="fixcmd">
          Fix in an elevated terminal: <code data-testid="firewall-fix">{fw.fix}</code>{' '}
          <button
            onClick={() =>
              navigator.clipboard?.writeText(fw.fix ?? '').then(
                () => toast('command copied'),
                () => toast('copy failed', true),
              )
            }
          >
            copy
          </button>
        </div>
      )}
    </div>
  )
}
