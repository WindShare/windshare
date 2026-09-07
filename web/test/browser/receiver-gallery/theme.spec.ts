import { expect, test } from '@playwright/test'
import { capture, CURRENT_TASK, expectNoHorizontalOverflow, galleryEvidence, GALLERY_PATH, JADE, showScenario } from './assertions'
import { LONG_NAME } from './fixtures'

test('live system appearance preserves selection, task progress, modal identity and keyboard focus', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' })
  await page.goto(GALLERY_PATH)
  for (const scenario of ['folder', 'exact-progress'] as const) {
    await showScenario(page, scenario)
    await page.getByRole('button', { name: 'Select items', exact: true }).click()
    const selected = page.getByRole('checkbox', { name: 'Select Summer photos', exact: true })
    await expect(selected).toHaveAttribute('aria-checked', 'mixed')
    await selected.focus()
    await page.keyboard.press('Space')
    await expect(selected).toBeChecked()
    const task = page.locator(CURRENT_TASK)
    const progress = task.getByRole('progressbar')
    const value = await progress.getAttribute('value')
    if (scenario === 'folder') expect(value).toBeNull()
    else expect(Number(value)).toBeGreaterThan(0)
    const taskNode = await task.elementHandle()
    const downloads = page.getByRole('button', { name: /^Downloads/ })
    await downloads.click()
    const dialog = page.getByRole('dialog')
    const dialogNode = await dialog.elementHandle()
    const back = dialog.getByRole('button', { name: 'Close downloads', exact: true })
    await back.focus()
    const before = await galleryEvidence(page)
    for (const colorScheme of ['dark', 'light'] as const) {
      await page.emulateMedia({ colorScheme })
      await expect(page.locator('.receiver-shell')).toHaveCSS('background-color', JADE[colorScheme].page)
      await expect(dialog).toHaveCSS('background-color', JADE[colorScheme].paper)
      await expect(dialog).toHaveCSS('color', JADE[colorScheme].ink)
      await expect(back).toHaveCSS('color', JADE[colorScheme].ink)
      await expect(dialog.getByRole('button', { name: 'Details', exact: true }).last()).toHaveCSS('color', JADE[colorScheme].ink)
      expect(await taskNode!.evaluate(element => element.isConnected)).toBe(true)
      expect(await dialogNode!.evaluate(element => element.isConnected && (element as HTMLDialogElement).open)).toBe(true)
      await expect(back).toBeFocused()
      await expect(selected).toBeChecked()
      expect(await galleryEvidence(page)).toEqual(before)
      expect(await progress.getAttribute('value')).toBe(value)
      await capture(page, 'receiver-downloads-' + scenario + '-' + colorScheme)
    }
    await page.emulateMedia({ reducedMotion: 'reduce' })
    expect(await page.locator('.receiver-shell, .receiver-shell *, .detail-sheet, .detail-sheet *').evaluateAll(elements =>
      elements.every(element => {
        const style = getComputedStyle(element)
        return style.animationName === 'none' && style.transitionDuration.split(',').every(duration => parseFloat(duration) === 0)
      }))).toBe(true)
    await page.keyboard.press('Escape')
    await expect(downloads).toBeFocused()
    const pause = task.getByRole('button', { name: 'Pause', exact: true })
    await pause.focus()
    await page.keyboard.press('Enter')
    expect((await galleryEvidence(page)).intents).toContain('task-action')
  }
})

test('Jade content and task actions reflow at narrow widths, large text and short height', async ({ page }) => {
  await page.goto(GALLERY_PATH)
  for (const [width, height, fontSize, colorScheme] of [
    [320, 568, '16px', 'light'], [390, 500, '32px', 'dark'], [390, 500, '32px', 'light'],
  ] as const) {
    await page.setViewportSize({ width, height })
    await page.emulateMedia({ colorScheme, reducedMotion: 'reduce' })
    await page.evaluate(size => { document.documentElement.style.fontSize = size }, fontSize)
    await showScenario(page, 'long-names')
    await expectNoHorizontalOverflow(page)
    const longName = page.getByRole('button', { name: LONG_NAME, exact: true })
    await longName.scrollIntoViewIfNeeded()
    await expect(longName).toBeInViewport()
    expect(await longName.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
    await longName.click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    expect(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
    await page.keyboard.press('Escape')
    await expect(longName).toBeFocused()
    await showScenario(page, 'full-directory')
    const contents = page.getByRole('list', { name: 'Folder contents' })
    await expect(contents.locator('.explorer-row')).toHaveCount(256)
    await contents.focus()
    await page.keyboard.press('Control+End')
    await expect(page.getByRole('button', { name: 'Document 256.txt', exact: true })).toBeInViewport()
    const pause = page.locator(CURRENT_TASK).getByRole('button', { name: 'Pause', exact: true })
    await pause.scrollIntoViewIfNeeded()
    await expect(pause).toBeInViewport()
    await pause.click()
    expect((await galleryEvidence(page)).intents).toContain('task-action')
    await expectNoHorizontalOverflow(page)
    await capture(page, 'reflow-' + width + '-' + fontSize + '-' + colorScheme, true)
    await page.locator(CURRENT_TASK).getByRole('button', { name: 'Details', exact: true }).click()
    const details = page.getByRole('dialog')
    expect(await details.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
    await capture(page, 'reflow-details-' + width + '-' + fontSize + '-' + colorScheme)
    await page.keyboard.press('Escape')
  }
})

test('dark media remains uncropped and unsupported previews retain an immediate download action', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' })
  await page.goto(GALLERY_PATH)
  for (const [width, height] of [[1024, 900], [360, 740]] as const) {
    await page.setViewportSize({ width, height })
    for (const scenario of ['folder', 'portrait', 'landscape', 'video', 'unsupported'] as const) {
      await showScenario(page, scenario)
      if (scenario === 'portrait' || scenario === 'landscape') {
        await expect(page.getByRole('img', { name: /^Preview of/ })).toHaveCSS('object-fit', 'contain')
      }
      if (scenario === 'video') {
        await page.getByRole('button', { name: 'Preview a frame', exact: true }).click()
        await expect(page.locator('.preview-frame-stage')).toHaveAttribute('aria-busy', 'false')
        const seek = page.getByRole('slider', { name: 'Seek Summer afternoon.mp4' })
        await seek.focus()
        await page.keyboard.press('ArrowRight')
        expect((await galleryEvidence(page)).intents.some(intent => intent.startsWith('seek:'))).toBe(true)
      }
      if (scenario === 'unsupported') {
        await page.getByRole('button', { name: 'Preview', exact: true }).click()
        await expect(page.getByRole('alert')).toContainText('cannot be previewed')
        const download = page.getByRole('button', { name: 'Download file', exact: true })
        await expect(download).toBeEnabled()
        await download.click()
        expect((await galleryEvidence(page)).intents.some(intent => intent.startsWith('choose:'))).toBe(true)
      }
      await expectNoHorizontalOverflow(page)
      await capture(page, 'dark-' + width + '-' + scenario, true)
    }
  }
})
