import { useEffect, useRef, type RefObject } from 'react'

/**
 * Closes an open popover (a menu, a dropdown, the version details) the same
 * way everywhere, in the panel and in the tray:
 *
 *  - a mouse down or touch outside the element the returned ref is on
 *    (put it on the wrapper of the trigger and the popover: a click on the
 *    trigger toggles instead);
 *  - Escape, wherever the focus is; when it was inside, it goes back to the
 *    trigger (the element with aria-expanded);
 *  - the focus moving out of it (Tab past the last item).
 *
 * The listeners are on the document in the capture phase, so a component
 * that stops the event's propagation can't keep a popover open.
 */
export function useDismiss<T extends HTMLElement = HTMLElement>(open: boolean, onClose: () => void): RefObject<T | null> {
  const ref = useRef<T>(null)
  const close = useRef(onClose)
  useEffect(() => {
    close.current = onClose
  })
  useEffect(() => {
    if (!open) return
    const inside = (t: EventTarget | null) => t instanceof Node && !!ref.current?.contains(t)
    const onDown = (e: Event) => {
      if (!inside(e.target)) close.current()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      const hadFocus = inside(document.activeElement)
      close.current()
      if (hadFocus) ref.current?.querySelector<HTMLElement>('[aria-expanded]')?.focus()
    }
    const onFocus = (e: FocusEvent) => {
      if (!inside(e.target)) close.current()
    }
    document.addEventListener('mousedown', onDown, true)
    document.addEventListener('touchstart', onDown, true)
    document.addEventListener('keydown', onKey, true)
    document.addEventListener('focusin', onFocus, true)
    return () => {
      document.removeEventListener('mousedown', onDown, true)
      document.removeEventListener('touchstart', onDown, true)
      document.removeEventListener('keydown', onKey, true)
      document.removeEventListener('focusin', onFocus, true)
    }
  }, [open])
  return ref
}
