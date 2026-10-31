import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FileGeometry } from '../../src/content/geometry'
import { BlockLaneAttemptsError, LaneSet, type BlockDemand, type BlockLane, type BlockRetryObservation } from '../../src/content/lane-set'
import type { BlockRecord, FileRevisionDescriptor } from '../../src/content/records'
import type { BlockRouteEligibility, BlockTransportRoute } from '../../src/content/route-policy'
import { MAXIMUM_BLOCK_ATTEMPTS } from '../../src/content/scheduling/attempts'
import { BlockInactivityTimeoutError } from '../../src/content/scheduling/block-response-wait'

const BLOCK_BYTES = 1024
const descriptor: FileRevisionDescriptor = {
  shareInstance: new Uint8Array(16), shareInstanceId: 'share',
  fileId: new Uint8Array(16), fileIdText: 'file',
  fileRevision: new Uint8Array(16), fileRevisionText: 'revision',
  exactSize: 64n * BigInt(BLOCK_BYTES), geometry: new FileGeometry(64n * BigInt(BLOCK_BYTES), BigInt(BLOCK_BYTES)),
}
const routes: BlockRouteEligibility = {
  active: true, allows: () => true, assertActive() {}, subscribe: () => () => {},
}
const relayOnly = { ...routes, allows: (route: BlockTransportRoute) => route === 'application-relay' }
function demand(index: bigint): BlockDemand {
  return { descriptor, localBlockIndex: index, leaseId: new Uint8Array(16) }
}
function record(input: BlockDemand): BlockRecord {
  return { descriptor, localBlockIndex: input.localBlockIndex, data: new Uint8Array(BLOCK_BYTES) }
}
function timeout() { return new BlockInactivityTimeoutError('receiving_fragments', 15_000, 0) }
async function flush() { for (let turn = 0; turn < 32; turn++) await Promise.resolve() }

class ControlledLane implements BlockLane {
  readonly calls: bigint[] = []
  readonly #pending = new Map<bigint, { succeed(): void; fail(error: unknown): void }>()
  readonly id: number
  constructor(id: number) { this.id = id }
  fetchBlock(input: BlockDemand, signal: AbortSignal): Promise<BlockRecord> {
    this.calls.push(input.localBlockIndex)
    return new Promise((resolve, reject) => {
      const finish = () => {
        this.#pending.delete(input.localBlockIndex)
        signal.removeEventListener('abort', abort)
      }
      const abort = () => { finish(); reject(signal.reason) }
      this.#pending.set(input.localBlockIndex, {
        succeed: () => { finish(); resolve(record(input)) },
        fail: error => { finish(); reject(error) },
      })
      signal.addEventListener('abort', abort, { once: true })
    })
  }
  succeed(index: bigint): void {
    const pending = this.#pending.get(index)
    if (pending === undefined) throw new Error('No admitted block to complete')
    pending.succeed()
  }
  fail(index: bigint, error = timeout()): void {
    const pending = this.#pending.get(index)
    if (pending === undefined) throw new Error('No admitted block to fail')
    pending.fail(error)
  }
}

function read(lanes: LaneSet, index: bigint, policy = routes, signal = new AbortController().signal) {
  // These cases isolate sequential retries; rescue ownership is exercised separately.
  return lanes.fetchWork({ demand: demand(index), routes: policy, signal, priority: 'download', distance: () => 1n })
}

