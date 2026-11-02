// Captures the hub's front page section by section at 2x device pixels, as
// still "footage" for the film (assets/site-*.png) plus each element's rect.
// The page is opened as if served by `hub` (its address in the install
// lines and the page text) while the frontend comes from the web dev server
// (cd web && npx vite --port 5288), so it can be ahead of what the hub runs.
// With a real hub, its release and sign-in providers are the real ones;
// the signed-in user is a stand-in.
// node tools/capture-site.mjs [hub] [dev-server]
import { createRequire } from 'node:module'
import { writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const { chromium } = createRequire(join(here, '../../../web/package.json'))('@playwright/test')
const hub = (process.argv[2] ?? 'https://hub.example.com').replace(/\/+$/, '')
const dev = process.argv[3] ?? 'http://localhost:5288'
const real = !hub.endsWith('.example.com')
const out = join(here, '..', 'assets')

const host = new URL(hub).host
const me = { user: 'alex', hub: host, version: '', role: 'user', tenant: 'alex', base_url: hub }
const rel = { version: '0.2.0', files: [{ name: 'longarm-windows-amd64.msi' }, { name: 'longarm-windows-arm64.msi' }] }
const auth = { providers: [{ id: 'entra', label: 'Microsoft' }] }

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/140' })
await page.emulateMedia({ reducedMotion: 'reduce' })
// the hub's pages and assets from the dev server (registered first: later routes win)
await page.route(`${hub}/**`, async (r) => {
  const u = new URL(r.request().url())
  r.fulfill({ response: await r.fetch({ url: dev + u.pathname + u.search }) })
})
await page.route('**/api/me', (r) => r.fulfill({ json: me }))
await page.route('**/releases/latest.json', (r) => (real ? r.continue() : r.fulfill({ json: rel })))
await page.route('**/auth/info', (r) => (real ? r.continue() : r.fulfill({ json: auth })))
await page.goto(hub + '/')
await page.waitForSelector('[data-testid=landing-signed-in]')
await page.evaluate(() => document.fonts.ready)
await page.waitForTimeout(800)

const rects = {}
// the hero as it first appears, the whole page, and the sections the film zooms into
await page.screenshot({ path: join(out, 'site-hero.png') })
await page.screenshot({ path: join(out, 'site-full.png'), fullPage: true })
for (const [name, sel] of Object.entries({
  top: '.lp-top',
  install: '.lp-install:first-child',
  start: '#start',
  msi: '[data-testid=landing-msi]',
  ps1: '[data-testid=landing-ps1]',
  auth: '[data-testid=landing-signed-in]',
})) {
  const b = await page.locator(sel).first().evaluate((el) => {
    const r = el.getBoundingClientRect()
    return { x: r.x + scrollX, y: r.y + scrollY, w: r.width, h: r.height }
  })
  rects[name] = b
}
rects.page = await page.evaluate(() => ({ w: document.documentElement.scrollWidth, h: document.documentElement.scrollHeight }))
writeFileSync(join(out, 'site-rects.json'), JSON.stringify(rects, null, 2))
console.log(rects)
await browser.close()
