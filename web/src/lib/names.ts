// A node's name, as the hub checks it (internal/pki ValidName and
// internal/nodeid CheckName): a rename's input says what is wrong before
// the request goes out.

const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/
const ID_LIKE = /^[0-9a-fA-F-]{4,}$/

/** Why name can't be a node's name ('' = it can). */
export function nodeNameError(name: string): string {
  if (!NAME.test(name)) return 'Letters, digits, . _ - only, at most 63, starting with a letter or digit'
  const lower = name.toLowerCase()
  if (lower.startsWith('id:') || lower.startsWith('name:')) return '"id:" and "name:" are reserved'
  if (ID_LIKE.test(name)) return `Only hex digits and dashes looks like a node ID: add a letter (e.g. w-${name})`
  return ''
}

/** A hub error without gRPC's "rpc error: code = X desc = " prefix. */
export function hubErrorText(e: unknown): string {
  const s = e instanceof Error ? e.message : String(e)
  return s.replace(/^rpc error: code = \w+ desc = /, '')
}
