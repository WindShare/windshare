import { expect, type Page } from '@playwright/test'
import { join } from 'node:path'
import type { Scenario } from './fixtures'

export const GALLERY_PATH = '/test/browser/receiver-gallery/index.html'
export const CURRENT_TASK = '.share-workspace > .task-card'

export async function showScenario(page: Page, scenario: Scenario) {
  await page.getByLabel('Synthetic scenario').selectOption(scenario)
  await expect(page.locator('[data-gallery-scenario]')).toHaveAttribute('data-gallery-scenario', scenario)
  await expect(page.locator(scenario.startsWith('portal') ? '.portal-root' : '.receiver-shell')).toBeVisible()
}

export async function capture(page: Page, name: string, fullPage = false) {
  const directory = process.env.WINDSHARE_GALLERY_EVIDENCE_DIR
  if (directory !== undefined) {
    // The explicit fixture route owns synthetic media and operation identities.
    expect(new URL(page.url()).pathname).toBe(GALLERY_PATH)
    await page.screenshot({ path: join(directory, name + '.png'), fullPage })
  }
}

export async function galleryEvidence(page: Page) {
  return page.evaluate(() => {
    const gallery = window as typeof window & { windshareGalleryEvidence(): { intents: string[]; taskId: string | null; taskLabel: string | null; taskBytes: string; draftEmpty: boolean } }
    return gallery.windshareGalleryEvidence()
  })
}

export async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => ({
    width: innerWidth, fontSize: getComputedStyle(document.documentElement).fontSize,
    scrollWidth: document.documentElement.scrollWidth,
    elements: [...document.querySelectorAll('.receiver-shell *')].filter(element => element.getBoundingClientRect().right > innerWidth + 1)
      .map(element => element.className).slice(0, 12),
  }))
  expect(overflow.scrollWidth, JSON.stringify(overflow)).toBeLessThanOrEqual(overflow.width)
}
