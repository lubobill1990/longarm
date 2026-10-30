import { expect, test, type Page, type Route } from '@playwright/test'
import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { extname, join } from 'node:path'
import * as mock from './mock'

// Screenshots of the tray popup (web/dist, built first) with demo data:
// every tab, light and dark, the ~460 px popup and a wide window, plus
// the sign-in flow, empty, loading and banner states. No tray, daemon or
// hub: the page's API calls are answered here.
//   pnpm build && pnpm shots          (SHOTS_DIR: where; default shots/out)
//   SHOTS_CHANNEL=msedge pnpm shots   (an installed Edge instead of Playwright's Chromium)

const dist = join(import.meta.dirname, '..', 'dist')
const out = process.env.SHOTS_DIR ?? join(import.meta.dirname, 'out')
mkdirSync(out, { recursive: true })
const ORIGIN = 'http://tray.test'
const TYPES: Record<string, string> = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.png': 'image/png' }

interface Scenario {
  needSignIn?: boolean
  signin?: 'idle' | 'running' | 'confirm'
  daemonDown?: boolean
  emptyPool?: boolean
  /** never answer the pool and jobs: the loading skeletons */
  loading?: boolean
  update?: boolean
  /** the client daemon is older than the tray (not restarted after an update) */
  daemonOld?: boolean
  /** a long jobs list (30 jobs) */
  manyJobs?: boolean
}

async function serve(page: Page, sc: Scenario = {}) {
  const json = (r: Route, body: unknown, status = 200) => r.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  await page.route(`${ORIGIN}/**`, async (r) => {
    const u = new URL(r.request().url())
    const p = u.pathname
    const post = r.request().method() === 'POST'
    if (p.startsWith('/trayapi/') || p.startsWith('/local/')) {
      if (sc.loading && (p.includes('/pool') || p.includes('/jobs'))) return // pending forever
      if (p === '/trayapi/info') return json(r, mock.info(sc))
      if (p === '/trayapi/settings') return json(r, mock.traySettings)
      if (p === '/trayapi/skills') return json(r, mock.skills)
      if (p === '/trayapi/signin/state') return json(r, mock.signinState(sc.signin ?? 'idle'))
      if (p === '/trayapi/update') return sc.update ? json(r, mock.update) : json(r, { error: 'no' }, 404)
      if (p === '/trayapi/pin') return json(r, { pinned: true })
      if (p.startsWith('/trayapi/')) return json(r, { ok: true })
      if (p === '/local/longarm/v1/pool') return json(r, mock.pool({ empty: sc.emptyPool }))
      if (p === '/local/longarm/v1/jobs') return json(r, { jobs: sc.manyJobs ? mock.manyJobs : mock.jobs })
      if (p.startsWith('/local/longarm/v1/jobs/') && p.endsWith('/logs'))
        return r.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          body: `event: log\ndata: ${JSON.stringify({ data: Buffer.from('> pnpm build\n\nvite v8.3.5 building for production...\n✓ 412 modules transformed.\ndist/index.html   0.41 kB\ndist/assets/index.css  31.2 kB\ndist/assets/index.js  402.7 kB\n✓ built in 3.82s\n').toString('base64') })}\n\nevent: end\ndata: {}\n\n`,
        })
      if (p === '/local/longarm/v1/forwards') return json(r, post ? mock.forwards[0] : { forwards: mock.forwards })
      if (p === '/local/longarm/v1/settings') return json(r, { transport: 'race' })
      if (p === '/local/longarm/v1/status') return json(r, { version: sc.daemonOld ? '0.9.1' : '0.9.2', identity: 'client/alice-laptop' })
      if (p === '/local/longarmd/v1/status') return json(r, mock.workerStatus)
      if (p.startsWith('/local/longarmd/v1/jobs')) return json(r, { jobs: mock.workerJobs })
      return json(r, { error: 'not found' }, 404)
    }
    let f = join(dist, p === '/' || p.startsWith('/tray') ? 'index.html' : p)
    if (!existsSync(f)) f = join(dist, 'index.html')
    return r.fulfill({ body: readFileSync(f), contentType: TYPES[extname(f)] ?? 'application/octet-stream' })
  })
}

const SIZES = { narrow: { width: 460, height: 780 }, wide: { width: 1180, height: 780 } }
const TABS: [string, string, string][] = [
  ['pool', 'Workers', 'workers'],
  ['jobs', 'My jobs', 'my-jobs'],
  ['forwards', 'Forwards', 'forwards'],
  ['worker', 'This worker', 'this-worker'],
  ['settings', 'Settings', 'settings'],
  ['account', 'Account', 'account'],
]

async function open(page: Page, tab: string, sc: Scenario = {}) {
  await serve(page, sc)
  await page.goto(`${ORIGIN}/tray?tab=${tab}&poll=1`)
  await expect(page.getByLabel('pin')).toHaveAttribute('aria-pressed', 'true')
}

const shot = (page: Page, name: string) => page.screenshot({ path: join(out, `${name}.png`) })

