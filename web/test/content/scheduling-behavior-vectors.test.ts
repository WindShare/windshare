import { describe, expect, it } from 'vitest'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { rescueDue } from '../../src/content/scheduling/exploration'
import { loadVectorFile } from '../vectors'

interface Metrics {
  readonly pendingBytes: number
  readonly pendingBlocks: number
  readonly queueMs: number
  readonly completionMs: number
  readonly bytesPerSecond: number
  readonly hasSuccessfulSample: boolean
  readonly busyMs: number
}

type SchedulingEvent = { readonly atMs: number } & (
  | { readonly event: 'admit'; readonly lane: string; readonly attempt: string; readonly bytes: number; readonly expectedMs: number }
  | { readonly event: 'settle'; readonly attempt: string; readonly outcome: 'success' | 'cancelled' | 'superseded' }
  | { readonly event: 'inspect'; readonly lane: string; readonly bytes: number; readonly expected: Metrics }
  | { readonly event: 'rescue'; readonly attempt: string; readonly alternative: string; readonly decision: 'wait' | 'rescue' }
)

interface SchedulingCase {
  readonly name: string
  readonly events: readonly SchedulingEvent[]
}

interface Attempt {
  readonly lane: LanePerformance
  readonly started: number
  readonly expected: number
  readonly bytes: number
  active: boolean
}

class SchedulingReplay {
  readonly lanes = new Map<string, LanePerformance>()
  readonly attempts = new Map<string, Attempt>()

  lane(id: string): LanePerformance {
    let lane = this.lanes.get(id)
    if (lane === undefined) {
      lane = new LanePerformance()
      this.lanes.set(id, lane)
    }
    return lane
  }

  activeAttempt(id: string): Attempt {
    const attempt = this.attempts.get(id)
    expect(attempt?.active, 'active attempt ' + id).toBe(true)
    return attempt!
  }

  step(event: SchedulingEvent): void {
    switch (event.event) {
      case 'admit': {
        expect(this.attempts.has(event.attempt), 'duplicate attempt').toBe(false)
        const lane = this.lane(event.lane)
        const expected = lane.begin(event.atMs, event.bytes)
        this.attempts.set(event.attempt, { lane, started: event.atMs, expected, bytes: event.bytes, active: true })
        expect(expected, 'admission estimate').toBeCloseTo(event.expectedMs, 6)
        break
      }
      case 'settle': {
        const attempt = this.activeAttempt(event.attempt)
        expect(['success', 'cancelled', 'superseded']).toContain(event.outcome)
        attempt.lane.complete(event.atMs, attempt.bytes, event.outcome === 'success')
        if (event.outcome === 'superseded') attempt.lane.superseded(attempt.bytes, event.atMs - attempt.started)
        attempt.active = false
        break
      }
      case 'inspect':
        checkMetrics(this.lane(event.lane), event.atMs, event.bytes, event.expected)
        break
      case 'rescue': {
        const attempt = this.activeAttempt(event.attempt)
        const alternative = this.lane(event.alternative).estimateCompletionMilliseconds(attempt.bytes)
        const decision = rescueDue(event.atMs - attempt.started, attempt.expected, alternative) ? 'rescue' : 'wait'
        expect(decision, 'rescue decision').toBe(event.decision)
        break
      }
      default:
        throw new Error('Unknown scheduling event: ' + JSON.stringify(event))
    }
  }
}

function checkMetrics(lane: LanePerformance, now: number, bytes: number, expected: Metrics): void {
  expect(lane.pendingBytes, 'pending bytes').toBe(expected.pendingBytes)
  expect(lane.pendingBlocks, 'pending blocks').toBe(expected.pendingBlocks)
  expect(lane.hasSuccessfulSample, 'authenticated sample').toBe(expected.hasSuccessfulSample)
  expect(lane.estimateQueueMilliseconds(), 'queue').toBeCloseTo(expected.queueMs, 6)
  expect(lane.estimateCompletionMilliseconds(bytes), 'completion').toBeCloseTo(expected.completionMs, 6)
  expect(lane.bytesPerSecond, 'throughput').toBeCloseTo(expected.bytesPerSecond, 6)
  expect(lane.busyMilliseconds(now), 'busy time').toBeCloseTo(expected.busyMs, 6)
}

const vectors = loadVectorFile(new URL('../../../core/testvectors/content-scheduling-v1.json', import.meta.url))
if (vectors.kind !== 'content-scheduling-v1' || vectors.cases.length === 0) throw new Error('Missing scheduling behavior contract')

describe('shared content scheduling behavior', () => {
  it.each(vectors.cases as unknown as SchedulingCase[])('$name', ({ events }) => {
    const replay = new SchedulingReplay()
    for (const event of events) replay.step(event)
  })
})
