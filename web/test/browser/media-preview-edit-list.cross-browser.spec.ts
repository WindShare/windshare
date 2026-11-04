import { expect, test } from '@playwright/test'
import { presentSegments } from './media-preview-edit-list-probe'

// Files that showed a frame on main in Firefox or WebKitGTK, which apply edit
// lists differently from Chromium, plus the B-frame shift. Every target must
// present the keyframe segmentAt reports; a stalled decoder fails through the
// production deadline instead of loading forever. Commands are in
// test/preview/mp4-range.test.ts.
const CASES = [
  // `-itsoffset 0.5 -bf 0`: keyframes at 0.5 s and 1.5 s, raw cts 0 and 1 s.
  { fixture: 'h264-leading-empty-edit-no-bframes.mp4', targets: [0, 1.5], keyframes: [0.5, 1.5] },
  // `-bf 0` copied with `-ss 0.5 -c copy`: keyframes at 0.5 s and 1.5 s.
  { fixture: 'h264-head-trimmed-no-bframes.mp4', targets: [0, 1.5], keyframes: [0.5, 1.5] },
  // `-bf 2`: a 0.2 s B-frame shift in the edit list.
  { fixture: 'h264-bframes-edit-list.mp4', targets: [0, 1.5], keyframes: [0, 1] },
] as const

for (const { fixture, targets, keyframes } of CASES) {
  test(`presents a frame for ${fixture} in every browser`, async ({ page }) => {
    const results = await presentSegments(page, fixture, targets)
    for (const [index, observation] of results.entries()) {
      const context = JSON.stringify(observation)
      expect(observation.outcome, context).toBe('presented')
      expect(observation.keyframeSeconds, context).toBeCloseTo(keyframes[index] ?? Number.NaN, 6)
    }
  })
}
