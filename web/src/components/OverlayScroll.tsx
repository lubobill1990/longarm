import { useEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react'

/**
 * A scroll area whose scrollbar takes no room: the native one is hidden and
 * a thin thumb of ours is drawn over the right edge - shown on hover and
 * while scrolling, draggable. The content keeps the same margin on both
 * sides whether it scrolls or not (a reserved gutter left the thumb beside
 * the cards on one side and empty room on the other).
 */
export function OverlayScroll({ children, className, ...rest }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const [thumb, setThumb] = useState({ top: 0, height: 0 })
  const [active, setActive] = useState(false)
  const idle = useRef<ReturnType<typeof setTimeout>>(undefined)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const measure = () => {
      const { scrollTop, scrollHeight, clientHeight } = el
      if (scrollHeight <= clientHeight + 1) {
        setThumb({ top: 0, height: 0 })
        return
      }
      const height = Math.max(28, (clientHeight * clientHeight) / scrollHeight)
      const top = (scrollTop / (scrollHeight - clientHeight)) * (clientHeight - height)
      setThumb({ top, height })
    }
    const onScroll = () => {
      measure()
      setActive(true)
      clearTimeout(idle.current)
      idle.current = setTimeout(() => setActive(false), 900)
    }
    measure()
    el.addEventListener('scroll', onScroll, { passive: true })
    // the content grows and shrinks (polling cards, tabs): measure again
    const ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(measure) : undefined
    ro?.observe(el)
    for (const c of Array.from(el.children)) ro?.observe(c)
    const mo = typeof MutationObserver !== 'undefined' ? new MutationObserver(measure) : undefined
    mo?.observe(el, { childList: true, subtree: true })
    return () => {
      el.removeEventListener('scroll', onScroll)
      ro?.disconnect()
      mo?.disconnect()
      clearTimeout(idle.current)
    }
  }, [])

  const drag = (e: React.PointerEvent<HTMLDivElement>) => {
    const el = ref.current
    if (!el) return
    e.preventDefault()
    const startY = e.clientY
    const startTop = el.scrollTop
    const k = (el.scrollHeight - el.clientHeight) / Math.max(1, el.clientHeight - thumb.height)
    const move = (ev: PointerEvent) => {
      el.scrollTop = startTop + (ev.clientY - startY) * k
    }
    const up = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
  }

  return (
    <div className={`oscroll ${active ? 'active' : ''}`}>
      <div ref={ref} className={`oscroll-area ${className ?? ''}`} {...rest}>
        {children}
      </div>
      {thumb.height > 0 && (
        <div className="oscroll-thumb" style={{ top: thumb.top, height: thumb.height }} onPointerDown={drag} aria-hidden="true" />
      )}
    </div>
  )
}
