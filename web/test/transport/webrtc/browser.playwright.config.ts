import { defineConfig } from '@playwright/test'

const INTEROP_TEST_TIMEOUT_MILLISECONDS = 60_000
const INTEROP_HARD_TIMEOUT_MILLISECONDS = 120_000

export default defineConfig({
  testDir: '.',
  testMatch: ['browser.spec.ts', 'pion-interop.spec.ts'],
  outputDir: '../../../test-results/pion-interop',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: INTEROP_TEST_TIMEOUT_MILLISECONDS,
  globalTimeout: INTEROP_HARD_TIMEOUT_MILLISECONDS,
  projects: [{
    name: 'chromium',
    use: { browserName: 'chromium' },
  }],
  use: {
    baseURL: ownedWebBaseURL(),
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'off',
    screenshot: 'only-on-failure',
    video: 'off',
  },
})

function ownedWebBaseURL(): string {
  const encoded = process.env.WINDSHARE_WEB_BASE_URL ?? 'http://127.0.0.1:1'
  const parsed = new URL(encoded)
  const port = Number(parsed.port)
  if (
    parsed.protocol !== 'http:' || parsed.hostname !== '127.0.0.1' ||
    !Number.isSafeInteger(port) || port < 1 || port > 65_535 ||
    parsed.pathname !== '/' || parsed.search !== '' || parsed.hash !== ''
  ) throw new Error('WINDSHARE_WEB_BASE_URL must identify an owned loopback HTTP listener')
  return parsed.href.slice(0, -1)
}
