import { defineConfig } from '@playwright/test'

const PROGRESSIVE_TEST_TIMEOUT_MILLISECONDS = 75_000
const PROGRESSIVE_HARD_TIMEOUT_MILLISECONDS = 150_000

export default defineConfig({
  testDir: './e2e',
  // Route switching has its own network gate so this owner cannot silently
  // acquire another expensive weekly scenario through a filename wildcard.
  testMatch: ['v2-direct-progressive.weekly.spec.ts'],
  outputDir: 'test-results/progressive-catalog',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: PROGRESSIVE_TEST_TIMEOUT_MILLISECONDS,
  globalTimeout: PROGRESSIVE_HARD_TIMEOUT_MILLISECONDS,
  expect: { timeout: 20_000 },
  projects: [{
    name: 'chromium',
    use: { browserName: 'chromium' },
  }],
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    // Direct receiver setup passes capability keys through the page boundary; keep
    // all browser artifacts disabled at this owner boundary.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
})
