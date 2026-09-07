import { expect, test, type Page } from '@playwright/test'
import { capture, expectNoHorizontalOverflow, galleryEvidence, GALLERY_PATH, showScenario } from './assertions'

interface VideoGallery {
  windshareHoldVideoSeeks(): void
  windshareCompleteVideoSeek(seconds: number, decoderUrl?: string): Promise<void>
}

async function openVideo(page: Page) {
  await page.goto(GALLERY_PATH)
  await showScenario(page, 'video')
  await page.getByRole('button', { name: 'Preview a frame', exact: true }).click()
  await expect.poll(async () => (await galleryEvidence(page)).intents).toContain('presented:2')
  await page.evaluate(() => (window as unknown as VideoGallery).windshareHoldVideoSeeks())
}

async function visibleFrame(page: Page) {
  return page.locator('canvas.preview-frame').evaluate((canvas: HTMLCanvasElement) => {
    const rect = canvas.getBoundingClientRect()
    return { pixels: canvas.toDataURL(), x: rect.x, y: rect.y, width: rect.width, height: rect.height }
  })
}

async function seeks(page: Page) {
  return (await galleryEvidence(page)).intents.filter(intent => intent.startsWith('seek:'))
}

for (const viewport of [{ width: 1024, height: 900 }, { width: 360, height: 740 }]) {
  test(`video scrubbing retains its frame and layout through network and decoder waits at ${viewport.width}px`, async ({ page }) => {
    await page.setViewportSize(viewport)
    await openVideo(page)
    const slider = page.getByRole('slider', { name: 'Seek Summer afternoon.mp4' })
    await slider.scrollIntoViewIfNeeded()
    const original = await visibleFrame(page)
    const controls = await slider.boundingBox()
    expect(controls).not.toBeNull()
    expect(original.height).toBeGreaterThan(100)
    const { x, y, width, height } = controls!
    await page.mouse.move(x + width * 0.1, y + height / 2)
    await page.mouse.down()
    await page.mouse.move(x + width * 0.7, y + height / 2, { steps: 8 })
    await expect(slider).toHaveValue('0.2')
    expect(await seeks(page)).toEqual([])
    await page.mouse.up()
    await expect.poll(() => seeks(page)).toEqual(['seek:0.2'])
    await expect(slider).toBeFocused()
    await expect(page.locator('.preview-frame-status')).toHaveText('Loading frame…')
    expect(await visibleFrame(page)).toEqual(original)

    // Hold the new decoder response after the range request has settled. This
    // exposes the intrinsic-size collapse that fast cached fixtures used to hide.
    const segment = await page.locator('video').evaluate(async (video: HTMLVideoElement) => {
      const blob = await fetch(video.currentSrc).then(response => response.blob())
      return { bytes: [...new Uint8Array(await blob.arrayBuffer())], type: blob.type }
    })
    let deliver!: () => void
    const delivery = new Promise<void>(resolve => { deliver = resolve })
    await page.route('**/held-video-segment', async route => {
      await delivery
      await route.fulfill({ status: 206, body: Buffer.from(segment.bytes), contentType: segment.type,
        headers: { 'Accept-Ranges': 'bytes', 'Content-Range': `bytes 0-${segment.bytes.length - 1}/${segment.bytes.length}` } })
    })
    try {
      const request = page.waitForRequest('**/held-video-segment')
      await page.evaluate(() => (window as unknown as VideoGallery).windshareCompleteVideoSeek(0.1, '/held-video-segment'))
      await request
      expect(await visibleFrame(page)).toEqual(original)
      expect(await slider.boundingBox()).toEqual(controls)
      await expect(slider).toHaveValue('0.2')
      expect((await galleryEvidence(page)).intents).not.toContain('presented:3')
      await capture(page, 'video-seek-pending-' + viewport.width, true)
      deliver()
      await expect.poll(async () => (await galleryEvidence(page)).intents).toContain('presented:3')
      await expect(page.locator('.preview-frame-status')).toHaveText('')
      const next = await visibleFrame(page)
      expect({ ...next, pixels: original.pixels }).toEqual(original)
      expect(await page.locator('video').evaluate((video: HTMLVideoElement) => video.currentTime)).toBeCloseTo(0.1)
      expect(next.pixels !== original.pixels, 'The new decoded frame must replace the retained pixels').toBe(true)
      expect(await slider.boundingBox()).toEqual(controls)
      await expect(slider).toHaveValue('0.2')
      await expect(slider).toBeFocused()
      await expectNoHorizontalOverflow(page)
      await capture(page, 'video-seek-ready-' + viewport.width, true)
    } finally {
      deliver()
    }
  })
}

test('keyboard seeks, cancelled drags and closing a pending frame keep their intent boundaries', async ({ page }) => {
  await openVideo(page)
  const slider = page.getByRole('slider', { name: 'Seek Summer afternoon.mp4' })
  await slider.focus()
  await page.keyboard.press('ArrowRight')
  await expect(slider).toHaveValue('0.1')
  expect(await seeks(page)).toEqual(['seek:0.1'])
  const bounds = (await slider.boundingBox())!
  await page.mouse.move(bounds.x + bounds.width / 3, bounds.y + bounds.height / 2)
  await page.mouse.down()
  await slider.fill('0.2')
  await slider.dispatchEvent('pointercancel', { pointerId: 1 })
  await page.mouse.up()
  await expect(slider).toHaveValue('0.1')
  expect(await seeks(page)).toEqual(['seek:0.1'])
  await page.getByRole('button', { name: 'Close preview', exact: true }).click()
  await expect(page.getByRole('region', { name: 'File preview' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Preview a frame', exact: true })).toBeVisible()
})
