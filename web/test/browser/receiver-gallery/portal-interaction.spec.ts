import { expect, test } from '@playwright/test'

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('windshare.ui.preferences',
    JSON.stringify({ language: 'zh-CN', appearance: 'system', motion: 'system' })))
})

test('keyboard tabs preserve a receive draft and keep one tab in the tab order', async ({ page }) => {
  await page.goto('/')
  const input = page.getByRole('textbox', { name: '分享链接或密钥' })
  await input.fill('draft-share-link')
  const tabs = page.getByRole('tab')
  await tabs.first().focus()
  await page.keyboard.press('ArrowRight')
  await expect(tabs.nth(1)).toBeFocused()
  await expect(tabs.nth(1)).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('End')
  await expect(tabs.last()).toBeFocused()
  await page.keyboard.press('ArrowRight')
  await expect(tabs.first()).toBeFocused()
  await expect(input).toHaveValue('draft-share-link')
  await expect(page.getByRole('tab').and(page.locator('[tabindex="0"]'))).toHaveCount(1)
  await expect(page.getByRole('button', { name: '接收文件', exact: true })).toBeEnabled()
})

test('clipboard failures stay actionable and copying reports success only after resolution', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
      readText: async () => { throw new DOMException('Denied', 'NotAllowedError') },
      writeText: async () => { throw new DOMException('Denied', 'NotAllowedError') },
    } })
  })
  await page.goto('/')
  const input = page.getByRole('textbox', { name: '分享链接或密钥' })
  await page.getByRole('button', { name: '从剪贴板粘贴' }).click()
  await expect(input).toBeFocused()
  await expect(input).toBeEmpty()
  await expect(page.getByRole('tabpanel').getByRole('status')).toContainText('无法读取剪贴板')
  await expect(page.getByRole('button', { name: '接收文件', exact: true })).toBeDisabled()

  await page.getByRole('tab', { name: '命令行分享' }).click()
  await page.getByRole('button', { name: '复制命令' }).click()
  await expect(page.getByRole('tabpanel').getByRole('status')).toContainText('复制失败')
  await expect(page.getByRole('button', { name: '已复制', exact: true })).toHaveCount(0)
  let copied = ''
  await page.exposeFunction('capturePortalCopy', (text: string) => { copied = text })
  await page.evaluate(() => {
    navigator.clipboard.writeText = text =>
      (window as typeof window & { capturePortalCopy(text: string): Promise<void> }).capturePortalCopy(text)
  })
  await page.getByRole('button', { name: '重试复制' }).click()
  await expect(page.getByRole('button', { name: '已复制', exact: true })).toBeVisible()
  expect(copied).toBe('wind share ./My-Project-Folder')
})

test('a delayed clipboard response cannot overwrite a newer receive draft', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
      readText: () => new Promise<string>(resolve => {
        Object.assign(window, { finishPortalPaste: resolve })
      }),
    } })
  })
  await page.goto('/')
  const paste = page.getByRole('button', { name: '从剪贴板粘贴' })
  const input = page.getByRole('textbox', { name: '分享链接或密钥' })
  await paste.click()
  await expect(paste).toBeDisabled()
  await input.fill('newer-manual-draft')
  await page.evaluate(() => {
    (window as typeof window & { finishPortalPaste(text: string): void }).finishPortalPaste('older-clipboard-link')
  })
  await expect(input).toHaveValue('newer-manual-draft')
  await expect(paste).toBeEnabled()
})

test('mobile navigation opens the share guide even when its anchor is already current', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/')
  const menu = page.getByLabel('打开导航')
  const navigation = page.getByRole('navigation', { name: '移动端导航' })
  const shareTab = page.getByRole('tab', { name: '命令行分享' })
  for (let visit = 0; visit < 2; visit++) {
    await menu.click()
    await navigation.getByRole('link', { name: '开始分享', exact: true }).click()
    await expect(shareTab).toHaveAttribute('aria-selected', 'true')
    await expect(navigation).toBeHidden()
    expect(new URL(page.url()).hash).toBe('#cli')
    await page.getByRole('tab', { name: '接收文件', exact: true }).click()
  }
  await menu.click()
  await page.keyboard.press('Escape')
  await expect(navigation).toBeHidden()
  await expect(menu).toBeFocused()
})
