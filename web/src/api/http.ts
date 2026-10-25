// Small fetch / SSE helpers shared by the hub and tray APIs.

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined || method !== 'GET') {
    // state-changing calls are JSON (the hub's CSRF rule)
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
    init.body = body === undefined ? '{}' : JSON.stringify(body)
  }
  const resp = await fetch(url, init)
  const text = await resp.text()
  let data: unknown = undefined
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!resp.ok) {
    const msg =
      data && typeof data === 'object' && 'error' in (data as Record<string, unknown>)
        ? String((data as Record<string, unknown>).error)
        : `${resp.status} ${resp.statusText}`
    throw new ApiError(resp.status, msg)
  }
  // the local gateways report errors as {"error": ...} with 400
  if (data && typeof data === 'object' && 'error' in (data as Record<string, unknown>) && Object.keys(data as object).length === 1) {
    throw new ApiError(400, String((data as Record<string, unknown>).error))
  }
  return data as T
}

export const get = <T>(url: string) => request<T>('GET', url)
export const post = <T>(url: string, body?: unknown) => request<T>('POST', url, body)
export const put = <T>(url: string, body?: unknown) => request<T>('PUT', url, body)
export const del = <T>(url: string) => request<T>('DELETE', url)

export interface Stream {
  close(): void
}

/**
 * Subscribes to server-sent events; handlers by event name. "end" and
 * "error" events (sent by the APIs) close the stream; onDone gets the
 * error message, if any. Reconnects (EventSource's default) otherwise.
 */
export function sse(
  url: string,
  handlers: Record<string, (data: unknown) => void>,
  onDone?: (err?: string) => void,
  opts: { reconnect?: boolean } = {},
): Stream {
  const es = new EventSource(url, { withCredentials: true })
  let closed = false
  const close = (err?: string) => {
    if (closed) return
    closed = true
    es.close()
    onDone?.(err)
  }
  for (const [name, fn] of Object.entries(handlers)) {
    es.addEventListener(name, (ev) => {
      try {
        fn(JSON.parse((ev as MessageEvent).data))
      } catch {
        /* ignore malformed events */
      }
    })
  }
  es.addEventListener('end', () => close())
  es.addEventListener('error', (ev) => {
    const data = (ev as MessageEvent).data
    if (typeof data === 'string' && data) {
      // an "error" event sent by the server
      try {
        close(JSON.parse(data).error ?? 'error')
      } catch {
        close('error')
      }
      return
    }
    // transport error: EventSource retries unless told not to
    if (opts.reconnect === false) close('connection lost')
  })
  return { close: () => close() }
}
