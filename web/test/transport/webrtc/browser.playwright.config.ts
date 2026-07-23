import { fileURLToPath } from 'node:url'
import { defineConfig } from '@playwright/test'

import { PLAYWRIGHT_BROWSER_PROJECTS } from '../../../playwright.projects.js'

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
  projects: PLAYWRIGHT_BROWSER_PROJECTS,
  use: {
    baseURL: WEB_BASE_URL,
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
      command: 'go run ./transport/webrtc/testdata/browser/server',
      cwd: REPOSITORY_ROOT,
      env: {
        WINDSHARE_D1_BROWSER_ADDR: PION_ADDRESS,
        WINDSHARE_D1_BROWSER_SCENARIO: 'happy',
      },
      url: `http://${PION_ADDRESS}/healthz`,
      reuseExistingServer: false,
      timeout: SERVER_TIMEOUT_MS,
    },
  ],
})
