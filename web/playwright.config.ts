import { defineConfig } from '@playwright/test'

// Driven by the Go test e2e.TestUIPlaywright (LONGARM_PLAYWRIGHT=1), which
// starts a real longarm-hub, longarmd workers, longarm daemon and longarm-tray --serve on
// loopback and passes their URLs in LONGARM_TEST_* variables.
export default defineConfig({
  testDir: 'e2e',
  timeout: 180_000,
  expect: { timeout: 30_000 },
  workers: 1,
  reporter: [['list']],
  use: {
    headless: true,
    viewport: { width: 1440, height: 900 },
    colorScheme: 'dark',
    screenshot: 'only-on-failure',
  },
})