for (const scheme of ['light', 'dark'] as const) {
  for (const [size, viewport] of Object.entries(SIZES)) {
    test.describe(`${scheme} ${size}`, () => {
      test.use({ colorScheme: scheme, viewport })

      for (const [tab, label, file] of TABS) {
        test(`${file}`, async ({ page }) => {
          await open(page, tab)
          await expect(page.getByRole('tab', { name: label })).toHaveAttribute('aria-selected', 'true')
          await expect(page.locator('html')).toHaveAttribute('data-theme', scheme)
          if (tab === 'pool') await expect(page.getByTestId('worker-cpc-alice-x7k2q')).toBeVisible()
          if (tab === 'jobs') {
            await page.getByText('build', { exact: true }).click()
            await expect(page.getByTestId('log')).toContainText('built in')
          }
          if (tab === 'forwards') await expect(page.getByTestId('forwards')).toContainText('127.0.0.1:53000')
          if (tab === 'worker') await expect(page.getByTestId('worker-state')).toContainText('accepting work')
          if (tab === 'settings') await expect(page.getByTestId('client-info')).toContainText('0.9.2')
          if (tab === 'account') await expect(page.getByTestId('account-card')).toBeVisible()
          // the popup never scrolls sideways
          expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(viewport.width)
          await shot(page, `tray-${file}-${scheme}-${size}`)
        })
      }

      test('settings: agent skill', async ({ page }) => {
        await open(page, 'settings')
        const section = page.getByTestId('agent-skill')
        await expect(section.getByTestId('skill-state-copilot')).toHaveText('0.9.1 → 0.9.2')
        await section.scrollIntoViewIfNeeded()
        await section.screenshot({ path: join(out, `tray-agent-skill-${scheme}-${size}.png`) })
      })

      test('workers, menu and offline section open', async ({ page }) => {
        await open(page, 'pool')
        const card = page.getByTestId('worker-cpc-alice-x7k2q')
        await card.getByLabel('more actions').click()
        await page.getByTestId('offline-section').locator('summary').click()
        await shot(page, `tray-workers-menu-${scheme}-${size}`)
      })

      test('my jobs: a long list, a job opened', async ({ page }) => {
        await open(page, 'jobs', { manyJobs: true })
        await page.getByText('test-e2e', { exact: true }).click()
        await expect(page.getByTestId('log')).toContainText('built in')
        await shot(page, `tray-my-jobs-long-${scheme}-${size}`)
      })

      test('workers: the ⋯ menu, then a click outside', async ({ page }) => {
        await open(page, 'pool')
        const card = page.getByTestId('worker-cpc-alice-x7k2q')
        await card.getByLabel('more actions').click()
        await expect(card.getByRole('menu')).toBeVisible()
        await page.getByTestId('online-count').click()
        await expect(card.getByRole('menu')).toBeHidden()
        await shot(page, `tray-menu-outside-click-${scheme}-${size}`)
      })

      test('sign-in: start', async ({ page }) => {
        await open(page, 'account', { needSignIn: true })
        await expect(page.getByTestId('signin-start')).toBeVisible()
        await shot(page, `tray-signin-${scheme}-${size}`)
      })

      test('sign-in: the code', async ({ page }) => {
        await open(page, 'account', { needSignIn: true, signin: 'running' })
        await expect(page.getByTestId('signin-code')).toHaveText('BCDF-GHJK')
        await shot(page, `tray-signin-code-${scheme}-${size}`)
      })

      test('banners: not signed in, update, empty pool', async ({ page }) => {
        await open(page, 'pool', { needSignIn: true, emptyPool: true, update: true })
        await expect(page.getByTestId('signin-banner')).toBeVisible()
        await expect(page.getByTestId('empty-pool')).toBeVisible()
        await shot(page, `tray-banners-${scheme}-${size}`)
      })

      test('version details: the daemon behind, an update', async ({ page }) => {
        await open(page, 'pool', { update: true, daemonOld: true })
        await expect(page.getByTestId('version-chip')).toHaveClass(/warn/)
        await page.getByTestId('version-chip').click()
        await expect(page.getByTestId('version-daemon')).toHaveClass(/behind/)
        await expect(page.getByTestId('version-update')).toBeVisible()
        await shot(page, `tray-versions-${scheme}-${size}`)
      })

      test('sign-in: signed in already', async ({ page }) => {
        await open(page, 'account', { signin: 'confirm' })
        await expect(page.getByTestId('signin-confirm')).toBeVisible()
        await shot(page, `tray-signin-confirm-${scheme}-${size}`)
      })

      test('daemon down', async ({ page }) => {
        await open(page, 'jobs', { daemonDown: true })
        await expect(page.getByTestId('daemon-down')).toBeVisible()
        await shot(page, `tray-daemon-down-${scheme}-${size}`)
      })

      test('loading', async ({ page }) => {
        await open(page, 'pool', { loading: true })
        await expect(page.getByTestId('pool-loading')).toBeVisible()
        await shot(page, `tray-loading-${scheme}-${size}`)
      })
    })
  }
}

test.describe('keyboard', () => {
  test.use({ colorScheme: 'light', viewport: SIZES.narrow })
  test('arrows move between tabs; the focus ring shows', async ({ page }) => {
    await open(page, 'pool')
    await page.getByRole('tab', { name: 'Workers' }).focus()
    await page.keyboard.press('ArrowRight')
    await expect(page.getByRole('tab', { name: 'My jobs' })).toBeFocused()
    await expect(page.getByRole('tab', { name: 'My jobs' })).toHaveAttribute('aria-selected', 'true')
    await page.keyboard.press('End')
    await expect(page.getByRole('tab', { name: 'Account' })).toBeFocused()
    await shot(page, 'tray-keyboard-focus-light-narrow')
  })
  test('Settings > Appearance: Dark switches the theme', async ({ page }) => {
    await open(page, 'settings')
    await page.locator('.segmented label', { hasText: 'Dark' }).click()
    await expect(page.getByRole('radio', { name: 'Dark' })).toBeChecked()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
    await shot(page, 'tray-settings-theme-dark-override')
  })
})