describe('authenticated block retry authority', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => { expect(vi.getTimerCount()).toBe(0); vi.useRealTimers() })

  it('retries block 27 on the same relay after later delivery and a direct timeout', async () => {
    const observations: BlockRetryObservation[] = []
    const lanes = new LaneSet({ now: () => 0, onBlockRetry: event => observations.push(event) })
    const relay = new ControlledLane(1), direct = new ControlledLane(2)
    lanes.add(relay, 'application-relay')
    const result = read(lanes, 27n)
    const checked = expect(result).resolves.toMatchObject({ localBlockIndex: 27n })
    await flush()
    lanes.add(direct, 'direct')
    relay.fail(27n)
    await flush()
    const other = read(lanes, 28n, relayOnly)
    await flush()
    relay.succeed(28n)
    await other
    direct.fail(27n)
    await flush()
    expect(relay.calls).toEqual([27n, 28n, 27n])
    relay.succeed(27n)
    await checked
    expect(observations).toMatchObject([
      { transition: 'retrying', reason: 'untried_lane', attempts: 2, laneId: 2 },
      { transition: 'retrying', reason: 'recovered_lane', attempts: 3, laneId: 1,
        firstDispatchSequence: 1, successfulBlocksAtFailure: 0, successfulBlocks: 1 },
    ])
    expect(lanes.schedulingDemand()).toMatchObject({ inFlightBytes: 0, unfinishedBytes: 0, queuedBlocks: 0 })
    lanes.close()
  })

  it('does not use success before the failed attempt as recovery evidence', async () => {
    const observations: BlockRetryObservation[] = []
    const lanes = new LaneSet({ onBlockRetry: event => observations.push(event) })
    const relay = new ControlledLane(1), direct = new ControlledLane(2)
    lanes.add(relay, 'application-relay')
    const warm = read(lanes, 28n)
    await flush()
    relay.succeed(28n)
    await warm
    const result = read(lanes, 27n)
    const checked = expect(result).rejects.toBeInstanceOf(BlockLaneAttemptsError)
    await flush()
    lanes.add(direct, 'direct')
    relay.fail(27n)
    await flush()
    direct.fail(27n)
    await checked
    expect(relay.calls).toEqual([28n, 27n])
    expect(observations.at(-1)).toMatchObject({ transition: 'exhausted', reason: 'no_recovered_lane', attempts: 2 })
    lanes.close()
  })

  it('never replenishes a block budget when other blocks keep succeeding', async () => {
    const observations: BlockRetryObservation[] = []
    const lanes = new LaneSet({ now: () => 0, onBlockRetry: event => observations.push(event) })
    const failures: Error[] = []
    let previousRoute: BlockTransportRoute | undefined
    for (const [id, route] of [[1, 'direct'], [2, 'application-relay']] as const) {
      lanes.add({ id, fetchBlock: async input => {
        if (input.localBlockIndex !== 27n) return record(input)
        if (previousRoute !== undefined) {
          const recoveryRoute = previousRoute
          await read(lanes, 28n, { ...routes, allows: candidate => candidate === recoveryRoute })
        }
        previousRoute = route
        const failure = timeout()
        failures.push(failure)
        throw failure
      } }, route)
    }
    await expect(read(lanes, 27n)).rejects.toMatchObject({ errors: failures })
    expect(failures).toHaveLength(MAXIMUM_BLOCK_ATTEMPTS)
    expect(observations.filter(event => event.transition === 'retrying' && event.reason === 'recovered_lane'))
      .toHaveLength(MAXIMUM_BLOCK_ATTEMPTS - 2)
    expect(observations.at(-1)).toMatchObject({
      transition: 'exhausted', reason: 'attempt_limit', attempts: MAXIMUM_BLOCK_ATTEMPTS,
      maximumAttempts: MAXIMUM_BLOCK_ATTEMPTS, firstDispatchSequence: 1,
    })
    lanes.close()
  })

  it('bounds replacement generations even if the last failed lane disappears', async () => {
    const lanes = new LaneSet()
    let calls = 0
    const replace = (epoch: number) => lanes.add({ id: 1, fetchBlock: async () => {
      calls++
      lanes.remove(1)
      if (calls < MAXIMUM_BLOCK_ATTEMPTS) replace(epoch + 1)
      throw timeout()
    } }, 'direct', epoch)
    replace(0)
    await expect(read(lanes, 27n)).rejects.toBeInstanceOf(BlockLaneAttemptsError)
    expect(calls).toBe(MAXIMUM_BLOCK_ATTEMPTS)
    expect(lanes.size).toBe(0)
    lanes.close()
  })

  it('charges rescue and replacement attempts to the same finite budget', async () => {
    const observations: string[] = []
    const lanes = new LaneSet({ now: Date.now, onBlockScheduled: event => observations.push(event.purpose) })
    const primary = new ControlledLane(1), rescue = new ControlledLane(2)
    lanes.add(primary, 'direct')
    lanes.add(rescue, 'application-relay')
    const result = lanes.fetch(demand(27n), routes, new AbortController().signal)
    const checked = expect(result).rejects.toBeInstanceOf(BlockLaneAttemptsError)
    await vi.advanceTimersByTimeAsync(1000)
    expect(primary.calls).toEqual([27n])
    expect(rescue.calls).toEqual([27n])
    let replacements = 0
    const replace = (epoch: number) => lanes.add({ id: 3, fetchBlock: async () => {
      replacements++
      lanes.remove(3)
      if (replacements < MAXIMUM_BLOCK_ATTEMPTS) replace(epoch + 1)
      throw timeout()
    } }, 'direct', epoch)
    replace(0)
    primary.fail(27n)
    rescue.fail(27n)
    await checked
    expect(observations.filter(purpose => purpose === 'rescue')).toHaveLength(1)
    expect(replacements).toBe(MAXIMUM_BLOCK_ATTEMPTS - 2)
    expect(lanes.schedulingDemand()).toMatchObject({ inFlightBytes: 0, unfinishedBytes: 0, queuedBlocks: 0 })
    lanes.close()
  })

  it('cancels recovery while shared capacity is unavailable', async () => {
    const lanes = new LaneSet()
    const relay = new ControlledLane(1), direct = new ControlledLane(2)
    const controller = new AbortController()
    let ready = true, held = 0
    lanes.add(relay, 'application-relay')
    const result = lanes.fetchWork({
      demand: demand(27n), routes, signal: controller.signal, priority: 'download', distance: () => 1n,
      capacity: { lastServed: () => 0, readiness: () => ready ? 'ready' : 'consumer',
        acquire: () => { held++; return () => { held-- } } },
    })
    const reason = new DOMException('Paused by caller', 'AbortError')
    const checked = expect(result).rejects.toBe(reason)
    await flush()
    lanes.add(direct, 'direct')
    relay.fail(27n)
    await flush()
    const other = read(lanes, 28n, relayOnly)
    await flush()
    relay.succeed(28n)
    await other
    ready = false
    direct.fail(27n)
    await flush()
    expect(lanes.schedulingDemand()).toMatchObject({ queuedBlocks: 1, capacityWaitingBytes: BLOCK_BYTES })
    expect(relay.calls).toEqual([27n, 28n])
    controller.abort(reason)
    await checked
    expect(held).toBe(0)
    expect(lanes.schedulingDemand()).toMatchObject({ queuedBlocks: 0, inFlightBytes: 0 })
    lanes.close()
  })

  it('does not reauthorize a recovered route forbidden by the caller', async () => {
    const lanes = new LaneSet()
    const relay = new ControlledLane(1), direct = new ControlledLane(2)
    let allowRelay = true
    const policy = { ...routes, allows: (route: BlockTransportRoute) => allowRelay || route === 'direct' }
    lanes.add(relay, 'application-relay')
    const result = read(lanes, 27n, policy)
    const checked = expect(result).rejects.toBeInstanceOf(BlockLaneAttemptsError)
    await flush()
    lanes.add(direct, 'direct')
    relay.fail(27n)
    await flush()
    const other = read(lanes, 28n, relayOnly)
    await flush()
    relay.succeed(28n)
    await other
    allowRelay = false
    direct.fail(27n)
    await checked
    expect(relay.calls).toEqual([27n, 28n])
    lanes.close()
  })
})
