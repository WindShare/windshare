import { expect, test } from '@playwright/test'
import { openApplication, reloadApplication } from './navigation'

// A suspended module reproduces the browser load/initialization ordering without
// depending on CI load, network timing, or a fixed sleep.
const SUSPEND_STARTUP = `
await new Promise(resolve => {
  window.resumeApplicationStartup = resolve;
});
`

declare global {
  interface Window {
    resumeApplicationStartup: () => void
  }
}

test('application navigation awaits initialization for every document', async ({ page }) => {
  await page.route('**/src/main.tsx', async route => {
    const response = await route.fetch()
    await route.fulfill({ response, body: SUSPEND_STARTUP + await response.text() })
  })

  const navigations = [
    () => openApplication(page, '/'),
    () => reloadApplication(page),
    () => openApplication(page, '/different-share'),
  ]
  for (const navigate of navigations) {
    const loaded = page.waitForEvent('load')
    let initialized = false
    const navigation = navigate().then(response => {
      initialized = true
      return response
    })
    await loaded
    await expect.poll(() => page.evaluate(() => typeof window.resumeApplicationStartup)).toBe('function')
    try {
      expect(await page.evaluate(() => window.windshareDiagnostics)).toBeUndefined()
      expect(initialized).toBe(false)
    } finally {
      await page.evaluate(() => window.resumeApplicationStartup())
      expect((await navigation)?.ok()).toBe(true)
    }
    expect(await page.evaluate(() => window.windshareDiagnostics.status().enabled)).toBe(false)
  }
})
