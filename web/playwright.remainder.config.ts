import { defineConfig } from '@playwright/test'

import baseConfig from './playwright.config.js'

// The focused sample owns the hot-switch proof. Excluding the file here keeps
// the remainder suite broad without letting one passing behavior count twice.
export default defineConfig(baseConfig, {
  testIgnore: ['e2e/v2-real-hot-switch.spec.ts'],
})
