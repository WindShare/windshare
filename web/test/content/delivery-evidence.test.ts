import { describe, expect, it } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { BlockReceiveWindow } from '../../src/content/scheduling/receive-window'
import { EVIDENCE_LIFETIME_MILLISECONDS } from '../../src/content/scheduling/completion'
import { ContentAllocation, type ContentLane, type ContentWork } from '../../src/content/scheduling/selection'
import { LanePerformance } from '../../src/content/scheduling/performance'
import { LaneSampling } from '../../src/content/scheduling/sampling'

const MIB = 1024 * 1024
const descriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share', fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision', exactSize: BigInt(MIB * 16),
  geometry: new FileGeometry(BigInt(MIB * 16), BigInt(MIB)),
}
const work: ContentWork = {
  demand: { descriptor, leaseId: new Uint8Array(16), localBlockIndex: 0n },
  routes: { active: true, allows: () => true, assertActive() {}, subscribe: () => () => {} },
  signal: new AbortController().signal, priority: 'download', distance: () => 0n,
}
function lane(id: number, route: ContentLane['route'], now: () => number): ContentLane {
  return { lane: { id, async fetchBlock() { throw new Error('Selection cannot fetch') } }, laneEpoch: 0, route,
    performance: new LanePerformance(), sampling: new LaneSampling(), receive: new BlockReceiveWindow(now),
    unsubscribeReceive() {}, inflight: 0, failed: false }
}

describe('delivery evidence and allocation', () => {
  it('reduces network work on receipt and excludes newer work from an older attempt', () => {
    let now = 0
    const receive = new BlockReceiveWindow(() => now)
    const first = receive.tryAcquire(MIB)!
    now = 100
    first.receive(MIB / 2)
    expect(receive.estimateCompletion(MIB)).toEqual({ evidence: 'recent', milliseconds: 300 })
    const before = first.progress().remaining
    const next = receive.tryAcquire(MIB)!
    expect(first.progress().remaining).toEqual(before)
    expect(receive.unfinishedBytes).toBe(MIB * 1.5)
    now = 200
    first.receive(MIB / 2)
    expect(first.progress().remainingBytes).toBe(0)
    expect(receive.unfinishedBytes).toBe(MIB)
    first.close(); next.close()
    expect(receive.load().pendingResponses).toBe(0)
  })

  it.each([true, false])('admits one revalidation while relearning (previous attempt closed: %s)', closed => {
    let now = 0
    const receive = new BlockReceiveWindow(() => now)
    const warm = receive.tryAcquire(MIB)!
    now = 10
    warm.receive(MIB); if (closed) warm.close()
    now += EVIDENCE_LIFETIME_MILLISECONDS + 1
    expect(receive.estimateCompletion(MIB).evidence).toBe('stale')
    const first = receive.tryAcquire(MIB)!
    expect(receive.estimateCompletion(MIB).evidence).toBe('unknown')
    expect(receive.tryAcquire(MIB)).toBeUndefined()
    first.close(); warm.close()
  })

  it.each<ContentLane['route']>(['direct', 'turn', 'application-relay'])(
    'protects the frontier using recent %s evidence despite a numerically cheaper unknown lane', route => {
      let now = 0
      const measured = lane(1, route, () => now)
      const warm = measured.receive.tryAcquire(MIB)!
      now = 200
      warm.receive(MIB); warm.close()
      const pending = measured.receive.tryAcquire(MIB)!
      measured.inflight = 1
      const unknown = lane(2, route === 'direct' ? 'turn' : 'direct', () => now)
      expect(measured.receive.estimateCompletion(MIB).milliseconds).toBe(400)
      expect(unknown.receive.estimateCompletion(MIB).milliseconds).toBe(250)
      const selected = new ContentAllocation().select([work], [unknown, measured], now, () => true)
      expect(selected.kind === 'dispatch' && selected.assignment.lane).toBe(measured)
      pending.close()
    },
  )

  it('learns a faster new path from fragments before the first authenticated block', () => {
    let now = 0
    const slow = lane(1, 'application-relay', () => now)
    const warm = slow.receive.tryAcquire(MIB)!
    now = 1000
    warm.receive(MIB); warm.close()
    const fast = lane(2, 'direct', () => now)
    const partial = fast.receive.tryAcquire(MIB)!
    now++
    partial.receive(64 * 1024)
    fast.inflight = 1
    expect(fast.performance.hasSuccessfulSample).toBe(false)
    const selected = new ContentAllocation().select([work], [slow, fast], now, () => true)
    expect(selected.kind === 'dispatch' && selected.assignment.lane).toBe(fast)
    partial.close()
  })

  it('does not give old fast evidence permanent priority over current delivery', () => {
    let now = 0
    const old = lane(1, 'direct', () => now)
    const warm = old.receive.tryAcquire(MIB)!
    now = 10
    warm.receive(MIB); warm.close()
    now += EVIDENCE_LIFETIME_MILLISECONDS + 1
    const current = lane(2, 'turn', () => now)
    const recent = current.receive.tryAcquire(MIB)!
    now += 200
    recent.receive(MIB); recent.close()
    const selected = new ContentAllocation().select([work], [old, current], now, () => true)
    expect(selected.kind === 'dispatch' && selected.assignment.lane).toBe(old)
    const revalidation = old.receive.tryAcquire(MIB)!
    old.inflight = 1
    const next = new ContentAllocation().select([work], [old, current], now, () => true)
    expect(next.kind === 'dispatch' && next.assignment.lane).toBe(current)
    expect(old.receive.tryAcquire(MIB)).toBeUndefined()
    revalidation.close()
  })

  it('keeps established streaming capacity through a new response startup', () => {
    let now = 0
    const receive = new BlockReceiveWindow(() => now)
    const first = receive.tryAcquire(MIB)!
    now = 100; first.receive(MIB / 2)
    now = 110; first.receive(MIB / 2); first.close()
    const before = receive.load().sample
    const next = receive.tryAcquire(MIB)!
    now = 210; next.receive(MIB / 2)
    expect(receive.load().sample).toEqual(before)
    now = 310; next.receive(MIB / 4)
    const after = receive.load().sample
    expect(after.kind !== 'unknown' && before.kind !== 'unknown' && after.bytesPerSecond < before.bytesPerSecond).toBe(true)
    next.close()
  })

  it('starts immediately when the only usable route has no evidence', () => {
    const cold = lane(1, 'turn', () => 0)
    const selected = new ContentAllocation().select([work], [cold], 0, () => true)
    expect(selected.kind === 'dispatch' && selected.assignment.lane).toBe(cold)
  })
})
