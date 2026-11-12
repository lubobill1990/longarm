// The hub's links: the source repository it points at ([web] repo_url,
// default the repository the hub was built from) and where it collects
// feedback and bug reports ([web] feedback_url; default that repository's
// issues). The hub sends them in /auth/info, /api/me and /healthz; the tray
// falls back to the repository it was built from. '' = none: no link.

const isWeb = (u?: string): u is string => !!u && /^https?:\/\//i.test(u)

export function repoURL(fromHub?: string): string {
  return isWeb(fromHub) ? fromHub.replace(/\/+$/, '') : ''
}

export function feedbackURL(fromHub?: string, repoFromHub?: string): string {
  if (isWeb(fromHub)) return fromHub
  const repo = repoURL(repoFromHub)
  return repo ? `${repo}/issues` : ''
}

/** A page of the repository's docs (docs/usage.md and friends; '' without a repository). */
export const docsURL = (repo: string, page = 'usage.md', anchor = '') => (repo ? `${repo}/blob/main/docs/${page}${anchor ? `#${anchor}` : ''}` : '')

/** The promo film: VITE_FILM_URL (pnpm dev: a local copy), else the repository's release asset ('' = none). */
export const filmURL = (repo: string) =>
  (import.meta.env.VITE_FILM_URL as string | undefined) || (repo ? `${repo}/releases/download/promo-film/longarm-promo.mp4` : '')
