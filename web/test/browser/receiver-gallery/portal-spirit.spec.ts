import { expect, test, type Locator } from '@playwright/test'

async function center(element: Locator) {
  const bounds = (await element.boundingBox())!
  return { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 }
}

test('the spirit follows promptly, keeps a stable hit target, and rests under a stationary pointer', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.clock.install()
  await page.goto('/')
  const button = page.getByRole('figure').getByRole('button')
  const artwork = button.locator('img')
  await expect(button).toBeVisible()
  await button.click({ trial: true })
  const bounds = (await button.boundingBox())!
  const home = await center(button)
  await page.mouse.move(home.x + bounds.width * 0.45, home.y)
  await page.clock.runFor(150)
  expect((await center(artwork)).x - home.x).toBeGreaterThan(3)
  expect((await center(artwork)).x - home.x).toBeLessThan(bounds.width / 4)
  expect(await button.boundingBox()).toEqual(bounds)
  await page.clock.runFor(2000)
  expect(Math.abs((await center(artwork)).x - home.x)).toBeLessThan(0.1)
  expect(await button.boundingBox()).toEqual(bounds)
})

test('keyboard play respects reduced motion, including a live preference change', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.clock.install()
  await page.goto('/')
  const button = page.getByRole('figure').getByRole('button')
  const artwork = button.locator('img')
  await expect(button).toBeVisible()
  await button.click({ trial: true })
  const home = await center(button)
  await button.focus()
  await page.keyboard.press('Enter')
  await page.clock.runFor(100)
  expect((await center(artwork)).y).toBeLessThan(home.y - 4)
  await expect(button).toBeFocused()
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await page.clock.runFor(100)
  const resting = await artwork.boundingBox()
  await page.mouse.move(home.x + 60, home.y)
  await page.keyboard.press('Enter')
  await page.clock.runFor(1000)
  expect(await artwork.boundingBox()).toEqual(resting)
  await expect(button).toBeFocused()
})

test.describe('touch interaction', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true })

  test('taps give bounded feedback without following touch movements', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'no-preference' })
    await page.clock.install()
    await page.goto('/')
    const button = page.getByRole('figure').getByRole('button')
    const artwork = button.locator('img')
    await expect(button).toBeVisible()
    await button.click({ trial: true })
    const home = await center(button)
    await button.tap()
    await page.clock.runFor(100)
    expect((await center(artwork)).y).toBeLessThan(home.y - 4)
    await page.clock.runFor(2000)
    expect(Math.abs((await center(artwork)).x - home.x)).toBeLessThan(0.1)
    expect(await center(button)).toEqual(home)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true)
  })
})
