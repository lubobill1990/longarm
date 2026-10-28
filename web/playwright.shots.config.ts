import { defineConfig } from '@playwright/test'

// The tray screenshots with demo data (shots/tray.spec.ts): needs web/dist
// (pnpm build) and Playwright's Chromium (or SHOTS_CHANNEL=msedge).
export default defineConfig({
  testDir: 'shots',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  workers: 1,
  reporter: [['list']],
  use: {
    headless: true,
    // also checks prefers-reduced-motion: no transition half-way in a screenshot
    reducedMotion: 'reduce',
    channel: process.env.SHOTS_CHANNEL || undefined,
    launchOptions: { args: ['--renderer-process-limit=1'] },
  },
})
