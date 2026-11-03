import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'

/**
 * One tooltip for the whole UI, drawn by the page: every element's `title`
 * shows in it instead of the browser's (WebView2's native tooltip is plain,
 * slow and can't be styled). Mount a TooltipHost once per app; components
 * keep writing title="..." (newlines are line breaks).
 *
 * On hover or keyboard focus the host moves the element's title to
 * data-tip (so the native one never shows), waits a moment and shows it
 * below the element - above when there is no room - inside the window. It
 * hides on leave, scroll, a click or Escape. An element with no text and no
 * aria-label keeps the title as its accessible name (aria-label).
 */
const DELAY_MS = 350

function tipOf(el: Element): string {
  const t = el.getAttribute('title')
  if (t !== null) {
    el.removeAttribute('title')
    if (t) {
      el.setAttribute('data-tip', t)
      if (!el.getAttribute('aria-label') && !(el.textContent ?? '').trim()) el.setAttribute('aria-label', t)
    }
    return t
  }
  return el.getAttribute('data-tip') ?? ''
}

function target(n: EventTarget | null): Element | null {
  return n instanceof Element ? n.closest('[title], [data-tip]') : null
}

export function TooltipHost() {
  const [tip, setTip] = useState<{ text: string; rect: DOMRect }>()
  const [pos, setPos] = useState<{ left: number; top: number }>()
  const box = useRef<HTMLDivElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const cur = useRef<Element | null>(null)

  useEffect(() => {
    const hide = () => {
      clearTimeout(timer.current)
      cur.current = null
      setTip(undefined)
    }
    const show = (el: Element) => {
      const text = tipOf(el)
      clearTimeout(timer.current)
      cur.current = el
      if (!text) return setTip(undefined)
      timer.current = setTimeout(() => {
        if (cur.current === el && el.isConnected) setTip({ text: el.getAttribute('data-tip') ?? text, rect: el.getBoundingClientRect() })
      }, DELAY_MS)
    }
    const over = (e: Event) => {
      const el = target(e.target)
      if (el === cur.current) return
      if (el) show(el)
      else hide()
    }
    const out = (e: Event) => {
      const to = (e as PointerEvent).relatedTarget
      if (cur.current && !(to instanceof Node && cur.current.contains(to))) hide()
    }
    const key = (e: KeyboardEvent) => e.key === 'Escape' && hide()
    document.addEventListener('pointerover', over)
    document.addEventListener('pointerout', out)
    document.addEventListener('focusin', over)
    document.addEventListener('focusout', hide)
    document.addEventListener('pointerdown', hide, true)
    document.addEventListener('scroll', hide, true)
    document.addEventListener('keydown', key)
    return () => {
      clearTimeout(timer.current)
      document.removeEventListener('pointerover', over)
      document.removeEventListener('pointerout', out)
      document.removeEventListener('focusin', over)
      document.removeEventListener('focusout', hide)
      document.removeEventListener('pointerdown', hide, true)
      document.removeEventListener('scroll', hide, true)
      document.removeEventListener('keydown', key)
    }
  }, [])

  // place it once its size is known: below the element, else above; inside the window
  useEffect(() => {
    if (!tip || !box.current) return setPos(undefined)
    const b = box.current.getBoundingClientRect()
    const vw = document.documentElement.clientWidth
    const vh = document.documentElement.clientHeight
    const left = Math.min(Math.max(6, tip.rect.left + tip.rect.width / 2 - b.width / 2), vw - b.width - 6)
    const below = tip.rect.bottom + 6
    const top = below + b.height <= vh - 6 ? below : Math.max(6, tip.rect.top - 6 - b.height)
    setPos({ left, top })
  }, [tip])

  if (!tip) return null
  return createPortal(
    <div
      ref={box}
      className="ui-tip"
      role="tooltip"
      data-testid="ui-tip"
      style={pos ? { left: pos.left, top: pos.top } : { left: 0, top: 0, visibility: 'hidden' }}
    >
      {tip.text}
    </div>,
    document.body,
  )
}
