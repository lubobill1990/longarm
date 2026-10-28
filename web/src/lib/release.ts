// Release versions: "1.2.3", "v1.2.3", "1.3.0-rc1" (as internal/update/version.go).

function split(v: string): { nums: number[]; pre: string } | undefined {
  const m = /^v?(\d+(?:\.\d+)*)(?:-([0-9A-Za-z.]+))?(?:\+.*)?$/.exec(v.trim())
  if (!m) return undefined
  return { nums: m[1].split('.').map(Number), pre: m[2] ?? '' }
}

/** -1 / 0 / 1; a version that doesn't parse ("dev") is older than any release. */
export function compareVersions(a: string, b: string): number {
  const pa = split(a)
  const pb = split(b)
  if (!pa || !pb) return !pa && !pb ? 0 : !pa ? -1 : 1
  for (let i = 0; i < Math.max(pa.nums.length, pb.nums.length); i++) {
    const x = pa.nums[i] ?? 0
    const y = pb.nums[i] ?? 0
    if (x !== y) return x < y ? -1 : 1
  }
  if (pa.pre === pb.pre) return 0
  if (pa.pre === '') return 1 // a release is newer than its pre-releases
  if (pb.pre === '') return -1
  return pa.pre < pb.pre ? -1 : 1
}

/** A released version older than latest (development builds never are). */
export function isOutdated(version: string | undefined, latest: string | undefined): boolean {
  if (!version || !latest || !split(version) || !split(latest)) return false
  return compareVersions(version, latest) < 0
}

/** The MSI of a release for arch, if it has one. */
export function msiFile(rel: { version: string; files?: { name: string }[] } | undefined, arch = 'amd64'): string | undefined {
  if (!rel?.version) return undefined
  const name = `longarm-windows-${arch}.msi`
  return rel.files?.some((f) => f.name === name) ? `/releases/${rel.version}/${name}` : undefined
}
