import { describe, expect, it } from 'vitest'
import { createSpiritMotion, pointerPose } from '../../src/ui/portal/spirit/motion'

const STEP_SECONDS = 1 / 120

function settle(motion: ReturnType<typeof createSpiritMotion>) {
  let frame = motion.advance(STEP_SECONDS)
  for (let count = 0; frame.moving && count < 300; count++) frame = motion.advance(STEP_SECONDS)
  return frame
}

describe('wind spirit motion', () => {
  it('responds promptly and settles when released', () => {
    const motion = createSpiritMotion()
    motion.follow({ x: 12, y: -6, tilt: 4 })
    const response = motion.advance(0.1)
    expect(response.pose.x).toBeGreaterThan(6)
    expect(response.pose.x).toBeLessThan(12)
    expect(response.pose.y).toBeLessThan(-3)
    motion.release()
    expect(settle(motion)).toEqual({ pose: { x: 0, y: 0, tilt: 0 }, moving: false })
  })

  it('preserves the response across refresh rates', () => {
    const poses = [30, 60, 120].map(fps => {
      const motion = createSpiritMotion()
      motion.follow({ x: 12, y: -6, tilt: 4 })
      let frame = motion.advance(0)
      for (let step = 1; step <= fps / 5; step++) frame = motion.advance(1 / fps)
      return frame.pose
    })
    const reference = poses[0]!
    for (const pose of poses) {
      expect(pose.x).toBeCloseTo(reference.x, 10)
      expect(pose.y).toBeCloseTo(reference.y, 10)
      expect(pose.tilt).toBeCloseTo(reference.tilt, 10)
    }
  })

  it('releases immediately and can clear motion on suspension', () => {
    const motion = createSpiritMotion()
    motion.follow({ x: 12, y: 0, tilt: 4 })
    motion.advance(0.1)
    motion.release()
    expect(motion.advance(0.2).pose.x).toBeLessThan(6)
    expect(motion.reset()).toEqual({ x: 0, y: 0, tilt: 0 })
    expect(motion.advance(60).moving).toBe(false)
  })

  it('keeps repeated taps bounded and settles without queued animations', () => {
    const motion = createSpiritMotion()
    for (let count = 0; count < 100; count++) {
      motion.tap()
      const frame = motion.advance(STEP_SECONDS)
      expect(Math.abs(frame.pose.y)).toBeLessThan(30)
      expect(Math.abs(frame.pose.tilt)).toBeLessThan(12)
    }
    expect(settle(motion).moving).toBe(false)
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
