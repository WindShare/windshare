import { expect, test, type Locator } from '@playwright/test'

const CLOCK_INSTALL_TIME = new Date('2026-01-01T00:00:00Z')
const CLOCK_PAUSE_TIME = new Date('2026-01-01T01:00:00Z')

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.clock.install({ time: CLOCK_INSTALL_TIME })
  // Freeze before navigation so startup and driver latency cannot advance expressions.
  await page.clock.pauseAt(CLOCK_PAUSE_TIME)
  await page.goto('/')
})

async function center(element: Locator) {
  const bounds = (await element.boundingBox())!
  return { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 }
}

async function face(artwork: Locator) {
  return artwork.evaluate(element => {
    const style = getComputedStyle(element)
    return { openness: Number(style.getPropertyValue('--spirit-eye-openness')), smile: Number(style.getPropertyValue('--spirit-smile')) }
  })
}

test('the spirit follows promptly, acknowledges a pause, and returns to its stable hit target', async ({ page }) => {
  const button = page.getByRole('figure').getByRole('button')
  const artwork = button.getByRole('img', { includeHidden: true })
  await expect(button).toBeVisible()
  await button.click({ trial: true })
  const bounds = (await button.boundingBox())!
  const home = await center(button)
  await page.mouse.move(home.x + bounds.width * 0.45, home.y)
  await page.clock.runFor(150)
  expect((await center(artwork)).x - home.x).toBeGreaterThan(3)
  expect((await center(artwork)).x - home.x).toBeLessThan(bounds.width / 4)
  expect(await button.boundingBox()).toEqual(bounds)
  await page.clock.runFor(330)
  expect((await face(artwork)).openness).toBeLessThan(0.3)
  await page.clock.runFor(2000)
  expect(await face(artwork)).toEqual({ openness: 1, smile: 0 })
  expect(Math.abs((await center(artwork)).x - home.x)).toBeLessThan(0.1)
  expect(await button.boundingBox()).toEqual(bounds)
})

test('keyboard play respects reduced motion, including a live preference change', async ({ page }) => {
  const button = page.getByRole('figure').getByRole('button')
  const artwork = button.getByRole('img', { includeHidden: true })
  await expect(button).toBeVisible()
  await button.click({ trial: true })
  const home = await center(button)
  await button.focus()
  await page.keyboard.press('Enter')
  await page.clock.runFor(100)
  expect((await center(artwork)).y).toBeLessThan(home.y - 4)
  expect((await face(artwork)).smile).toBeGreaterThan(0.95)
  await expect(button).toBeFocused()
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await page.clock.runFor(100)
  const resting = await artwork.boundingBox()
  await page.mouse.move(home.x + 60, home.y)
  await page.keyboard.press('Enter')
  await page.clock.runFor(1000)
  expect(await artwork.boundingBox()).toEqual(resting)
  expect(await face(artwork)).toEqual({ openness: 1, smile: 0 })
  await expect(button).toBeFocused()
})

test.describe('touch interaction', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true })

  test('taps give bounded feedback without following touch movements', async ({ page }) => {
    const button = page.getByRole('figure').getByRole('button')
    const artwork = button.getByRole('img', { includeHidden: true })
    await expect(button).toBeVisible()
    await button.click({ trial: true })
    const home = await center(button)
    await button.tap()
    await page.clock.runFor(100)
    expect((await center(artwork)).y).toBeLessThan(home.y - 4)
    expect((await face(artwork)).smile).toBeGreaterThan(0.95)
    await page.clock.runFor(2000)
    expect(Math.abs((await center(artwork)).x - home.x)).toBeLessThan(0.1)
    expect(await center(button)).toEqual(home)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true)
  })
})
