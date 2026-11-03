import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { blocks, cells, Markdown } from './Markdown'

afterEach(cleanup)

describe('Markdown tables', () => {
  const md = ['Mostly, yes:', '', '| Row | Also shown in |', '|---|:---:|', '| **hub** | Account `a|b` |', '| identity \\| role | nowhere |', '', 'After.'].join('\n')

  it('a header, a separator and rows make a table; text around it stays', () => {
    const b = blocks(md)
    expect(b.map((x) => x.kind)).toEqual(['para', 'table', 'para'])
    const t = b[1] as Extract<ReturnType<typeof blocks>[number], { kind: 'table' }>
    expect(t.head).toEqual(['Row', 'Also shown in'])
    expect(t.align).toEqual([undefined, 'center'])
    // a "|" in code or escaped is not a column
    expect(t.rows).toEqual([
      ['**hub**', 'Account `a|b`'],
      ['identity | role', 'nowhere'],
    ])
  })

  it('splits cells with or without the outer pipes', () => {
    expect(cells('a | b | c')).toEqual(['a', 'b', 'c'])
    expect(cells('| a | b |')).toEqual(['a', 'b'])
  })

  it('renders a table with inline markup in its cells', () => {
    render(<Markdown text={md} />)
    const table = screen.getByRole('table')
    expect(table.querySelectorAll('th')).toHaveLength(2)
    expect(table.querySelectorAll('tbody tr')).toHaveLength(2)
    expect(table.querySelector('tbody td strong')?.textContent).toBe('hub')
    expect((table.querySelectorAll('th')[1] as HTMLElement).style.textAlign).toBe('center')
  })

  it('a line with a pipe alone is not a table', () => {
    expect(blocks('a | b\nno separator').map((x) => x.kind)).toEqual(['para'])
  })
})
