import { expect, test } from '@playwright/test'

test('loads the application shell', async ({ page }) => {
  const response = await page.goto('/')
  if (response === null) {
    throw new Error('application navigation did not return an HTTP response')
  }
  expect(response.ok()).toBe(true)
  await expect(page.locator('#root')).toBeAttached()
})
