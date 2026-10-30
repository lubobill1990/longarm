import { describe, expect, it, vi } from 'vitest'
import { clipboardKey, type ClipboardTerm } from './terminal'

function setup(selection = '') {
  const term: ClipboardTerm & { pasted: string[] } = {
    pasted: [],
    hasSelection: () => selection !== '',
    getSelection: () => selection,
    clearSelection: () => {
      selection = ''
    },
    paste(t) {
      this.pasted.push(t)
    },
  }
  const clip = { writeText: vi.fn(async () => {}), readText: vi.fn(async () => 'pasted text') } as unknown as Clipboard
  return { term, clip }
}

const key = (k: string, mods: { ctrl?: boolean; shift?: boolean } = {}) =>
  new KeyboardEvent('keydown', { key: k, ctrlKey: mods.ctrl ?? true, shiftKey: mods.shift ?? false })

describe('terminal clipboard keys', () => {
  it('Ctrl+C copies a selection and is not sent; without a selection it reaches the program', () => {
    const { term, clip } = setup('some output')
    expect(clipboardKey(term, key('c'), clip)).toBe(false)
    expect(clip.writeText).toHaveBeenCalledWith('some output')
    expect(term.hasSelection()).toBe(false)
    expect(clipboardKey(term, key('c'), clip)).toBe(true) // now an interrupt
  })

  it('Ctrl+Shift+C never interrupts, even without a selection', () => {
    const { term, clip } = setup()
    expect(clipboardKey(term, key('C', { shift: true }), clip)).toBe(false)
    expect(clip.writeText).not.toHaveBeenCalled()
  })

  it('Ctrl+V and Ctrl+Shift+V paste through xterm', async () => {
    const { term, clip } = setup()
    expect(clipboardKey(term, key('v'), clip)).toBe(false)
    expect(clipboardKey(term, key('V', { shift: true }), clip)).toBe(false)
    await vi.waitFor(() => expect(term.pasted).toEqual(['pasted text', 'pasted text']))
  })

  it('other keys, key-ups and no clipboard pass through', () => {
    const { term, clip } = setup('x')
    expect(clipboardKey(term, key('d'), clip)).toBe(true)
    expect(clipboardKey(term, key('c', { ctrl: false }), clip)).toBe(true)
    expect(clipboardKey(term, new KeyboardEvent('keyup', { key: 'c', ctrlKey: true }), clip)).toBe(true)
    expect(clipboardKey(term, key('c'), undefined)).toBe(true)
  })
})
