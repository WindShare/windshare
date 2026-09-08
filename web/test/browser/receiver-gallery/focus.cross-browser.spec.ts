import { expect, test, type Locator } from '@playwright/test'
import { CURRENT_TASK, GALLERY_PATH, showScenario } from './assertions'

test.use({ hasTouch: true })

for (const activation of ['pointer', 'keyboard', 'touch'] as const) {
  test(`sheets restore their actual ${activation} invoker across preview, details and nested confirmation`, async ({ page }) => {
    await page.goto(GALLERY_PATH)
    await showScenario(page, 'saved-cleanup')
    const contents = page.getByRole('list', { name: 'Folder contents' })
    const activate = async (trigger: Locator) => {
      if (activation === 'keyboard') {
        await trigger.focus()
        await trigger.press('Enter')
      } else if (activation === 'touch') await trigger.tap()
      else await trigger.click()
    }

    const triggers = [
      page.getByRole('button', { name: 'Landscape 1.png', exact: true }),
      page.getByRole('button', { name: 'Encrypted', exact: true }),
      page.locator('.connection-status'),
      page.locator(CURRENT_TASK).getByRole('button', { name: 'Details', exact: true }),
      page.getByRole('button', { name: 'Other ways to save', exact: true }),
    ]
    for (const trigger of triggers) {
      // Pointer activation need not move focus in WebKit. A distinct prior focus
      // proves that the sheet remembers its invoker rather than that prior owner.
      await contents.focus()
      await activate(trigger)
      const sheet = page.getByRole('dialog')
      await expect(sheet).toBeVisible()
      if (activation === 'keyboard') await page.keyboard.press('Escape')
      else await activate(sheet.getByRole('button', { name: 'Back to share', exact: true }))
      await expect(sheet).toHaveCount(0)
      await expect(trigger).toBeFocused()
    }

    const downloads = page.getByRole('button', { name: /^Downloads/ })
    await contents.focus()
    await activate(downloads)
    const outer = page.getByRole('dialog')
    const details = outer.getByRole('button', { name: 'Details', exact: true }).last()
    await activate(details)
    const removal = outer.getByText('Remove task or retained data', { exact: true })
    await removal.click()
    const remove = outer.getByRole('button', { name: 'Delete retained result', exact: true })
    await activate(remove)
    const confirmation = page.getByRole('dialog', { name: 'Delete retained result', exact: true })
    await expect(confirmation).toBeVisible()
    if (activation === 'keyboard') await page.keyboard.press('Escape')
    else await activate(confirmation.getByRole('button', { name: 'Keep task', exact: true }))
    await expect(confirmation).toHaveCount(0)
    await expect(outer).toBeVisible()
    await expect(remove).toBeFocused()
    await activate(outer.getByRole('button', { name: 'All downloads', exact: true }))
    await expect(details).toBeFocused()
    if (activation === 'keyboard') await page.keyboard.press('Escape')
    else await activate(outer.getByRole('button', { name: 'Close downloads', exact: true }))
    await expect(outer).toHaveCount(0)
    await expect(downloads).toBeFocused()
  })
}
