import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ask, ConfirmHost } from './confirm'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('ask', () => {
  it('without a host: window.confirm (a browser, tests)', async () => {
    const native = vi.spyOn(window, 'confirm').mockReturnValue(true)
    expect(await ask('Kill it?')).toBe(true)
    expect(native).toHaveBeenCalledWith('Kill it?')
  })

  it('with a host: our dialog, never the webview native one', async () => {
    const native = vi.spyOn(window, 'confirm')
    render(<ConfirmHost />)
    const yes = ask('Update w1 to 0.1.31?\n\nIt restarts.', { ok: 'Update' })
    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toContain('Update w1 to 0.1.31?')
    fireEvent.click(screen.getByText('Update'))
    expect(await yes).toBe(true)
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(native).not.toHaveBeenCalled()

    // Cancel, Esc and a click beside the dialog all say no
    const no1 = ask('Remove?', { ok: 'Remove', danger: true })
    expect((await screen.findByText('Remove')).className).toBe('danger-solid')
    fireEvent.click(screen.getByText('Cancel'))
    expect(await no1).toBe(false)
    const no2 = ask('Remove?')
    await screen.findByRole('alertdialog')
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(await no2).toBe(false)
    const no3 = ask('Remove?')
    fireEvent.mouseDown(await screen.findByTestId('ask'))
    expect(await no3).toBe(false)
  })
})
