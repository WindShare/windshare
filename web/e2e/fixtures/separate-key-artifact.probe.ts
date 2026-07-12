import process from 'node:process'

import { expect, test, type Page } from '@playwright/test'

import { submitSeparateKey } from './test'

interface C4Harness {
  mountC4Harness(mode: 'key' | 'key-pending'): Promise<void>
  failPendingJoin(): void
}

const HARNESS_PATH = '/test/browser/c4-harness.ts'
const MAIN_CONFIG_TIMEOUT_PROBE_MS = 3_000
const temporaryKey = process.env.WINDSHARE_ARTIFACT_PROBE_KEY

if (temporaryKey === undefined || temporaryKey === '') {
  throw new Error('Separate-key artifact probe requires a temporary key')
}

async function mountKeyEntry(page: Page, pendingJoin: boolean) {
  await page.goto('/')
  const mode: 'key' | 'key-pending' = pendingJoin ? 'key-pending' : 'key'
  await page.evaluate(async ({ path, mode }) => {
    const harness = (await import(path)) as C4Harness
    await harness.mountC4Harness(mode)
  }, { path: HARNESS_PATH, mode })
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
  await mountKeyEntry(page, false)
  await disableSubmitAfterInput(page)

  await expect(submitSeparateKey(page, temporaryKey)).rejects.toThrow(
    'The browser could not submit the temporary separate key',
  )
  throw new Error('Forced sanitized separate-key helper failure')
})

test.describe('production action timing', () => {
  test.use({ actionTimeout: 0 })

  test('keeps the test-wide timeout artifact capability-free', async ({ page }) => {
    test.setTimeout(MAIN_CONFIG_TIMEOUT_PROBE_MS)
    await mountKeyEntry(page, false)
    await disableSubmitAfterInput(page)
    await expect(page.getByRole('button', { name: 'Open share' })).toBeEnabled()

    await submitSeparateKey(page, temporaryKey)
    throw new Error('Separate-key timeout probe unexpectedly submitted')
  })
})

test('keeps a late gateway failure artifact on an empty retry form', async ({ page }) => {
  await mountKeyEntry(page, true)
  await submitSeparateKey(page, temporaryKey)
  await page.evaluate(async (path) => {
    const harness = (await import(path)) as C4Harness
    harness.failPendingJoin()
  }, HARNESS_PATH)
  await expect(page.getByLabel('Separate key')).toHaveValue('')

  throw new Error('Forced late gateway failure after capability destruction')
})
