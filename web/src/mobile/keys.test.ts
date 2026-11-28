import { describe, expect, it } from 'vitest'
import { modify } from './keys'

describe('modify', () => {
  it('Ctrl makes control codes', () => {
    expect(modify('c', true, false)).toBe('\x03')
    expect(modify('C', true, false)).toBe('\x03')
    expect(modify('[', true, false)).toBe('\x1b')
    expect(modify(' ', true, false)).toBe('\x00')
  })
  it('Alt prefixes ESC', () => {
    expect(modify('b', false, true)).toBe('\x1bb')
    expect(modify('x', true, true)).toBe('\x1b\x18')
  })
  it('leaves text alone without modifiers, and multi-character text with Ctrl', () => {
    expect(modify('hi', false, false)).toBe('hi')
    expect(modify('hi', true, false)).toBe('hi')
  })
})
