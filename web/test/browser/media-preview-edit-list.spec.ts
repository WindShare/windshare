import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

// Chromium-only on purpose: Firefox has a pre-existing bug where every non-zero
// seek of a one-sample preview segment never presents, so this contract is not
// named *.cross-browser.spec.ts.
const FIXTURE_DIRECTORY = '/test/fixtures/preview'
// Each target must present the keyframe segmentAt reports. See
// test/preview/mp4-range.test.ts for how each fixture was encoded.
const CASES = [
  // B-frame shift in the edit list: keyframes at 0 s and 1 s; 2 s clamps to the
  // last keyframe because the duration ends after the last decoded sample.
  { fixture: 'h264-bframes-edit-list.mp4', targets: [0, 1, 2], keyframes: [0, 1, 1] },
  // Leading empty edit (`-itsoffset 0.5`): keyframes at 0.5 s and 1.5 s.
  { fixture: 'h264-leading-empty-edit.mp4', targets: [0, 1.5, 2], keyframes: [0.5, 1.5, 1.5] },
  // Head-trimmed copy (`-ss 0.5 -c copy`): the keyframe before the cut is at
  // -0.5 s, so 0 s must use the first keyframe on the timeline, 0.5 s.
  { fixture: 'h264-head-trimmed.mp4', targets: [0, 1.5], keyframes: [0.5, 1.5] },
  // 29.97 fps head-trimmed copy (`-ss 0.685 -c copy`): the first visible
  // keyframe is at 7478/30000 s, where (cts / timescale) * timescale < cts.
  { fixture: 'h264-ntsc-head-trimmed-float.mp4', targets: [0, 1.2], keyframes: [7_478 / 30_000, 35_506 / 30_000] },
  // 600-tick track with `-itsoffset 0.1234`: the 133 ms empty edit is 80 ticks,
  // so keyframes are at 80/600 s and 680/600 s, not 0.133 s and 1.133 s.
  { fixture: 'h264-ts600-empty-edit.mp4', targets: [0, 1.14, 2], keyframes: [80 / 600, 680 / 600, 680 / 600] },
  // Head-trimmed copy (`-ss 0.5 -c copy`, 30 fps): 1.5 s lands exactly on a
  // keyframe with a nonzero edit offset.
  { fixture: 'h264-head-trimmed-on-keyframe.mp4', targets: [0, 1.5], keyframes: [0.5, 1.5] },
  // Head-trimmed copy whose cut (`-ss 0.77`) is not on a frame boundary.
  { fixture: 'h264-head-trimmed-mid-frame.mp4', targets: [0, 1.24], keyframes: [3_533 / 15_360, 18_893 / 15_360] },
  // 29.97 fps with timescale 30000: keyframes at 0, 1.001 s and 2.002 s.
  { fixture: 'h264-ntsc-30000.mp4', targets: [0, 1.001, 2.002], keyframes: [0, 1.001, 2.002] },
] as const
const PRESENTATION_TIMEOUT_MS = 5_000
// Browser media pipelines round rational sample times to microseconds.
const MEDIA_TIME_TOLERANCE_SECONDS = 1e-5

for (const { fixture, targets, keyframes } of CASES) {
  test(`presents the keyframe segmentAt reports for ${fixture}`, async ({ page }) => {
    await page.goto(BROWSER_CONTRACT_HOST_PATH)
    const results = await page.evaluate(async ({ fixturePath, targets, timeoutMs }) => {
      const mp4RangePath = '/src/preview/mp4-range.ts'
      const presentationPath = '/src/ui/share-content/video-frame-presentation.ts'
      const [mp4RangeModule, presentationModule] = await Promise.all([
        import(mp4RangePath) as Promise<typeof import('../../src/preview/mp4-range')>,
        import(presentationPath) as Promise<typeof import('../../src/ui/share-content/video-frame-presentation')>,
      ])

      const response = await fetch(fixturePath)
      if (!response.ok) throw new Error(`Fixture returned HTTP ${response.status}`)
      const bytes = new Uint8Array(await response.arrayBuffer())
      const source = {
        exactSize: BigInt(bytes.byteLength),
        async read(range: { readonly start: bigint; readonly end: bigint }, signal: AbortSignal) {
          signal.throwIfAborted()
          return bytes.slice(Number(range.start), Number(range.end))
        },
      }
      const signal = new AbortController().signal
      const preview = await mp4RangeModule.Mp4RangePreview.open(source, signal)

      const observations = []
      for (const target of targets) {
        const segment = await preview.segmentAt(target, signal)
        const url = URL.createObjectURL(segment.bytes)
        const video = document.createElement('video')
        video.muted = true
        video.playsInline = true
        video.preload = 'auto'
        video.hidden = true
        let lastMediaTime: number | undefined
        // Production observer, real element: only the frame callback is wrapped so
        // the test can read the mediaTime of the frame the observer accepted.
        const decoder = {
          get currentTime() { return video.currentTime },
          set currentTime(value: number) { video.currentTime = value },
          get readyState() { return video.readyState },
          HAVE_METADATA: video.HAVE_METADATA,
          addEventListener: video.addEventListener.bind(video),
          removeEventListener: video.removeEventListener.bind(video),
          cancelVideoFrameCallback: video.cancelVideoFrameCallback.bind(video),
          requestVideoFrameCallback: (callback: VideoFrameRequestCallback) =>
            video.requestVideoFrameCallback((now, metadata) => {
              lastMediaTime = metadata.mediaTime
              callback(now, metadata)
            }),
        } as Parameters<typeof presentationModule.observeVideoFrame>[0]
        const outcome = await new Promise<'presented' | 'failed' | 'timeout'>((resolve) => {
          let close = () => {}
          const timer = setTimeout(() => { close(); resolve('timeout') }, timeoutMs)
          close = presentationModule.observeVideoFrame(
            decoder,
            segment.positionSeconds,
            () => { clearTimeout(timer); resolve('presented') },
            () => { clearTimeout(timer); resolve('failed') },
          )
          document.body.append(video)
          video.src = url
        })
        observations.push({
          target,
          outcome,
          positionSeconds: segment.positionSeconds,
          mediaTime: lastMediaTime ?? null,
          currentTime: video.currentTime,
        })
        video.removeAttribute('src')
        video.load()
        video.remove()
        URL.revokeObjectURL(url)
      }
      return observations
    }, { fixturePath: `${FIXTURE_DIRECTORY}/${fixture}`, targets, timeoutMs: PRESENTATION_TIMEOUT_MS })

    for (const [index, observation] of results.entries()) {
      const context = JSON.stringify(observation)
      expect(observation.outcome, context).toBe('presented')
      expect(observation.positionSeconds, context).toBeCloseTo(keyframes[index] ?? Number.NaN, 6)
      expect(observation.mediaTime, context).not.toBeNull()
      expect(Math.abs((observation.mediaTime ?? Number.NaN) - observation.positionSeconds), context)
        .toBeLessThanOrEqual(MEDIA_TIME_TOLERANCE_SECONDS)
    }
  })
}
