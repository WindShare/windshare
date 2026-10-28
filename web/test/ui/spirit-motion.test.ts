import { describe, expect, it } from 'vitest'
import { createSpiritMotion, pointerPose } from '../../src/ui/portal/spirit/motion'

const STEP_SECONDS = 1 / 120
const STEP_MILLISECONDS = 1000 * STEP_SECONDS

function settle(motion: ReturnType<typeof createSpiritMotion>, from = 0) {
  let now = from + STEP_MILLISECONDS
  let frame = motion.advance(STEP_SECONDS, now)
  for (let count = 0; frame.moving && count < 300; count++) {
    now += STEP_MILLISECONDS
    frame = motion.advance(STEP_SECONDS, now)
  }
  return frame
}

describe('wind spirit motion', () => {
  it('responds promptly, then returns home even if the pointer never leaves', () => {
    const motion = createSpiritMotion()
    motion.follow({ x: 12, y: -6, tilt: 4 }, 0)
    const response = motion.advance(0.1, 100)
    expect(response.pose.x).toBeGreaterThan(6)
    expect(response.pose.x).toBeLessThan(12)
    expect(response.pose.y).toBeLessThan(-3)
    expect(settle(motion, 100)).toEqual({ pose: { x: 0, y: 0, tilt: 0 }, moving: false })
  })

  it('preserves the response across refresh rates', () => {
    const poses = [30, 60, 120].map(fps => {
      const motion = createSpiritMotion()
      motion.follow({ x: 12, y: -6, tilt: 4 }, 0)
      let frame = motion.advance(0, 0)
      for (let step = 1; step <= fps / 5; step++) frame = motion.advance(1 / fps, step * 1000 / fps)
      return frame.pose
    })
    const reference = poses[0]!
    for (const pose of poses) {
      expect(pose.x).toBeCloseTo(reference.x, 10)
      expect(pose.y).toBeCloseTo(reference.y, 10)
      expect(pose.tilt).toBeCloseTo(reference.tilt, 10)
    }
  })

  it('releases attention immediately on leaving and can clear motion on suspension', () => {
    const motion = createSpiritMotion()
    motion.follow({ x: 12, y: 0, tilt: 4 }, 0)
    motion.advance(0.1, 100)
    motion.release()
    expect(motion.advance(0.2, 300).pose.x).toBeLessThan(6)
    expect(motion.reset()).toEqual({ x: 0, y: 0, tilt: 0 })
    expect(motion.advance(60, 60_300).moving).toBe(false)
  })

  it('keeps repeated taps bounded and settles without queued animations', () => {
    const motion = createSpiritMotion()
    for (let count = 0; count < 100; count++) {
      motion.tap()
      const frame = motion.advance(STEP_SECONDS, count * STEP_MILLISECONDS)
      expect(Math.abs(frame.pose.y)).toBeLessThan(30)
      expect(Math.abs(frame.pose.tilt)).toBeLessThan(12)
    }
    expect(settle(motion, 1000).moving).toBe(false)
  })

  it('scales travel with the artwork and bounds distant pointers', () => {
    const desktop = { left: 0, top: 0, width: 176, height: 176 }
    const compact = { left: 0, top: 0, width: 88, height: 88 }
    expect(pointerPose(88, 88, desktop)).toEqual({ x: 0, y: 0, tilt: 0 })
    const full = pointerPose(176, 176, desktop)
    const half = pointerPose(88, 88, compact)
    expect(half.x).toBe(full.x / 2)
    expect(half.y).toBe(full.y / 2)
    expect(half.tilt).toBe(full.tilt)
    expect(pointerPose(1e6, 1e6, desktop)).toEqual(pointerPose(1e3, 1e3, desktop))
  })
})
