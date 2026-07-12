import { fileURLToPath } from 'node:url'
import { defineConfig } from '@playwright/test'

const REPOSITORY_ROOT = fileURLToPath(new URL('../../../../', import.meta.url))
const WEB_ROOT = fileURLToPath(new URL('../../../', import.meta.url))
const WEB_HOST = '127.0.0.1'
const WEB_PORT = 4176
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const PION_ADDRESS = '127.0.0.1:17849'
const SERVER_TIMEOUT_MS = 120_000

export default defineConfig({
  testDir: '.',
  testMatch: ['browser.spec.ts', 'pion-interop.spec.ts'],
  outputDir: '../../../test-results/d2-webrtc',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: 'line',
  timeout: 120_000,
  use: {
    baseURL: WEB_BASE_URL,
    browserName: 'chromium',
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  webServer: [
    {
      command:
        `pnpm exec vite --config test/transport/webrtc/vite.config.ts ` +
        `--host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
      cwd: WEB_ROOT,
      url: WEB_BASE_URL,
      reuseExistingServer: false,
      timeout: SERVER_TIMEOUT_MS,
    },
    {
      command: 'go run ./transport/webrtc/testdata/chromium/server',
      cwd: REPOSITORY_ROOT,
      env: {
        WINDSHARE_D1_CHROMIUM_ADDR: PION_ADDRESS,
        WINDSHARE_D1_CHROMIUM_SCENARIO: 'happy',
      },
      url: `http://${PION_ADDRESS}/healthz`,
      reuseExistingServer: false,
      timeout: SERVER_TIMEOUT_MS,
    },
  ],
})
