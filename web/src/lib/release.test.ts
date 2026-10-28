import { describe, expect, it } from 'vitest'
import { compareVersions, isOutdated, msiFile } from './release'

describe('release versions', () => {
  it('compares like the hub (numeric parts, a release after its pre-releases)', () => {
    expect(compareVersions('1.2.0', '1.10.0')).toBe(-1)
    expect(compareVersions('v1.2.0', '1.2.0')).toBe(0)
    expect(compareVersions('1.3.0-rc1', '1.3.0')).toBe(-1)
    expect(compareVersions('1.3.0', '1.2.9')).toBe(1)
    expect(compareVersions('dev', '0.0.1')).toBe(-1)
  })
  it('isOutdated: only released versions older than the latest', () => {
    expect(isOutdated('0.1.2', '0.1.3')).toBe(true)
    expect(isOutdated('0.1.3', '0.1.3')).toBe(false)
    expect(isOutdated('0.2.0', '0.1.3')).toBe(false)
    expect(isOutdated('dev', '0.1.3')).toBe(false)
    expect(isOutdated('0.1.2', '')).toBe(false)
    expect(isOutdated(undefined, '0.1.3')).toBe(false)
  })
  it('msiFile: the MSI path when the release has one', () => {
    const rel = { version: '0.1.3', files: [{ name: 'longarm-windows-amd64.msi' }, { name: 'longarmd-linux-amd64' }] }
    expect(msiFile(rel)).toBe('/releases/0.1.3/longarm-windows-amd64.msi')
    expect(msiFile(rel, 'arm64')).toBeUndefined()
    expect(msiFile({ version: '' })).toBeUndefined()
  })
})

