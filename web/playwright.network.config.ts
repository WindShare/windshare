import { defineConfig } from '@playwright/test'

const NETWORK_TEST_TIMEOUT_MILLISECONDS = 80_000
const NETWORK_HARD_TIMEOUT_MILLISECONDS = 240_000

export default defineConfig({
  testDir: './e2e',
  // The ordinary smoke owns the relay-only path. These scenarios complement it
  // by proving authenticated direct and TURN peer adoption after relay loss.
  testMatch: [
    'v2-direct-hot-switch.weekly.spec.ts',
    'v2-local-turn.network.spec.ts',
  ],
  outputDir: 'test-results/network-routes',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: NETWORK_TEST_TIMEOUT_MILLISECONDS,
  globalTimeout: NETWORK_HARD_TIMEOUT_MILLISECONDS,
  expect: { timeout: 20_000 },
  projects: [{
    name: 'chromium',
    use: { browserName: 'chromium' },
  }],
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    // Receiver URLs carry capability material, so no browser artifact may retain it.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
})
