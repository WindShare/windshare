import { defineConfig } from '@playwright/test'

import baseConfig from './playwright.config.js'
import { createPlaywrightSuiteDeclarations } from './playwright.suite-config.ts'

const focused = createPlaywrightSuiteDeclarations('main', 'focused')
const chromium = focused.projects.filter(({ name }) => name === 'chromium')
if (chromium.length !== 1) throw new Error('Chromium smoke requires exactly one Chromium project')

// The PR smoke is intentionally one product oracle. Full browser diversity stays
// in Browsergate's main/Pion matrix rather than being hidden behind this entry.
export default defineConfig(baseConfig, {
  testDir: focused.testDir,
  testMatch: focused.testMatch,
  projects: chromium,
  retries: 0,
  workers: 1,
})
