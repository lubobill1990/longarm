// Where feedback and bug reports go: the hub's [web] feedback_url (its
// /auth/info, /api/me, /healthz), else - an older hub, no hub yet - the
// project's issues.
export const PROJECT_ISSUES = 'https://github.com/lubobill1990/longarm/issues'

export function feedbackURL(fromHub?: string): string {
  return fromHub && /^https?:\/\//i.test(fromHub) ? fromHub : PROJECT_ISSUES
}
