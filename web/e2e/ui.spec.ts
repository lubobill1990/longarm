import { expect, test, type Page } from '@playwright/test'
import { createHmac } from 'node:crypto'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

const env = (k: string) => {
  const v = process.env[k]
  if (!v) throw new Error(`${k} not set (run through go test -run TestUIPlaywright ./e2e)`)
  return v
}
const shots = process.env.LONGARM_TEST_SHOTS ?? join(__dirname, '..', '..', 'docs', 'ui-screenshots')
mkdirSync(shots, { recursive: true })
const shot = (page: Page, name: string) => page.screenshot({ path: join(shots, `${name}.png`), fullPage: true })

/** RFC 6238 TOTP (SHA-1, 30 s, 6 digits) of a base32 secret. */
function totp(secret: string, at = Date.now()): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of secret.replace(/=+$/, '').toUpperCase()) bits += alphabet.indexOf(c).toString(2).padStart(5, '0')
  const key = Buffer.alloc(Math.floor(bits.length / 8))
  for (let i = 0; i < key.length; i++) key[i] = parseInt(bits.slice(i * 8, i * 8 + 8), 2)
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)))
  const h = createHmac('sha1', key).update(msg).digest()
  const o = h[h.length - 1] & 0xf
  const n = ((h[o] & 0x7f) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3]
  return String(n % 1_000_000).padStart(6, '0')
}

test.describe.configure({ mode: 'serial' })

test('hub panel: TOTP login, approve, live metrics, jobs, logs, kill, every page', async ({ page }) => {
  const hub = env('LONGARM_TEST_HUB_URL')
  const helper = env('LONGARM_TEST_HELPER')
  const pending = env('LONGARM_TEST_PENDING')

  // Login with password + TOTP.
  await page.goto(hub + '/')
  await expect(page.getByRole('button', { name: 'Log in' })).toBeVisible()
  await page.getByLabel('user').fill(env('LONGARM_TEST_USER'))
  await page.getByLabel('password').fill(env('LONGARM_TEST_PASS'))
  await page.getByLabel('TOTP code').fill('000000')
  await shot(page, 'panel-login')
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page.getByText('wrong user, password or code')).toBeVisible()
  await page.getByLabel('TOTP code').fill(totp(env('LONGARM_TEST_TOTP')))
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page.getByRole('heading', { name: 'Nodes' })).toBeVisible()

  // Workers online; a pending one waits for approval.
  await expect(page.getByTestId('node-wa')).toContainText('online')
  await expect(page.getByTestId('node-wb')).toContainText('online')
  await expect(page.getByTestId(`pending-${pending}`)).toBeVisible()
  // Live metrics: wa's CPU sparkline changes without a reload (SSE).
  const spark = page.getByTestId('node-wa').locator('path').first()
  const before = await spark.getAttribute('d')
  await expect.poll(async () => spark.getAttribute('d'), { timeout: 30_000 }).not.toBe(before)
  await expect(page.getByText(/alice:/).first()).toBeVisible({ timeout: 40_000 }) // a client link report
  await shot(page, 'panel-nodes-pending')

  // Approve the pending worker: it finishes enrolling and comes online.
  await page.getByTestId(`pending-${pending}`).getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByTestId(`pending-${pending}`)).toHaveCount(0)
  await expect(page.getByTestId(`node-${pending}`)).toContainText('online', { timeout: 60_000 })
  await shot(page, 'panel-nodes')

  // Jobs: submit on wa, follow the live log to the end.
  await page.getByRole('link', { name: 'Jobs' }).click()
  await page.getByRole('button', { name: 'New job' }).click()
  await page.getByLabel('worker', { exact: true }).selectOption('wa')
  await page.getByLabel('command').fill(`"${helper}" print 40 50 0`)
  await page.getByLabel('name', { exact: true }).fill('pw-print')
  await page.getByRole('button', { name: 'Run' }).click()
  await expect(page.getByRole('heading', { name: 'pw-print' })).toBeVisible()
  await expect(page.getByTestId('log')).toContainText('line 1')
  await expect(page.getByTestId('log')).toContainText('line 40', { timeout: 60_000 })
  await expect(page.getByTestId('log-state')).toHaveText('end of output')
  await expect(page.locator('.page-title .pill')).toHaveText('done')
  await shot(page, 'panel-job-logs')

  // A long job: kill it from the list.
  await page.getByRole('link', { name: '← Jobs' }).click()
  await page.getByRole('button', { name: 'New job' }).click()
  await page.getByLabel('worker', { exact: true }).selectOption('wa')
  await page.getByLabel('command').fill(`"${helper}" sleep 300`)
  await page.getByLabel('name', { exact: true }).fill('pw-sleep')
  await page.getByRole('button', { name: 'Run' }).click()
  await expect(page.locator('.page-title .pill')).toHaveText('running')
  await page.getByRole('link', { name: '← Jobs' }).click()
  const row = page.locator('tr', { hasText: 'pw-sleep' })
  await row.getByRole('button', { name: 'Kill' }).click()
  await expect(row).toContainText('killed')
  await shot(page, 'panel-jobs')

  // Join codes with the install commands.
  await page.getByRole('link', { name: 'Add machine' }).click()
  await page.getByLabel('labels').fill('region=lab')
  await page.getByRole('button', { name: 'Create join code' }).click()
  await expect(page.getByTestId('join-code')).toHaveText(/^[0-9A-Z]{3}-[0-9A-Z]{3}$/)
  await expect(page.getByTestId('install-windows')).toContainText('install.ps1')
  await expect(page.getByTestId('install-linux')).toContainText('install.sh | sh -s -- --join')
  await shot(page, 'panel-joincodes')

  // Toolchain profiles.
  await page.getByRole('link', { name: 'Toolchains' }).click()
  await page.getByRole('button', { name: 'New profile' }).click()
  await page.getByLabel('profile name').fill('dev')
  await page.getByLabel('tools').fill('git=latest\nnode=22\npnpm=latest')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByTestId('profile-dev')).toContainText('node=22')
  await shot(page, 'panel-profiles')

  await page.getByRole('link', { name: 'Links' }).click()
  await expect(page.getByTestId('links')).toContainText('alice')
  await shot(page, 'panel-links')

  await page.getByRole('link', { name: 'Audit' }).click()
  await expect(page.getByTestId('audit')).toContainText('web/ops via hub')
  await shot(page, 'panel-audit')

  await page.getByRole('link', { name: 'Panel logins' }).click()
  await page.getByLabel('new user name').fill('eve')
  await page.getByLabel('new user password').fill('a long enough password')
  await page.getByLabel('your password').fill(env('LONGARM_TEST_PASS'))
  await page.getByRole('button', { name: 'Add user' }).click()
  await expect(page.getByTestId('totp-secret')).toContainText('otpauth://totp/')
  await expect(page.getByTestId('user-eve')).toBeVisible()
  await shot(page, 'panel-users')
})

