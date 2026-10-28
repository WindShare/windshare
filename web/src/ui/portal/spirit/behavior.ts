import { createSpiritExpression, type SpiritFace } from './expression'
import { createSpiritMotion, type SpiritPose } from './motion'

const ATTENTION_DURATION_MS = 420
const RETURN_AFTER_BLINK_MS = 90

type Attention =
  | { readonly kind: 'suspended' | 'rest' }
  | { readonly kind: 'following' | 'returning'; readonly until: number }

export interface SpiritFrame {
  readonly pose: SpiritPose
  readonly face: SpiritFace
  readonly animating: boolean
  readonly wakeAt: number | null
}

export function createSpiritBehavior(random: () => number) {
  const motion = createSpiritMotion()
  const expression = createSpiritExpression(random)
  let attention: Attention = { kind: 'suspended' }

  return {
    resume(now: number) {
      attention = { kind: 'rest' }
      expression.resume(now)
    },
    suspend(): SpiritFrame {
      attention = { kind: 'suspended' }
      return { pose: motion.reset(), face: expression.suspend(), animating: false, wakeAt: null }
    },
    follow(target: SpiritPose, now: number) {
      if (attention.kind === 'suspended') return
      attention = { kind: 'following', until: now + ATTENTION_DURATION_MS }
      motion.follow(target)
    },
    release() {
      if (attention.kind === 'suspended') return
      attention = { kind: 'rest' }
      motion.release()
    },
    tap(now: number) {
      if (attention.kind === 'suspended') return
      attention = { kind: 'rest' }
      motion.tap()
      expression.smile(now)
    },
    advance(seconds: number, now: number): SpiritFrame {
      if (attention.kind === 'following' && now >= attention.until) {
        expression.blink(now)
        // Let the eyes acknowledge the pause before the body starts returning.
        attention = { kind: 'returning', until: now + RETURN_AFTER_BLINK_MS }
      }
      if (attention.kind === 'returning' && now >= attention.until) {
        attention = { kind: 'rest' }
        motion.release()
      }
      const body = motion.advance(seconds)
      const face = expression.sample(now)
      return { pose: body.pose, face: face.face, animating: body.moving || face.animating, wakeAt: face.wakeAt }
    },
  }
}
