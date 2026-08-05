import { defineConfig } from '@playwright/test'

const CROSS_BROWSER_TEST_TIMEOUT_MILLISECONDS = 80_000
const CROSS_BROWSER_HARD_TIMEOUT_MILLISECONDS = 180_000

export default defineConfig({
  testDir: './e2e',
  testMatch: ['v2-direct-smoke.spec.ts', 'v2-direct-hot-switch.cross-browser.spec.ts'],
  outputDir: 'test-results/cross-browser-smoke',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: CROSS_BROWSER_TEST_TIMEOUT_MILLISECONDS,
  globalTimeout: CROSS_BROWSER_HARD_TIMEOUT_MILLISECONDS,
  expect: { timeout: 20_000 },
  projects: [
    { name: 'firefox', use: { browserName: 'firefox' } },
    { name: 'webkit', use: { browserName: 'webkit' } },
  ],
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    // The smoke navigates through a capability-bearing receiver URL.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
})
