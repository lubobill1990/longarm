import { describe, expect, it } from 'vitest'
import { docsURL, feedbackURL, filmURL, repoURL } from './feedback'

describe('hub links', () => {
  it('uses the hub repository and channel; none without them', () => {
    // a hub naming its repository and feedback channel
    expect(repoURL('https://git.example.com/team/longarm/')).toBe('https://git.example.com/team/longarm')
    expect(feedbackURL('https://chat.example.com/longarm', 'https://git.example.com/team/longarm')).toBe('https://chat.example.com/longarm')
    // a repository without a feedback link: that repository's issues
    expect(feedbackURL(undefined, 'https://git.example.com/team/longarm')).toBe('https://git.example.com/team/longarm/issues')
    // no hub, an older hub, or something that isn't a web link: no link
    expect(repoURL()).toBe('')
    expect(repoURL('javascript:alert(1)')).toBe('')
    expect(feedbackURL()).toBe('')
    expect(feedbackURL('file:///etc/passwd')).toBe('')
    expect(docsURL('https://git.example.com/team/longarm', 'usage.md', 'install')).toBe('https://git.example.com/team/longarm/blob/main/docs/usage.md#install')
    expect(docsURL('')).toBe('')
    expect(filmURL('https://git.example.com/team/longarm')).toBe('https://git.example.com/team/longarm/releases/download/promo-film/longarm-promo.mp4')
    expect(filmURL('')).toBe('')
  })
})
