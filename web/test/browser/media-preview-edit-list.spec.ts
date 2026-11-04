import { expect, test } from '@playwright/test'
import { presentSegments } from './media-preview-edit-list-probe'

// Chromium only: it checks that the accepted frame's mediaTime is the sample's
// raw composition time. Firefox rebases the one-sample segment to zero and
// reports mediaTime 0, so media-preview-edit-list.cross-browser.spec.ts checks
// presentation alone in every browser.

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
  // Open GOP whose last decode-order samples are the 1.2 s keyframe and its
  // leading frames: mp4box's time-based seek skipped the track there.
  { fixture: 'h264-open-gop-leading-tail.mp4', targets: [0, 1.2], keyframes: [0, 1.2] },
  // Edit list hides the only keyframe: it is shown at position 0.
  { fixture: 'h264-head-trimmed-single-keyframe.mp4', targets: [0, 1], keyframes: [0, 0] },
] as const
// Browser media pipelines round rational sample times to microseconds.
const MEDIA_TIME_TOLERANCE_SECONDS = 1e-5

for (const { fixture, targets, keyframes } of CASES) {
  test(`presents the keyframe segmentAt reports for ${fixture}`, async ({ page }) => {
    const results = await presentSegments(page, fixture, targets)
    for (const [index, observation] of results.entries()) {
      const context = JSON.stringify(observation)
      expect(observation.outcome, context).toBe('presented')
      expect(observation.positionSeconds, context).toBeCloseTo(keyframes[index] ?? Number.NaN, 6)
      expect(observation.mediaTime, context).not.toBeNull()
      expect(Math.abs((observation.mediaTime ?? Number.NaN) - observation.frameSeconds), context)
        .toBeLessThanOrEqual(MEDIA_TIME_TOLERANCE_SECONDS)
    }
  })
}
