import { useEffect, useState, type MouseEvent, type ReactNode } from 'react'

// A tiny history-API router (the panel has a handful of pages).

const listeners = new Set<() => void>()

export function navigate(to: string) {
  if (to === location.pathname + location.search) return
  history.pushState(null, '', to)
  listeners.forEach((l) => l())
}

export function usePath(): string {
  const [path, setPath] = useState(location.pathname)
  useEffect(() => {
    const on = () => setPath(location.pathname)
    listeners.add(on)
    window.addEventListener('popstate', on)
    return () => {
      listeners.delete(on)
      window.removeEventListener('popstate', on)
    }
  }, [])
  return path
}

export function Link({ to, children, className }: { to: string; children: ReactNode; className?: string }) {
  return (
    <a
      href={to}
      className={className}
      onClick={(e: MouseEvent) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
        e.preventDefault()
        navigate(to)
      }}
    >
      {children}
    </a>
  )
}
