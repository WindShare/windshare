const IDLE_BLINK_MIN_MS = 5000
const IDLE_BLINK_MAX_MS = 9000
const DOUBLE_BLINK_CHANCE = 0.14
const BLINK_CLOSE_MS = 55
const BLINK_HOLD_MS = 20
const BLINK_OPEN_MS = 75
const BLINK_DURATION_MS = BLINK_CLOSE_MS + BLINK_HOLD_MS + BLINK_OPEN_MS
const DOUBLE_BLINK_GAP_MS = 110
const CLOSED_EYE_OPENNESS = 0.08
const SMILE_DURATION_MS = 460
const SMILE_ENTER_MS = 65
const SMILE_LEAVE_MS = 140

export interface SpiritFace {
  readonly openness: number
  readonly smile: number
}

type Expression =
  | { readonly kind: 'suspended' }
  | { readonly kind: 'open'; readonly blinkAt: number }
  | { readonly kind: 'blinking'; readonly startedAt: number; readonly double: boolean }
  | { readonly kind: 'smiling'; readonly startedAt: number; readonly until: number; readonly from: number }

const OPEN_FACE: SpiritFace = { openness: 1, smile: 0 }

function ease(progress: number) {
  const bounded = Math.max(0, Math.min(1, progress))
  return bounded * bounded * (3 - 2 * bounded)
}

function blinkDuration(double: boolean) {
  return double ? BLINK_DURATION_MS * 2 + DOUBLE_BLINK_GAP_MS : BLINK_DURATION_MS
}

function blinkFace(elapsed: number, double: boolean): SpiritFace {
  const secondStart = BLINK_DURATION_MS + DOUBLE_BLINK_GAP_MS
  const phase = double && elapsed >= secondStart ? elapsed - secondStart : elapsed
  const closed = phase < BLINK_CLOSE_MS
    ? ease(phase / BLINK_CLOSE_MS)
    : 1 - ease((phase - BLINK_CLOSE_MS - BLINK_HOLD_MS) / BLINK_OPEN_MS)
  return { openness: 1 - closed * (1 - CLOSED_EYE_OPENNESS), smile: 0 }
}

function smileFace(expression: Extract<Expression, { kind: 'smiling' }>, now: number): SpiritFace {
  const enter = expression.from + (1 - expression.from) * ease((now - expression.startedAt) / SMILE_ENTER_MS)
  const leave = ease((expression.until - now) / SMILE_LEAVE_MS)
  return { openness: 1, smile: Math.min(enter, leave) }
}

export function createSpiritExpression(random: () => number) {
  let expression: Expression = { kind: 'suspended' }
  const schedule = (now: number): Expression => ({
    kind: 'open', blinkAt: now + IDLE_BLINK_MIN_MS + random() * (IDLE_BLINK_MAX_MS - IDLE_BLINK_MIN_MS),
  })

  return {
    resume(now: number) {
      expression = schedule(now)
    },
    suspend() {
      expression = { kind: 'suspended' }
      return OPEN_FACE
    },
    blink(now: number) {
      // Acknowledgement never cuts through a smile or queues behind another blink.
      if (expression.kind === 'open') expression = { kind: 'blinking', startedAt: now, double: false }
    },
    smile(now: number) {
      if (expression.kind === 'suspended') return
      const from = expression.kind === 'smiling' ? smileFace(expression, now).smile : 0
      expression = { kind: 'smiling', startedAt: now, until: now + SMILE_DURATION_MS, from }
    },
    sample(now: number) {
      if (expression.kind === 'open' && now >= expression.blinkAt) {
        expression = { kind: 'blinking', startedAt: now, double: random() < DOUBLE_BLINK_CHANCE }
      }
      if ((expression.kind === 'blinking' && now >= expression.startedAt + blinkDuration(expression.double))
        || (expression.kind === 'smiling' && now >= expression.until)) expression = schedule(now)
      if (expression.kind === 'blinking') {
        return { face: blinkFace(now - expression.startedAt, expression.double), animating: true, wakeAt: null }
      }
      if (expression.kind === 'smiling') {
        return { face: smileFace(expression, now), animating: true, wakeAt: null }
      }
      return { face: OPEN_FACE, animating: false, wakeAt: expression.kind === 'open' ? expression.blinkAt : null }
    },
  }
}
