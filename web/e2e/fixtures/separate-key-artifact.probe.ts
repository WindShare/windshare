import process from 'node:process'

import { expect, test, type Page } from '@playwright/test'

import { submitCapabilityKey } from './capability-input'

const SHARE_WITHOUT_FRAGMENT = '/s/AAAAAAAAAAAAAAAA'
const MAIN_CONFIG_TIMEOUT_PROBE_MS = 3_000
const temporaryKey = process.env.WINDSHARE_ARTIFACT_PROBE_KEY

if (temporaryKey === undefined || temporaryKey === '') {
  throw new Error('Separate-key artifact probe requires a temporary key')
}

async function mountKeyEntry(page: Page) {
  await page.goto(SHARE_WITHOUT_FRAGMENT)
  await expect(page.getByLabel('Separate key')).toBeVisible()
}

async function disableSubmitAfterInput(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Open share' }).evaluate((button) => {
    if (!(button instanceof HTMLButtonElement)) throw new Error('submit button is invalid')
    const input = button.form?.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) throw new Error('key field is missing')
    input.addEventListener('input', () => {
      button.disabled = true
    }, { once: true })
  })
}

test('sanitizes a helper failure after the key field was filled', async ({ page }) => {
  await mountKeyEntry(page)
  await disableSubmitAfterInput(page)

  await expect(submitCapabilityKey(page, temporaryKey)).rejects.toThrow(
    'The browser could not submit the temporary capability key',
  )
  throw new Error('Forced sanitized separate-key helper failure')
})

test.describe('production action timing', () => {
  test.use({ actionTimeout: 0 })

  test('keeps the test-wide timeout artifact capability-free', async ({ page }) => {
    test.setTimeout(MAIN_CONFIG_TIMEOUT_PROBE_MS)
    await mountKeyEntry(page)
    await disableSubmitAfterInput(page)
    await expect(page.getByRole('button', { name: 'Open share' })).toBeEnabled()

    await submitCapabilityKey(page, temporaryKey)
    throw new Error('Separate-key timeout probe unexpectedly submitted')
  })
})
