const ATTENTION_DURATION_MS = 420
const FOLLOW_RESPONSE = 22
const REST_RESPONSE = 10
const HORIZONTAL_TRAVEL_RATIO = 0.09
const VERTICAL_TRAVEL_RATIO = 0.06
const MAX_TILT_DEGREES = 6
const TAP_LIFT_SPEED = -230
const TAP_TILT_SPEED = 85
const SETTLED_DISTANCE = 0.02
const SETTLED_SPEED = 0.05

export interface SpiritPose {
  readonly x: number
  readonly y: number
  readonly tilt: number
}

interface SpiritBounds {
  readonly left: number
  readonly top: number
  readonly width: number
  readonly height: number
}

type Attention = { readonly kind: 'rest' } | {
  readonly kind: 'following'
  readonly target: SpiritPose
  readonly until: number
}

const REST_POSE: SpiritPose = { x: 0, y: 0, tilt: 0 }

export function pointerPose(x: number, y: number, bounds: SpiritBounds): SpiritPose {
  const horizontal = Math.max(-1, Math.min(1, (x - bounds.left - bounds.width / 2) / bounds.width))
  const vertical = Math.max(-1, Math.min(1, (y - bounds.top - bounds.height / 2) / bounds.height))
  return {
    x: horizontal * bounds.width * HORIZONTAL_TRAVEL_RATIO,
    y: vertical * bounds.height * VERTICAL_TRAVEL_RATIO,
    tilt: horizontal * MAX_TILT_DEGREES,
  }
}

// An analytic, critically damped spring keeps the same feel at different refresh
// rates, without the accumulating delay of restarting a CSS transition per event.
function advanceAxis(position: number, velocity: number, target: number, seconds: number, response: number) {
  const displacement = position - target
  const decay = Math.exp(-response * seconds)
  const momentum = velocity + response * displacement
  return {
    position: target + (displacement + momentum * seconds) * decay,
    velocity: (velocity - response * momentum * seconds) * decay,
  }
}

export function createSpiritMotion() {
  let pose = REST_POSE
  let velocity = REST_POSE
  let attention: Attention = { kind: 'rest' }

  return {
    follow(target: SpiritPose, now: number) {
      attention = { kind: 'following', target, until: now + ATTENTION_DURATION_MS }
    },
    release() {
      attention = { kind: 'rest' }
    },
    tap() {
      attention = { kind: 'rest' }
      // Account for displacement so repeated taps cannot ratchet the artwork away from home.
      velocity = { x: 0, y: TAP_LIFT_SPEED - REST_RESPONSE * pose.y, tilt: TAP_TILT_SPEED - REST_RESPONSE * pose.tilt }
    },
    reset() {
      pose = REST_POSE
      velocity = REST_POSE
      attention = { kind: 'rest' }
      return pose
    },
    advance(seconds: number, now: number) {
      if (attention.kind === 'following' && now >= attention.until) attention = { kind: 'rest' }
      const following = attention.kind === 'following'
      const target = attention.kind === 'following' ? attention.target : REST_POSE
      const response = following ? FOLLOW_RESPONSE : REST_RESPONSE
      const x = advanceAxis(pose.x, velocity.x, target.x, seconds, response)
      const y = advanceAxis(pose.y, velocity.y, target.y, seconds, response)
      const tilt = advanceAxis(pose.tilt, velocity.tilt, target.tilt, seconds, response)
      pose = { x: x.position, y: y.position, tilt: tilt.position }
      velocity = { x: x.velocity, y: y.velocity, tilt: tilt.velocity }
      const moving = following
        || Object.values(pose).some(value => Math.abs(value) > SETTLED_DISTANCE)
        || Object.values(velocity).some(value => Math.abs(value) > SETTLED_SPEED)
      if (!moving) {
        pose = REST_POSE
        velocity = REST_POSE
      }
      return { pose, moving }
    },
  }
}
