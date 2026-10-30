import { Fragment, type ReactNode } from 'react'
import { trayApi } from '../api/local'

// A small Markdown renderer for the conversation view: what agents write -
// paragraphs, headings, lists, quotes, fenced code, inline code, bold,
// italics and links. It builds React elements (never HTML strings), so a
// transcript can't inject markup; links open in the default browser
// through the tray (the window itself never navigates away).

type Block =
  | { kind: 'code'; lang: string; text: string }
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'list'; ordered: boolean; items: string[] }
  | { kind: 'quote'; text: string }
  | { kind: 'para'; text: string }

const fenceRE = /^\s*(```|~~~)\s*([\w+#.-]*)\s*$/
const headingRE = /^(#{1,6})\s+(.*)$/
const bulletRE = /^\s*[-*+]\s+(.*)$/
const orderedRE = /^\s*\d+[.)]\s+(.*)$/
const quoteRE = /^\s*>\s?(.*)$/

/** Splits text into blocks (exported for tests). */
export function blocks(text: string): Block[] {
  const lines = text.replace(/\r\n?/g, '\n').split('\n')
  const out: Block[] = []
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push({ kind: 'para', text: para.join('\n') })
    para = []
  }
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i]
    const fence = fenceRE.exec(l)
    if (fence) {
      flush()
      const body: string[] = []
      for (i++; i < lines.length && !lines[i].trim().startsWith(fence[1]); i++) body.push(lines[i])
      out.push({ kind: 'code', lang: fence[2], text: body.join('\n') })
      continue
    }
    const h = headingRE.exec(l)
    if (h) {
      flush()
      out.push({ kind: 'heading', level: h[1].length, text: h[2] })
      continue
    }
    const bullet = bulletRE.exec(l)
    const ordered = orderedRE.exec(l)
    if (bullet || ordered) {
      flush()
      const isOrdered = !bullet
      const items: string[] = []
      for (; i < lines.length; i++) {
        const m = (isOrdered ? orderedRE : bulletRE).exec(lines[i])
        if (m) items.push(m[1])
        else if (items.length && /^\s{2,}\S/.test(lines[i])) items[items.length - 1] += '\n' + lines[i].trim()
        else break
      }
      i--
      out.push({ kind: 'list', ordered: isOrdered, items })
      continue
    }
    const q = quoteRE.exec(l)
    if (q) {
      flush()
      const body = [q[1]]
      for (i++; i < lines.length && quoteRE.test(lines[i]); i++) body.push(quoteRE.exec(lines[i])![1])
      i--
      out.push({ kind: 'quote', text: body.join('\n') })
      continue
    }
    if (l.trim() === '') flush()
    else para.push(l)
  }
  flush()
  return out
}

// `code`, **bold** / __bold__, *italic* / _italic_, [text](url)
const inlineRE = /(`+)([^`]+?)\1|\*\*([^*]+?)\*\*|__([^_]+?)__|\*([^*\s][^*]*?)\*|(?<![\w])_([^_\s][^_]*?)_(?![\w])|\[([^\]]+)\]\(([^)\s]+)\)/g

/** Renders the inline markup of text (exported for tests). */
export function inline(text: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0
  let k = 0
  for (const m of text.matchAll(inlineRE)) {
    if (m.index > last) out.push(text.slice(last, m.index))
    if (m[2] !== undefined) out.push(<code key={k++}>{m[2]}</code>)
    else if (m[3] !== undefined || m[4] !== undefined) out.push(<strong key={k++}>{inline(m[3] ?? m[4])}</strong>)
    else if (m[5] !== undefined || m[6] !== undefined) out.push(<em key={k++}>{inline(m[5] ?? m[6])}</em>)
    else out.push(<Link key={k++} href={m[8]} text={m[7]} />)
    last = m.index + m[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

function Link({ href, text }: { href: string; text: string }) {
  if (!/^https?:\/\//i.test(href)) return <>{text}</>
  return (
    <a
      href={href}
      title={href}
      onClick={(e) => {
        e.preventDefault()
        void trayApi.open(href)
      }}
    >
      {text}
    </a>
  )
}

/** Lines of a paragraph keep their breaks (agents format with them). */
function lines(text: string): ReactNode {
  return text.split('\n').map((l, i) => (
    <Fragment key={i}>
      {i > 0 && <br />}
      {inline(l)}
    </Fragment>
  ))
}

export function Markdown({ text }: { text: string }) {
  return (
    <div className="md">
      {blocks(text).map((b, i) => {
        switch (b.kind) {
          case 'code':
            return (
              <pre key={i} className="md-code mono" data-lang={b.lang || undefined}>
                {b.text}
              </pre>
            )
          case 'heading': {
            const H = `h${Math.min(b.level + 2, 6)}` as 'h3'
            return <H key={i}>{inline(b.text)}</H>
          }
          case 'list': {
            const L = b.ordered ? 'ol' : 'ul'
            return (
              <L key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{lines(it)}</li>
                ))}
              </L>
            )
          }
          case 'quote':
            return <blockquote key={i}>{lines(b.text)}</blockquote>
          default:
            return <p key={i}>{lines(b.text)}</p>
        }
      })}
    </div>
  )
}
