import { defineConfig } from '@playwright/test'
import { fileURLToPath } from 'node:url'

const WEB_HOST = '127.0.0.1'
const DEFAULT_WEB_PORT = 4197
const WEB_PORT = contractPort(process.env.WINDSHARE_CONTRACT_PORT)
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const WEB_DIRECTORY = fileURLToPath(new URL('.', import.meta.url))

const ALL_COMPONENT_SPECS = '**/*.spec.ts'
const PERIODIC_COMPONENT_SPECS = '**/*.periodic.spec.ts'
const CROSS_BROWSER_COMPONENT_SPECS = '**/*.cross-browser.spec.ts'

function contractPort(configured: string | undefined): number {
  if (configured === undefined) return DEFAULT_WEB_PORT
  if (!/^\d+$/u.test(configured)) {
    throw new Error('WINDSHARE_CONTRACT_PORT must be a decimal TCP port')
  }
  const port = Number(configured)
  if (!Number.isSafeInteger(port) || port < 1_024 || port > 65_535) {
    throw new Error('WINDSHARE_CONTRACT_PORT must be between 1024 and 65535')
  }
  return port
}

export default defineConfig({
  testDir: './test/browser',
  outputDir: 'test-results/browser-contract',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: 80_000,
  expect: { timeout: 20_000 },
  projects: [
    {
      name: 'chromium-short',
      testMatch: ALL_COMPONENT_SPECS,
      testIgnore: PERIODIC_COMPONENT_SPECS,
      use: { browserName: 'chromium' },
    },
    {
      name: 'firefox-short',
      testMatch: CROSS_BROWSER_COMPONENT_SPECS,
      use: { browserName: 'firefox' },
    },
    {
      name: 'webkit-short',
      testMatch: CROSS_BROWSER_COMPONENT_SPECS,
      use: { browserName: 'webkit' },
    },
    {
      name: 'chromium-periodic',
      testMatch: PERIODIC_COMPONENT_SPECS,
      use: { browserName: 'chromium' },
    },
  ],
  use: {
    baseURL: WEB_BASE_URL,
    locale: 'en-US',
    timezoneId: 'UTC',
    // Contract pages can carry capability material in navigation state; retaining
    // any browser artifact would make a failure artifact a second secret sink.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
  webServer: {
    command: `pnpm exec vite --host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
    cwd: WEB_DIRECTORY,
    url: WEB_BASE_URL,
    reuseExistingServer: false,
  },
})
