import { useRef, useState } from 'react'
import { KEYS, modify } from './keys'

// The terminal's keyboard on a phone: a row of keys a soft keyboard lacks
// (Esc, Tab, Ctrl and Alt for the next key, arrows ...) and an input the
// soft keyboard types into. The input sends as it goes - each character,
// Backspace, Enter - and keeps itself empty; an IME's composition (Chinese,
// Japanese ...) goes when it is committed.

export function KeyBar({ send }: { send: (s: string) => void }) {
  const [ctrl, setCtrl] = useState(false)
  const [alt, setAlt] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const composing = useRef(false)
  const typed = (text: string) => {
    send(modify(text, ctrl, alt))
    setCtrl(false)
    setAlt(false)
  }
  const key = (k: string) => () => {
    send((alt ? '\x1b' : '') + KEYS[k])
    setAlt(false)
    setCtrl(false)
    input.current?.focus()
  }
  const btn = (label: string, onClick: () => void, on?: boolean) => (
    <button
      key={label}
      className={`m-key${on ? ' on' : ''}`}
      // keep the soft keyboard up: the input keeps the focus
      onPointerDown={(e) => e.preventDefault()}
      onClick={onClick}
    >
      {label}
    </button>
  )
  return (
    <div className="m-keybar">
      <div className="m-keys">
        {btn('Esc', key('esc'))}
        {btn('Tab', key('tab'))}
        {btn('Ctrl', () => setCtrl((v) => !v), ctrl)}
        {btn('Alt', () => setAlt((v) => !v), alt)}
        {btn('←', key('left'))}
        {btn('↓', key('down'))}
        {btn('↑', key('up'))}
        {btn('→', key('right'))}
        {btn('|', () => typed('|'))}
        {btn('~', () => typed('~'))}
        {btn('/', () => typed('/'))}
        {btn('-', () => typed('-'))}
        {btn('Home', key('home'))}
        {btn('End', key('end'))}
        {btn('PgUp', key('pgup'))}
        {btn('PgDn', key('pgdn'))}
      </div>
      <input
        ref={input}
        className="m-type"
        placeholder={ctrl ? 'Ctrl + key…' : alt ? 'Alt + key…' : 'Type here'}
        autoCapitalize="off"
        autoCorrect="off"
        autoComplete="off"
        spellCheck={false}
        enterKeyHint="send"
        onBeforeInput={(e) => {
          const ev = e.nativeEvent as InputEvent
          if (composing.current || ev.isComposing) return
          switch (ev.inputType) {
            case 'insertText':
            case 'insertReplacementText':
              if (ev.data) typed(ev.data)
              break
            case 'insertLineBreak':
            case 'insertParagraph':
              send('\r')
              break
            case 'deleteContentBackward':
              send('\x7f')
              break
            case 'insertFromPaste':
              if (ev.data) send(ev.data)
              break
            default:
              return
          }
          e.preventDefault()
        }}
        onKeyDown={(e) => {
          // a hardware keyboard (iPad, Bluetooth): Enter and Backspace on an empty input
          if (composing.current) return
          if (e.key === 'Enter') {
            send('\r')
            e.preventDefault()
          } else if (e.key === 'Backspace' && !e.currentTarget.value) {
            send('\x7f')
            e.preventDefault()
          } else if (e.key.length > 1 && KEYS[e.key.replace('Arrow', '').toLowerCase()]) {
            send(KEYS[e.key.replace('Arrow', '').toLowerCase()])
            e.preventDefault()
          }
        }}
        onPaste={(e) => {
          const t = e.clipboardData.getData('text')
          if (t) send(t)
          e.preventDefault()
        }}
        onCompositionStart={() => (composing.current = true)}
        onCompositionEnd={(e) => {
          composing.current = false
          if (e.data) typed(e.data)
          e.currentTarget.value = ''
        }}
        onChange={(e) => {
          // what beforeinput didn't take (old WebViews): send it, empty the field
          if (composing.current) return
          const v = e.target.value
          if (v) typed(v)
          e.target.value = ''
        }}
      />
    </div>
  )
}
