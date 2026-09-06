import { expect, test } from '@playwright/test'
import { capture, CURRENT_TASK, GALLERY_PATH, JADE, showScenario } from './assertions'

test('recovery details and nested removal confirmation keep opaque scoped surfaces and native focus', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  for (const colorScheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' })
    await showScenario(page, 'saved-cleanup')
    await page.locator(CURRENT_TASK).getByRole('button', { name: 'Details', exact: true }).click()
    let dialog = page.getByRole('dialog')
    await expect(dialog.locator('.compatible-name-repair')).toBeVisible()
    await expect(dialog).toHaveCSS('background-color', JADE[colorScheme].paper)
    const backdrop = await dialog.evaluate(element => getComputedStyle(element, '::backdrop').backgroundColor)
    expect(backdrop).not.toBe('rgba(0, 0, 0, 0)')
    await capture(page, 'recovery-details-' + colorScheme)
    await page.keyboard.press('Escape')
    await page.getByRole('button', { name: /^Downloads/ }).click()
    dialog = page.getByRole('dialog')
    await dialog.getByRole('button', { name: 'Details', exact: true }).last().click()
    await dialog.getByText('Remove task or retained data', { exact: true }).click()
    const remove = dialog.getByRole('button', { name: 'Delete retained result', exact: true })
    await remove.click()
    const confirmation = page.getByRole('dialog', { name: 'Delete retained result', exact: true })
    await expect(confirmation).toBeVisible()
    await expect(confirmation).toHaveCSS('background-color', JADE[colorScheme].paper)
    await expect(confirmation).toHaveCSS('color', JADE[colorScheme].ink)
    await expect(confirmation).toContainText('Verifies ownership')
    await capture(page, 'removal-confirmation-' + colorScheme)
    await page.keyboard.press('Escape')
    await expect(remove).toBeFocused()
    await page.keyboard.press('Escape')
  }
})
