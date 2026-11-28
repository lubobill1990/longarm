// Keys for a terminal from a phone: what the key bar's buttons send, and
// Ctrl / Alt applied to the next character typed.

export const KEYS: Record<string, string> = {
  esc: '\x1b',
  tab: '\t',
  up: '\x1b[A',
  down: '\x1b[B',
  right: '\x1b[C',
  left: '\x1b[D',
  home: '\x1b[H',
  end: '\x1b[F',
  pgup: '\x1b[5~',
  pgdn: '\x1b[6~',
}

/** text with Ctrl / Alt held: Ctrl+letter is its control code (Ctrl+C = 0x03), Alt prefixes ESC. */
export function modify(text: string, ctrl: boolean, alt: boolean): string {
  if (!text) return text
  let s = text
  if (ctrl && s.length === 1) {
    const c = s.toUpperCase().charCodeAt(0)
    if (c >= 64 && c <= 95) s = String.fromCharCode(c - 64) // @ A-Z [ \ ] ^ _
    else if (s === ' ') s = '\x00'
    else if (s === '?') s = '\x7f'
  }
  if (alt) s = '\x1b' + s
  return s
}
