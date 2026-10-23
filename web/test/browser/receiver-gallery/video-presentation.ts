import type { Page } from '@playwright/test'

interface PresentationGate {
  seeked: boolean
  frames: number
  release(): void
}

type GalleryWindow = typeof window & { windshareVideoPresentation: PresentationGate }

export async function holdVideoPresentation(page: Page, frameTimeSeconds: number) {
  await page.evaluate(frameTime => {
    let held = true
    const callbacks: { callback: VideoFrameRequestCallback; now: number; metadata: VideoFrameCallbackMetadata }[] = []
    const gate: PresentationGate = {
      seeked: false,
      frames: 0,
      release() {
        held = false
        for (const entry of callbacks.splice(0)) entry.callback(entry.now, entry.metadata)
      },
    }
    // Keep the real decoder and media events running while delaying delivery of
    // its compositor evidence. This makes premature presentation deterministic.
    const request = HTMLVideoElement.prototype.requestVideoFrameCallback
    HTMLVideoElement.prototype.requestVideoFrameCallback = function (callback) {
      return request.call(this, (now, metadata) => {
        if (!held || metadata.mediaTime < frameTime) { callback(now, metadata); return }
        gate.frames++
        callbacks.push({ callback, now, metadata })
      })
    }
    document.addEventListener('seeked', event => {
      if (event.target instanceof HTMLVideoElement) gate.seeked = true
    }, true)
    Object.assign(window, { windshareVideoPresentation: gate })
  }, frameTimeSeconds)
  return {
    state: () => page.evaluate(() => {
      const gate = (window as GalleryWindow).windshareVideoPresentation
      return { seeked: gate.seeked, frames: gate.frames }
    }),
    release: () => page.evaluate(() => (window as GalleryWindow).windshareVideoPresentation.release()),
  }
}
