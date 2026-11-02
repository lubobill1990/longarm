// The hub's links: the source repository it points at ([web] repo_url, e.g.
// a company's internal copy) and where it collects feedback and bug reports
// ([web] feedback_url; default that repository's issues). The hub sends them
// in /auth/info, /api/me and /healthz; without a hub, or with one from
// before them, the public repository and its issues.
export const PUBLIC_REPO = 'https://github.com/lubobill1990/longarm'
export const PROJECT_ISSUES = `${PUBLIC_REPO}/issues`

const isWeb = (u?: string): u is string => !!u && /^https?:\/\//i.test(u)

export function repoURL(fromHub?: string): string {
  return isWeb(fromHub) ? fromHub.replace(/\/+$/, '') : PUBLIC_REPO
}

export function feedbackURL(fromHub?: string, repoFromHub?: string): string {
  return isWeb(fromHub) ? fromHub : `${repoURL(repoFromHub)}/issues`
}

/** A page of the repository's docs (docs/usage.md and friends). */
export const docsURL = (repo: string, page = 'usage.md', anchor = '') => `${repo}/blob/main/docs/${page}${anchor ? `#${anchor}` : ''}`
