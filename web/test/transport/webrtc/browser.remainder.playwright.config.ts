import { defineConfig } from '@playwright/test'

import baseConfig from './browser.playwright.config.js'

// Adapter interop is already measured in isolated focused processes; this
// configuration owns every other Pion browser test exactly once.
export default defineConfig(baseConfig, {
  testIgnore: ['pion-interop.spec.ts'],
})
