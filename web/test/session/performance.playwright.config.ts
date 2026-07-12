import { fileURLToPath } from 'node:url'
import { defineConfig } from '@playwright/test'

const WEB_ROOT = fileURLToPath(new URL('../../', import.meta.url))
const WEB_HOST = '127.0.0.1'
const WEB_PORT = 4178
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const SERVER_TIMEOUT_MS = 120_000
const BENCHMARK_TIMEOUT_MS = 15 * 60_000
const EVIDENCE_OUTPUT_DIR = process.env.WINDSHARE_D5_BROWSER_OUTPUT_DIR

if (EVIDENCE_OUTPUT_DIR === undefined || EVIDENCE_OUTPUT_DIR.length === 0) {
  throw new Error(
    'WINDSHARE_D5_BROWSER_OUTPUT_DIR is required; run scripts/d5-performance.ps1 so evidence cannot be overwritten',
  )
}

export default defineConfig({
  testDir: '.',
  testMatch: ['browser-performance.bench.ts'],
  outputDir: EVIDENCE_OUTPUT_DIR,
  preserveOutput: 'always',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: BENCHMARK_TIMEOUT_MS,
  use: {
    baseURL: WEB_BASE_URL,
    browserName: 'chromium',
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
  webServer: {
    command:
      `pnpm exec vite --host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
    cwd: WEB_ROOT,
    url: WEB_BASE_URL,
    reuseExistingServer: false,
    timeout: SERVER_TIMEOUT_MS,
  },
})
