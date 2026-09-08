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
    // A sole shared folder opens automatically; its heading can coexist with the
    // root entry while that navigation is loading, so readiness belongs to the page.
    await expect(page.getByRole('heading', { name: DIRECTORY_NAME, exact: true })).toBeVisible()
    const explorer = page.getByRole('region', { name: 'Shared files', exact: true })
    const contents = explorer.getByRole('list', { name: 'Folder contents', exact: true })
    const pagination = explorer.getByRole('navigation', { name: 'Directory pages', exact: true })
    const previous = pagination.getByRole('button', { name: 'Previous', exact: true })
    const next = pagination.getByRole('button', { name: 'Next', exact: true })
    const firstFile = contents.getByRole('button', { name: fileName(0), exact: true })
    const lastFile = contents.getByRole('button', { name: fileName(FINAL_FILE_INDEX), exact: true })

    await expect(pagination.getByText('Page 1 of 2', { exact: true })).toBeVisible()
    await expect(contents.getByRole('listitem')).toHaveCount(V2_CATALOG_PAGE_ENTRIES)
    await expect(firstFile).toBeVisible()
    await expect(contents.getByRole('button', {
      name: fileName(V2_CATALOG_PAGE_ENTRIES - 1), exact: true,
    })).toBeVisible()
    await expect(lastFile).toHaveCount(0)
    await expect(previous).toBeDisabled()

    await next.click()
    await expect(pagination.getByText('Page 2 of 2', { exact: true })).toBeVisible()
    await expect(contents.getByRole('listitem')).toHaveCount(1)
    await expect(lastFile).toBeVisible()
    await expect(firstFile).toHaveCount(0)
    await expect(next).toBeDisabled()

    await previous.click()
    await expect(pagination.getByText('Page 1 of 2', { exact: true })).toBeVisible()
    await expect(contents.getByRole('listitem')).toHaveCount(V2_CATALOG_PAGE_ENTRIES)
    await expect(firstFile).toBeVisible()
    await expect(lastFile).toHaveCount(0)
    await expect(previous).toBeDisabled()
    await expect(next).toBeEnabled()
  } finally {
    await stack.dispose()
  }
})

function fileName(index: number): string {
  return `file-${index.toString().padStart(3, '0')}.bin`
}
