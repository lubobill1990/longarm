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
 * The fix command as PowerShell (the default Windows terminal shell) runs
 * it: a quoted executable needs the call operator (`& "C:\...\longarm.exe"
 * firewall allow`; without it PowerShell reports "Unexpected token"). Newer
 * longarm builds send it so; older ones (or a copy another app ships) don't.
 */
export function psCommand(cmd: string): string {
  const c = cmd.trim()
  return c.startsWith('"') || c.startsWith("'") ? `& ${c}` : c
}

/** A path's directory, compared case-insensitively (Windows). */
function sameDir(file: string, dir: string): boolean {
  const norm = (p: string) => p.replace(/\//g, '\\').replace(/\\+$/, '').toLowerCase()
  const i = Math.max(file.lastIndexOf('\\'), file.lastIndexOf('/'))
  return i >= 0 && norm(file.slice(0, i)) === norm(dir)
}

/** What the tray's Allow did (/trayapi/firewall/allow). */
export interface FirewallFixResult {
  program: string
  ok: boolean
  message?: string
}

/**
 * A Windows Firewall problem (a Block rule left by a dismissed prompt, or
 * no allow rule) for the executable that does the hole punching: direct
 * connections fail and legs stay relayed. Shows the exact fix command
 * (PowerShell) and, in the tray, an Allow button: one administrator prompt
 * runs the installed longarm.exe's `firewall allow` for that executable.
 * installDir: where Longarm is installed - a program elsewhere is a copy
 * another app ships (e.g. TMIA Desktop's), which the card says.
 */
export function FirewallWarning({
  fw,
  relayed,
  onAllow,
  installDir,
}: {
  fw?: FirewallStatus
  relayed?: string[]
  onAllow?: () => Promise<FirewallFixResult>
  installDir?: string
}) {
  const toast = useToast()
  const { run, busy } = useAction()
  const [fixed, setFixed] = useState<FirewallFixResult>()
  if (!fw || fw.ok) return null
  if (fixed?.ok && fixed.program === fw.program) {
    // the local API re-checks within a minute; until then, what the tray saw
    return (
      <div className="ok-box" data-testid="firewall-fixed">
        <b>Windows Firewall:</b> {fixed.message ?? `allowed ${fixed.program}.`}
      </div>
    )
  }
  const fix = fw.fix ? psCommand(fw.fix) : ''
  const copy = !!fw.program && !!installDir && !sameDir(fw.program, installDir)
  return (
    <div className="warn-box" data-testid="firewall-warning">
      <div>
        <b>Windows Firewall:</b> {fw.problem}.
        {relayed && relayed.length > 0 && <> Relayed now: {relayed.join(', ')}.</>}
      </div>
      {copy && (
        <div className="fixcmd" data-testid="firewall-copy">
          This longarm runs from {fw.program}, not from where Longarm is installed ({installDir}): a copy another app
          ships (e.g. TMIA Desktop). Allowing it adds rules for that path; a new version of that app needs them again.
        </div>
      )}
      {onAllow && fw.program && (
        <div className="fixcmd">
          <button
            data-testid="firewall-allow"
            disabled={busy}
            onClick={async () => {
              const r = await run(onAllow)
              if (!r) return
              if (r.ok) setFixed(r)
              else toast(r.message ?? 'still blocked', true)
            }}
          >
            {busy ? 'waiting for the administrator prompt…' : 'Allow'}
          </button>{' '}
          inbound connections to {copy ? 'that copy' : 'it'} (an administrator prompt).
        </div>
      )}
      {fix && (
        <div className="fixcmd">
          {onAllow ? 'Or in' : 'Fix in'} an elevated PowerShell: <code data-testid="firewall-fix">{fix}</code>{' '}
          <button
            onClick={() =>
              navigator.clipboard?.writeText(fix).then(
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
