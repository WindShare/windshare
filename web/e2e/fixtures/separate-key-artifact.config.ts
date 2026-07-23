import process from 'node:process'

import { defineConfig } from '@playwright/test'

import { PLAYWRIGHT_BROWSER_PROJECTS } from '../../playwright.projects.js'

const OUTPUT_DIR = process.env.WINDSHARE_ARTIFACT_PROBE_OUTPUT
const BASE_URL = process.env.WINDSHARE_ARTIFACT_PROBE_BASE_URL
const PROBE_TIMEOUT_MS = 10_000
const ORDINARY_FAILURE_ACTION_TIMEOUT_MS = 500

if (OUTPUT_DIR === undefined || BASE_URL === undefined) {
  throw new Error('Separate-key artifact probe requires isolated output and base URL settings')
}

export default defineConfig({
  testDir: '.',
  testMatch: 'separate-key-artifact.probe.ts',
  outputDir: OUTPUT_DIR,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: 'line',
  timeout: PROBE_TIMEOUT_MS,
  expect: { timeout: PROBE_TIMEOUT_MS },
  projects: PLAYWRIGHT_BROWSER_PROJECTS,
  use: {
    baseURL: BASE_URL,
    // The ordinary helper-failure branch needs a bounded action. Its sibling
    // explicitly restores production's unbounded action timing and fails by the
    // overall test deadline so both artifact-capture orders remain executable.
    actionTimeout: ORDINARY_FAILURE_ACTION_TIMEOUT_MS,
    // Playwright serializes password fill arguments into trace call logs. That
    // artifact is intentionally absent; screenshot, video, and Playwright's
    // automatic error context remain enabled and are inspected by the parent test.
    trace: 'off',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
})