test('longarm-tray popup: pool with latency, my jobs, forwards, worker tab, settings', async ({ page }) => {
  await page.setViewportSize({ width: 460, height: 900 })
  await page.goto(env('LONGARM_TEST_TRAY_URL'))
  await expect(page.getByTestId('online-count')).toHaveText(/[23]\/3 online/)
  const wa = page.getByTestId('worker-wa')
  await expect(wa).toHaveAttribute('data-state', 'online')
  await expect(wa.getByTestId('link-path')).toHaveText('tcp')
  await expect(wa.getByTestId('link-rtt')).toHaveText(/ms$/)
  await expect(page.getByTestId('source-text')).toHaveText(/live from|hub \+ direct/)
  // no horizontal overflow in the narrow popup
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(460)
  await shot(page, 'tray-workers')

  await page.getByRole('tab', { name: 'My jobs' }).click()
  await expect(page.getByTestId('my-jobs')).toBeVisible()
  await shot(page, 'tray-my-jobs')

  await page.getByRole('tab', { name: 'Forwards' }).click()
  await page.getByLabel('target port').fill(process.env.LONGARM_TEST_FORWARD_PORT ?? '3131')
  await page.getByRole('button', { name: 'Forward' }).click()
  await expect(page.getByTestId('forwards')).toContainText('127.0.0.1:')
  await shot(page, 'tray-forwards')

  await page.getByRole('tab', { name: 'This worker' }).click()
  await expect(page.getByTestId('worker-state')).toContainText('accepting work')
  await page.getByTestId('drain-toggle').click()
  await expect(page.getByTestId('worker-state')).toContainText('draining')
  await shot(page, 'tray-worker')
  await page.getByTestId('drain-toggle').click()
  await expect(page.getByTestId('worker-state')).toContainText('accepting work')

  await page.getByRole('tab', { name: 'Settings' }).click()
  await page.getByLabel(/prefer plain tcp/).check()
  await expect(page.getByText('CLI: longarm transport tcp')).toBeVisible()
  await shot(page, 'tray-settings')
  await page.getByLabel(/race: dial every leg/).check()
  await expect(page.getByText('CLI: longarm transport race')).toBeVisible()
})
