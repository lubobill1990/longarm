// xterm.js behind a small interface: TerminalView uses only this module,
// so the tests replace it (jsdom has no canvas).
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

export interface Term {
  write(data: Uint8Array | string): void
  /** clears the screen and the scrollback (before a replay) */
  reset(): void
  /** keyboard input as bytes (typed text UTF-8 encoded; mouse reports as is) */
  onInput(fn: (b: Uint8Array<ArrayBuffer>) => void): void
  /** fits the terminal to its element: the new size */
  fit(): { cols: number; rows: number }
  size(): { cols: number; rows: number }
  focus(): void
  dispose(): void
}

/** A token of tokens.css as xterm wants it (a colour or a font list). */
function token(el: HTMLElement, name: string, fallback: string): string {
  return getComputedStyle(el).getPropertyValue(name).trim() || fallback
}

export function createTerminal(el: HTMLElement): Term {
  const term = new Terminal({
    fontFamily: token(el, '--mono', 'monospace'),
    fontSize: 13,
    scrollback: 5000,
    cursorBlink: true,
    theme: {
      background: token(el, '--log-bg', '#151413'),
      foreground: token(el, '--log-text', '#d9d5ce'),
      cursor: token(el, '--accent', '#4fc2b3'),
      selectionBackground: token(el, '--accent-soft', 'rgba(79, 194, 179, 0.3)'),
    },
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el)
  const utf8 = new TextEncoder()
  return {
    write: (d) => term.write(d),
    reset: () => term.reset(),
    onInput: (fn) => {
      term.onData((s) => fn(utf8.encode(s)))
      // onBinary: one byte per char (mouse reports in X10 mode)
      term.onBinary((s) => fn(Uint8Array.from(s, (c) => c.charCodeAt(0) & 0xff)))
    },
    fit: () => {
      fit.fit()
      return { cols: term.cols, rows: term.rows }
    },
    size: () => ({ cols: term.cols, rows: term.rows }),
    focus: () => term.focus(),
    dispose: () => term.dispose(),
  }
}
