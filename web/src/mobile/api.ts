// The phone core's API (internal/mobile): /mapi/... plus the daemon's
// session routes (api/sessions.ts) at the same paths. The page got the
// token as a cookie: plain same-origin requests.
import { get, post } from '../api/http'
import type { LongarmSession, StartSession } from '../api/sessions'
import type { NodeInfo } from '../api/types'

export interface MobileStatus {
  version?: string
  signedIn?: boolean
  user?: string
  hub?: string
  error?: string
}

export interface SignInState {
  step?: string
  message?: string
  url?: string
  code?: string
  done?: boolean
  error?: string
}

const enc = encodeURIComponent
const sessions = (w: string) => `/local/longarm/v1/workers/${enc(w)}/sessions`

export const mobileApi = {
  status: () => get<MobileStatus>('/mapi/status'),
  signIn: (hub: string, name: string) => post<SignInState>('/mapi/signin', { hub, name }),
  signInState: () => get<SignInState>('/mapi/signin'),
  signOut: () => post('/mapi/signout'),
  machines: () => get<{ nodes?: NodeInfo[] }>('/mapi/machines').then((r) => r.nodes ?? []),
  sessions: (w: string) => get<{ sessions?: LongarmSession[] }>(sessions(w)).then((r) => r.sessions ?? []),
  start: (w: string, req: StartSession) => post<LongarmSession>(sessions(w), req),
  stop: (w: string, name: string) => post<LongarmSession>(`${sessions(w)}/${enc(name)}/stop`, {}),
}

/** The native side's actions (the LongarmCore plugin catches these URLs); "" when the app has none. */
export function nativeURL(action: string, worker: string): string {
  return `longarm-native://${action}?worker=${enc(worker)}`
}

/** Whether the app has the native desktop viewer (it says so in the page's URL: ?desktop=1). */
export function hasNativeDesktop(): boolean {
  const q = new URLSearchParams(location.search)
  if (q.get('desktop') === '1') sessionStorage.setItem('longarm.desktop', '1')
  return sessionStorage.getItem('longarm.desktop') === '1'
}
