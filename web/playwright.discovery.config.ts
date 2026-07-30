import type { PlaywrightTestConfig } from '@playwright/test'

import { createPlaywrightDiscoveryProjects } from './playwright.suite-config.ts'

/**
 * A named pure factory cannot be selected directly by Playwright's --config.
 * The discovery launcher gives each child a private one-shot default export,
 * keeping repository files incapable of becoming a reusable execution bypass.
 */
export function createPlaywrightDiscoveryConfig(): Pick<PlaywrightTestConfig, 'projects'> {
  return { projects: createPlaywrightDiscoveryProjects() }
}
