import { defineConfig } from '@playwright/test'
import { fileURLToPath } from 'node:url'

import { PLAYWRIGHT_BROWSER_PROJECTS } from '../../playwright.projects.js'

const WEB_HOST = '127.0.0.1'
const WEB_PORT = 4183
const WEB_BASE_URL = `http://${WEB_HOST}:${WEB_PORT}`
const WEB_DIRECTORY = fileURLToPath(new URL('../..', import.meta.url))

export default defineConfig({
  testDir: '.',
  testMatch: 'r0-storage-contract.spec.ts',
  fullyParallel: false,
  workers: 1,
  reporter: 'line',
  projects: PLAYWRIGHT_BROWSER_PROJECTS,
  use: {
    baseURL: WEB_BASE_URL,
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: `pnpm exec vite --host ${WEB_HOST} --port ${WEB_PORT} --strictPort`,
    cwd: WEB_DIRECTORY,
    url: WEB_BASE_URL,
    reuseExistingServer: false,
  },
})
