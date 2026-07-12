import { defineConfig } from '@playwright/test'

const WINDOWS_NETWORK_CONTRACT = 'stable-harness-v3'
const WINDOWS_LEASE_TOKEN_PATTERN = /^[0-9a-f]{32}$/u
const WINDOWS_RUNNER_PIPE_PATTERN = /^windshare-d5-[1-9]\d*-[0-9a-f]{32}$/u
if (
  process.platform === 'win32' &&
  (
    process.env.WINDSHARE_WINDOWS_OS_NETWORK !== WINDOWS_NETWORK_CONTRACT ||
    !WINDOWS_LEASE_TOKEN_PATTERN.test(process.env.WINDSHARE_D5_E2E_LEASE_TOKEN ?? '') ||
    !WINDOWS_RUNNER_PIPE_PATTERN.test(process.env.WINDSHARE_D5_RUNNER_PIPE ?? '')
  )
) {
  throw new Error(
    'Windows Playwright real-stack tests require scripts/d5-windows-performance.ps1 -Mode BrowserTests',
  )
}

const WEB_HOST = '127.0.0.1'
const WEB_PORT = 4173
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const WEB_SERVER_TIMEOUT_MS = 120_000
const BROWSER_TEST_TIMEOUT_MS = 120_000
const OUTPUT_DIRECTORY = process.env.WINDSHARE_D5_PLAYWRIGHT_OUTPUT_DIR ?? 'test-results'

export default defineConfig({
  testDir: '.',
  testMatch: ['test/browser/**/*.spec.ts', 'e2e/**/*.spec.ts'],
  outputDir: OUTPUT_DIRECTORY,
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: BROWSER_TEST_TIMEOUT_MS,
  use: {
    baseURL: WEB_BASE_URL,
    browserName: 'chromium',
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  webServer: {
    // A strict, fresh server makes an occupied developer port fail loudly instead
    // of letting the smoke test attach to unrelated content.
    command: `pnpm exec vite --host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
    url: WEB_BASE_URL,
    reuseExistingServer: false,
    timeout: WEB_SERVER_TIMEOUT_MS,
  },
})
