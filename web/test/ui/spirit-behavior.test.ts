import { describe, expect, it } from 'vitest'
import { createSpiritBehavior } from '../../src/ui/portal/spirit/behavior'
import { createSpiritExpression } from '../../src/ui/portal/spirit/expression'

const OPEN_FACE = { openness: 1, smile: 0 }

describe('wind spirit expressions', () => {
  it('sleeps between irregular idle blinks and opens again after a brief closure', () => {
    const expression = createSpiritExpression(() => 0.25)
    expression.resume(0)
    expect(expression.sample(1000)).toEqual({ face: OPEN_FACE, animating: false, wakeAt: 6000 })
    expression.sample(6000)
    expect(expression.sample(6060).face.openness).toBeLessThan(0.1)
    expect(expression.sample(6150)).toEqual({ face: OPEN_FACE, animating: false, wakeAt: 12_150 })
  })

  it('occasionally adds a second blink with an open pause between them', () => {
    const expression = createSpiritExpression(() => 0)
    expression.resume(0)
    expression.sample(5000)
    expect(expression.sample(5060).face.openness).toBeLessThan(0.1)
    expect(expression.sample(5190).face).toEqual(OPEN_FACE)
    expect(expression.sample(5320).face.openness).toBeLessThan(0.1)
    expect(expression.sample(5410)).toEqual({ face: OPEN_FACE, animating: false, wakeAt: 10_410 })
  })

  it('uses fresh randomness for the next idle interval', () => {
    const values = [0, 1, 0.75]
    const expression = createSpiritExpression(() => values.shift()!)
    expression.resume(0)
    expect(expression.sample(0).wakeAt).toBe(5000)
    expression.sample(5000)
    expect(expression.sample(5150).wakeAt).toBe(13_150)
  })

  it('lets smiles replace blinks and repeated taps extend the smile without flicker or a queue', () => {
    const expression = createSpiritExpression(() => 1)
    expression.resume(0)
    expression.blink(100)
    expect(expression.sample(160).face.openness).toBeLessThan(0.1)
    expression.smile(160)
    expect(expression.sample(230).face).toEqual({ openness: 1, smile: 1 })
    expression.blink(240)
    expression.smile(250)
    expect(expression.sample(250).face.smile).toBe(1)
    expect(expression.sample(320).face.smile).toBe(1)
    expect(expression.sample(710)).toEqual({ face: OPEN_FACE, animating: false, wakeAt: 9710 })
  })

  it('discards expressions during suspension and schedules a fresh interval on return', () => {
    const expression = createSpiritExpression(() => 0.5)
    expression.resume(0)
    expression.smile(100)
    expect(expression.suspend()).toEqual(OPEN_FACE)
    expression.blink(200)
    expression.smile(300)
    expect(expression.sample(60_000)).toEqual({ face: OPEN_FACE, animating: false, wakeAt: null })
    expression.resume(60_000)
    expect(expression.sample(60_000).wakeAt).toBe(67_000)
  })
})

describe('wind spirit choreography', () => {
  it('acknowledges a stationary pointer once before returning home', () => {
    const behavior = createSpiritBehavior(() => 1)
    behavior.resume(0)
    behavior.follow({ x: 12, y: 0, tilt: 4 }, 0)
    behavior.advance(0.4, 400)
    behavior.advance(0.02, 420)
    const blink = behavior.advance(0.06, 480)
    expect(blink.face.openness).toBeLessThan(0.1)
    expect(blink.pose.x).toBeGreaterThan(11)
    const returning = behavior.advance(0.12, 600)
    expect(returning.pose.x).toBeLessThan(blink.pose.x)
    const settled = behavior.advance(1.4, 2000)
    expect(settled.pose).toEqual({ x: 0, y: 0, tilt: 0 })
    expect(settled.face).toEqual(OPEN_FACE)
    expect(settled.animating).toBe(false)
    expect(behavior.advance(0, 3000).face).toEqual(OPEN_FACE)
  })

  it('cancels a pending return when the pointer moves again', () => {
    const behavior = createSpiritBehavior(() => 1)
    behavior.resume(0)
    behavior.follow({ x: 12, y: 0, tilt: 4 }, 0)
    behavior.advance(0.42, 420)
    behavior.follow({ x: -12, y: 0, tilt: -4 }, 460)
    expect(behavior.advance(0.2, 660).pose.x).toBeLessThan(-8)
  })

  it('leaves quietly and lets a tap take priority over a pending acknowledgement', () => {
    const behavior = createSpiritBehavior(() => 1)
    behavior.resume(0)
    behavior.follow({ x: 12, y: 0, tilt: 4 }, 0)
    behavior.advance(0.1, 100)
    behavior.release()
    expect(behavior.advance(0.4, 500).face).toEqual(OPEN_FACE)
    behavior.follow({ x: 12, y: 0, tilt: 4 }, 600)
    behavior.tap(700)
    expect(behavior.advance(0.1, 800).face.smile).toBe(1)
    expect(behavior.advance(0.25, 1050).face.openness).toBe(1)
  })

  it('suspends both body and face without replaying old interactions on resume', () => {
    const behavior = createSpiritBehavior(() => 1)
    behavior.resume(0)
    behavior.tap(0)
    behavior.advance(0.1, 100)
    expect(behavior.suspend()).toEqual({
      pose: { x: 0, y: 0, tilt: 0 }, face: OPEN_FACE, animating: false, wakeAt: null,
    })
    behavior.follow({ x: 12, y: 0, tilt: 4 }, 200)
    behavior.tap(300)
    expect(behavior.advance(60, 60_000).animating).toBe(false)
    behavior.resume(60_000)
    expect(behavior.advance(0, 60_000).wakeAt).toBe(69_000)
  })
})
