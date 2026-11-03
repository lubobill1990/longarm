import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TooltipHost } from './Tooltip'

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('TooltipHost', () => {
  it("shows a title in our tooltip, never the browser's; hides on leave", () => {
    vi.useFakeTimers()
    render(
      <>
        <button title={'6 of 6 workers online\nhub connected'}>6/6 online</button>
        <button title="close" />
        <TooltipHost />
      </>,
    )
    const btn = screen.getByText('6/6 online')
    fireEvent.pointerOver(btn)
    // the native tooltip is gone at once; ours after a moment
    expect(btn.getAttribute('title')).toBeNull()
    expect(btn.getAttribute('data-tip')).toBe('6 of 6 workers online\nhub connected')
    expect(screen.queryByTestId('ui-tip')).toBeNull()
    act(() => vi.advanceTimersByTime(400))
    expect(screen.getByTestId('ui-tip').textContent).toBe('6 of 6 workers online\nhub connected')
    fireEvent.pointerOut(btn, { relatedTarget: document.body })
    expect(screen.queryByTestId('ui-tip')).toBeNull()

    // an icon button keeps its name for screen readers
    const close = screen.getByRole('button', { name: 'close' })
    fireEvent.pointerOver(close)
    expect(close.getAttribute('aria-label')).toBe('close')
    act(() => vi.advanceTimersByTime(400))
    expect(screen.getByTestId('ui-tip').textContent).toBe('close')
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByTestId('ui-tip')).toBeNull()
  })
})
