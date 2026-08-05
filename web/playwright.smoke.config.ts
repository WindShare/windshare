import { defineConfig } from '@playwright/test'

const SMOKE_TEST_TIMEOUT_MILLISECONDS = 80_000
const SMOKE_HARD_TIMEOUT_MILLISECONDS = 90_000

export default defineConfig({
  testDir: './e2e',
  testMatch: ['v2-direct-smoke.spec.ts'],
  outputDir: 'test-results/direct-smoke',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: SMOKE_TEST_TIMEOUT_MILLISECONDS,
  globalTimeout: SMOKE_HARD_TIMEOUT_MILLISECONDS,
  expect: { timeout: 15_000 },
  projects: [{
    name: 'chromium',
    use: { browserName: 'chromium' },
  }],
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    // Navigation consumes the capability fragment; no browser artifact may outlive that secret.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
})
