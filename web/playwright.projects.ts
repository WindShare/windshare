import type { PlaywrightTestConfig } from '@playwright/test'

export const PLAYWRIGHT_BROWSER_NAMES = ['chromium', 'firefox', 'webkit'] as const

export type PlaywrightBrowserName = typeof PLAYWRIGHT_BROWSER_NAMES[number]

export const PLAYWRIGHT_BROWSER_PROJECTS: NonNullable<PlaywrightTestConfig['projects']> =
  PLAYWRIGHT_BROWSER_NAMES.map((browserName) => Object.freeze({
    name: browserName,
    use: Object.freeze({ browserName }),
  }))
