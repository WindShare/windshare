import { expect, test } from '@playwright/test'

import { V2_CATALOG_PAGE_ENTRIES } from '../src/catalog/v2-records'
import { capabilityUrl, DirectProductStack } from './fixtures/direct-product-stack'
import { withCapabilityRedaction } from './fixtures/capability-redactor'

const SCENARIO_ID = 'chromium-progressive-catalog'
const DIRECTORY_NAME = 'wide-directory'
const FINAL_FILE_INDEX = V2_CATALOG_PAGE_ENTRIES

test('browses a catalog directory across authenticated pages', async ({ page }) => {
  const stack = new DirectProductStack(SCENARIO_ID)
  await stack.start()
  try {
    const directory = await stack.createDirectory(
      DIRECTORY_NAME,
      Array.from({ length: FINAL_FILE_INDEX + 1 }, (_value, index) => ({
        name: fileName(index),
        bytes: Uint8Array.of(index & 0xff),
      })),
    )
    const share = await stack.share(directory)

    await page.addInitScript(() => {
      Object.defineProperty(window, 'RTCPeerConnection', { configurable: true, value: undefined })
    })
    const navigationUrl = capabilityUrl(share)
    await withCapabilityRedaction(() => page.goto(navigationUrl), {
      completeUrl: navigationUrl,
      fragment: new URL(navigationUrl).hash,
      separateKey: share.key,
    })
    await expect(page.getByText(DIRECTORY_NAME, { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Open' }).click()

    await expect(page.getByText('Page 1 of 2', { exact: true })).toBeVisible()
    await expect(page.getByText(fileName(0), { exact: true })).toBeVisible()
    await expect(page.getByText(fileName(V2_CATALOG_PAGE_ENTRIES - 1), { exact: true })).toBeVisible()
    await expect(page.getByText(fileName(FINAL_FILE_INDEX), { exact: true })).toHaveCount(0)

    await page.getByRole('button', { name: 'Next' }).click()
    await expect(page.getByText('Page 2 of 2', { exact: true })).toBeVisible()
    await expect(page.getByText(fileName(FINAL_FILE_INDEX), { exact: true })).toBeVisible()
    await expect(page.getByText(fileName(0), { exact: true })).toHaveCount(0)
  } finally {
    await stack.dispose()
  }
})

function fileName(index: number): string {
  return `file-${index.toString().padStart(3, '0')}.bin`
}
