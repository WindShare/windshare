import { expect, test } from '@playwright/test'

test('browser language is the default and a chosen language persists without losing the receive draft', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await page.getByRole('textbox', { name: 'Share link or key' }).fill('a-link-in-progress')
  await page.getByLabel('Appearance & language').click()
  await page.getByLabel('Language', { exact: true }).selectOption('zh-CN')
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
  await expect(page.getByRole('textbox', { name: '分享链接或密钥' })).toHaveValue('a-link-in-progress')
  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
  await expect(page.getByRole('textbox', { name: '分享链接或密钥' })).toBeVisible()
})

test('system appearance updates live while an explicit choice persists and styles Downloads', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' })
  await page.goto('/')
  const foreground = () => page.getByRole('main').evaluate(element => getComputedStyle(element).color)
  const dark = await foreground()
  await page.emulateMedia({ colorScheme: 'light' })
  await expect.poll(foreground).not.toBe(dark)
  const light = await foreground()
  await page.getByLabel('Appearance & language').click()
  await page.getByRole('radio', { name: 'Dark', exact: true }).check()
  await expect.poll(foreground).toBe(dark)
  await page.reload()
  await expect.poll(foreground).toBe(dark)
  await page.getByRole('button', { name: 'Downloads', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Downloads', exact: true })
  await expect(dialog).toHaveCSS('color-scheme', 'dark')
  await page.keyboard.press('Escape')
  await page.getByLabel('Appearance & language').click()
  await page.getByRole('radio', { name: 'System', exact: true }).check()
  await expect.poll(foreground).toBe(light)
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect.poll(foreground).toBe(dark)
})

test('system reduced motion disables transitions and animations while keeping the mascot interactive', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.goto('/')
  const runningAnimations = () => page.evaluate(() =>
    document.getAnimations().filter(animation => animation.playState === 'running').length)
  await expect.poll(runningAnimations).toBeGreaterThan(0)
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await expect.poll(runningAnimations).toBe(0)
  // Idle transitions still run on the next interaction; include ancestors of the main landmark.
  await expect.poll(() => page.evaluate(() => {
    for (const element of document.querySelectorAll('*')) {
      for (const pseudo of [null, '::before', '::after']) {
        const style = getComputedStyle(element, pseudo)
        if (style.animationName !== 'none' || style.transitionDuration.split(',').some(duration => parseFloat(duration) !== 0)) {
          return { tag: element.tagName, pseudo, animation: style.animationName, transition: style.transitionDuration }
        }
      }
    }
    return null
  })).toBeNull()
  const mascot = page.getByRole('figure').getByRole('button')
  await mascot.click()
  await expect(mascot).toBeFocused()
  await expect.poll(runningAnimations).toBe(0)
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await expect.poll(runningAnimations).toBeGreaterThan(0)
})

test('both languages keep navigation and settings within narrow viewports', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await page.goto('/')
  for (const width of [900, 390, 320]) {
    await page.setViewportSize({ width, height: 844 })
    for (const language of ['en', 'zh-CN']) {
      const settings = page.getByLabel(/Appearance & language|外观与语言/)
      await settings.click()
      await page.getByRole('combobox').selectOption(language)
      const appearance = await page.getByRole('group', { name: /Appearance|外观/ }).boundingBox()
      expect(appearance!.x).toBeGreaterThanOrEqual(0)
      expect(appearance!.x + appearance!.width).toBeLessThanOrEqual(width)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true)
      await page.keyboard.press('Escape')
      await expect(settings).toBeFocused()
    }
  }
})
