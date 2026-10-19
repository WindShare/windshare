import { defineConfig } from '@playwright/test'

// Cloud credentials and network availability belong to an explicit evidence run,
// never the ordinary local/CI browser gate.
export default defineConfig({
  testDir: './e2e',
  testMatch: 'cloudflare-relay.spec.ts',
  outputDir: 'test-results/cloudflare',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: [['line'], ['json', { outputFile: 'test-results/cloudflare/report.json' }]],
  timeout: 900_000,
  expect: { timeout: 30_000 },
  use: {
    browserName: 'chromium',
    locale: 'en-US',
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
})
