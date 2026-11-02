import { describe, expect, it } from 'vitest'
import { docsURL, feedbackURL, PROJECT_ISSUES, PUBLIC_REPO, repoURL } from './feedback'

describe('hub links', () => {
  it('uses the hub repository and channel, else the public repository and its issues', () => {
    // a company hub: its copy of the repository, its feedback channel
    expect(repoURL('https://github.com/gim-home/longarm/')).toBe('https://github.com/gim-home/longarm')
    expect(feedbackURL('https://aka.ms/longarmteams', 'https://github.com/gim-home/longarm')).toBe('https://aka.ms/longarmteams')
    // a repository without a feedback link: that repository's issues
    expect(feedbackURL(undefined, 'https://github.com/gim-home/longarm')).toBe('https://github.com/gim-home/longarm/issues')
    // no hub, an older hub, or something that isn't a web link
    expect(repoURL()).toBe(PUBLIC_REPO)
    expect(repoURL('javascript:alert(1)')).toBe(PUBLIC_REPO)
    expect(feedbackURL()).toBe(PROJECT_ISSUES)
    expect(feedbackURL('file:///etc/passwd')).toBe(PROJECT_ISSUES)
    expect(docsURL('https://github.com/gim-home/longarm', 'usage.md', 'install')).toBe('https://github.com/gim-home/longarm/blob/main/docs/usage.md#install')
  })
})
