import { useEffect, useRef, useState } from 'react'
import { attachSession, type AttachHandlers, type Attachment } from '../api/sessions'
import { createTerminal, type Term } from './terminal'

/** Re-attach delays (ms) after a lost connection; the last one repeats. */
export const BACKOFF = [500, 1000, 2000, 5000, 10000]

type Status = { kind: 'connecting' } | { kind: 'live' } | { kind: 'retry'; why: string } | { kind: 'exit'; code: number }

/**
 * A Longarm session's terminal: xterm.js over the attach WebSocket (binary
 * frames both ways, resize on fit). The session's replay restores the
 * screen on every (re)connect; a lost connection re-attaches with backoff;
 * the program's end shows "session ended (code N)". Unmounting detaches.
 */
export function TerminalView({
  worker,
  session,
  job,
  visible,
  onOutput,
  onExit,
}: {
  worker: string
  session: string
  /** session is a steerable job's id */
  job?: boolean
  visible: boolean
  /** output arrived (badges on background tabs) */
  onOutput?: () => void
  onExit?: (code: number) => void
}) {
  const el = useRef<HTMLDivElement>(null)
  const term = useRef<Term>(undefined)
  const conn = useRef<Attachment>(undefined)
  const [status, setStatus] = useState<Status>({ kind: 'connecting' })
  const cb = useRef({ onOutput, onExit })
  cb.current = { onOutput, onExit }

  useEffect(() => {
    const t = createTerminal(el.current!)
    term.current = t
    let stopped = false
    let tries = 0
    let timer: ReturnType<typeof setTimeout> | undefined
    t.onInput((b) => conn.current?.input(b))
    const connect = () => {
      const { cols, rows } = t.size()
      setStatus({ kind: 'connecting' })
      const handlers: AttachHandlers = {
        onData: (b) => {
          t.write(b)
          cb.current.onOutput?.()
        },
        onControl: (c) => {
          if (c.type === 'started') {
            // the replay follows: start from a clean screen
            t.reset()
            tries = 0
            // the size it was fitted to while connecting
            const sz = t.size()
            conn.current?.resize(sz.cols, sz.rows)
            setStatus({ kind: 'live' })
          } else if (c.type === 'exit') {
            stopped = true
            setStatus({ kind: 'exit', code: c.code })
            cb.current.onExit?.(c.code)
          } else if (c.type === 'error') {
            setStatus({ kind: 'retry', why: c.message })
          }
        },
        onClose: (code) => {
          conn.current = undefined
          if (stopped) return
          const delay = BACKOFF[Math.min(tries++, BACKOFF.length - 1)]
          setStatus((s) => (s.kind === 'retry' ? s : { kind: 'retry', why: code === 1008 ? 'too slow, re-attaching' : 'connection lost' }))
          timer = setTimeout(connect, delay)
        },
      }
      conn.current = attachSession(worker, session, cols, rows, handlers, job)
    }
    connect()
    return () => {
      stopped = true
      clearTimeout(timer)
      conn.current?.close()
      conn.current = undefined
      t.dispose()
      term.current = undefined
    }
  }, [worker, session, job])

  // fit to the element when shown and when it resizes; tell the session the new size
  useEffect(() => {
    if (!visible) return
    const fit = () => {
      const t = term.current
      if (!t || !el.current?.clientWidth) return
      const before = t.size()
      const { cols, rows } = t.fit()
      if (cols !== before.cols || rows !== before.rows) conn.current?.resize(cols, rows)
    }
    fit()
    term.current?.focus()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(fit)
    ro.observe(el.current!)
    return () => ro.disconnect()
  }, [visible])

  return (
    <div className="term-wrap" data-testid="terminal" data-status={status.kind}>
      <div className="term" ref={el} />
      {status.kind === 'exit' && (
        <div className="term-overlay" data-testid="term-ended">
          session ended (code {status.code})
        </div>
      )}
      {status.kind === 'retry' && (
        <div className="term-overlay subtle" data-testid="term-retry">
          {status.why} - reconnecting…
        </div>
      )}
    </div>
  )
}
