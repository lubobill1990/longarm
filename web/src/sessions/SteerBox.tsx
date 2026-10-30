import { useState, type KeyboardEvent } from 'react'
import { ApiError } from '../api/http'
import { sessionsApi, type SendInput } from '../api/sessions'

/** Input is refused while someone typed in the terminal this recently (409). */
export const IF_IDLE_MS = 1500

/**
 * Steers a Longarm session without taking it over (Sessions.Send): Enter
 * sends the text and Enter, Shift+Enter is a new line; Esc, "y" and Ctrl-C
 * (confirmed) as buttons. Refused while someone is typing in the
 * terminal: the text stays for a retry.
 */
export function SteerBox({ worker, session, disabled }: { worker: string; session: string; disabled?: boolean }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<{ text: string; error?: boolean }>()

  const send = async (input: Omit<SendInput, 'if_idle_ms'>, clear = false) => {
    setBusy(true)
    setNote(undefined)
    try {
      await sessionsApi.send(worker, session, { ...input, if_idle_ms: IF_IDLE_MS })
      if (clear) setText('')
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setNote({ text: 'someone is typing - retry' })
      else setNote({ text: e instanceof Error ? e.message : String(e), error: true })
    } finally {
      setBusy(false)
    }
  }

  // raw: the text's line breaks stay line feeds (a new line in the agent's
  // prompt, not Enter); the Enter key submits it
  const submit = () => send({ text, keys: ['enter'], raw: true }, true)

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
    e.preventDefault()
    if (!busy) submit()
  }

  const off = disabled || busy
  return (
    <div className="steer" data-testid="steer">
      <textarea
        aria-label="message to the session"
        placeholder="Type to the agent - Enter sends, Shift+Enter: new line"
        rows={2}
        value={text}
        disabled={disabled}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKey}
      />
      <div className="steer-actions">
        <button className="sm primary" disabled={off} onClick={submit}>
          Send
        </button>
        <button className="sm" disabled={off} title="the Esc key (interrupts the agent)" onClick={() => send({ keys: ['esc'] })}>
          Esc
        </button>
        <button className="sm" disabled={off} title='types "y"' onClick={() => send({ text: 'y', raw: true })}>
          y
        </button>
        <button
          className="sm danger"
          disabled={off}
          title="Ctrl-C (stops the running command; twice quits the agent)"
          onClick={() => confirm(`Send Ctrl-C to ${session}?`) && send({ keys: ['ctrl-c'] })}
        >
          Ctrl-C
        </button>
        {note && (
          <span className={`small ${note.error ? 'red' : 'amber'}`} data-testid="steer-note" role="status">
            {note.text}
          </span>
        )}
      </div>
    </div>
  )
}
