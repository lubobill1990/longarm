import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { PoolWorker } from '../api/types'
import { WorkerCard } from '../components/WorkerCard'
import { useDismiss } from './dismiss'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

/** A trigger and its popover, the way the panel and the tray use the hook. */
function Pop({ onClose }: { onClose?: () => void }) {
  const [open, setOpen] = useState(false)
  const ref = useDismiss<HTMLSpanElement>(open, () => {
    onClose?.()
    setOpen(false)
  })
  return (
    <div>
      <span ref={ref}>
        <button aria-expanded={open} onClick={() => setOpen((v) => !v)}>
          menu
        </button>
        {open && (
          <span role="menu">
            <button role="menuitem">first</button>
            <button role="menuitem">second</button>
          </span>
        )}
      </span>
      {/* stops propagation, as some cards and rows do */}
      <div data-testid="outside" onMouseDown={(e) => e.stopPropagation()}>
        elsewhere
      </div>
      <input aria-label="other field" />
    </div>
  )
}

describe('useDismiss', () => {
  it('closes on a mouse down outside, even where propagation stops; not inside', () => {
    render(<Pop />)
    fireEvent.click(screen.getByText('menu'))
    fireEvent.mouseDown(screen.getByText('first'))
    expect(screen.queryByRole('menu')).not.toBeNull()
    fireEvent.mouseDown(screen.getByTestId('outside'))
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('closes on a touch outside', () => {
    render(<Pop />)
    fireEvent.click(screen.getByText('menu'))
    fireEvent.touchStart(document.body)
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('the trigger toggles it (its own mouse down does not close it first)', () => {
    render(<Pop />)
    const trigger = screen.getByText('menu')
    fireEvent.mouseDown(trigger)
    fireEvent.click(trigger)
    expect(screen.queryByRole('menu')).not.toBeNull()
    fireEvent.mouseDown(trigger)
    fireEvent.click(trigger)
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('Escape closes it wherever the focus is, and gives the focus back to the trigger', () => {
    render(<Pop />)
    fireEvent.click(screen.getByText('menu'))
    act(() => screen.getByText('second').focus())
    fireEvent.keyDown(screen.getByText('second'), { key: 'Escape' })
    expect(screen.queryByRole('menu')).toBeNull()
    expect(document.activeElement).toBe(screen.getByText('menu'))
    // the focus elsewhere: closed, the focus stays where it was
    fireEvent.click(screen.getByText('menu'))
    fireEvent.keyDown(document.body, { key: 'Escape' })
    expect(screen.queryByRole('menu')).toBeNull()
    // other keys don't
    fireEvent.click(screen.getByText('menu'))
    fireEvent.keyDown(document.body, { key: 'a' })
    expect(screen.queryByRole('menu')).not.toBeNull()
  })

  it('closes when the focus moves out of it', () => {
    render(<Pop />)
    fireEvent.click(screen.getByText('menu'))
    act(() => screen.getByText('first').focus())
    expect(screen.queryByRole('menu')).not.toBeNull()
    act(() => screen.getByLabelText('other field').focus())
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('listens only while open', () => {
    const onClose = vi.fn()
    const add = vi.spyOn(document, 'addEventListener')
    render(<Pop onClose={onClose} />)
    fireEvent.mouseDown(document.body)
    fireEvent.keyDown(document.body, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
    expect(add.mock.calls.some(([type]) => type === 'mousedown')).toBe(false)
    fireEvent.click(screen.getByText('menu'))
    fireEvent.mouseDown(document.body)
    expect(onClose).toHaveBeenCalledTimes(1)
    fireEvent.mouseDown(document.body)
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})

describe('the worker card ⋯ menu', () => {
  const worker: PoolWorker = {
    name: 'box',
    state: 'online',
    source: 'direct',
    node: { name: 'box', machine: 'box', companion: 'box-admin' },
  } as PoolWorker
  const admin: PoolWorker = { name: 'box-admin', state: 'online', source: 'direct', node: { name: 'box-admin', admin_worker: true, machine: 'box' } } as PoolWorker

  it('closes on a click outside and on Escape', () => {
    const m = { key: 'box', session: worker, admin, primary: worker, state: 'online', longOffline: false, labels: [] }
    render(
      <div>
        <WorkerCard m={m as never} actions={{ attach: () => {}, ssh: () => {} }} />
        <p>outside</p>
      </div>,
    )
    const more = screen.getByLabelText('more actions')
    fireEvent.click(more)
    expect(more.getAttribute('aria-expanded')).toBe('true')
    expect(screen.getByRole('menu')).toBeTruthy()
    fireEvent.mouseDown(screen.getByText('outside'))
    expect(screen.queryByRole('menu')).toBeNull()
    expect(more.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(more)
    fireEvent.keyDown(document.body, { key: 'Escape' })
    expect(screen.queryByRole('menu')).toBeNull()
  })
})
