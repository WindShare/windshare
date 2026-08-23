import { describe, expect, it } from 'vitest'

import { byteRange } from '../../../src/content/geometry'
import type { V2BlockRangeReader } from '../../../src/content/v2-broker'
import {
  V2RevisionCapacityBusyError,
  type V2OpenedRevision,
  type V2RevisionReader,
} from '../../../src/content/v2-session-services'
import {
  V2_REVISION_CODE_QUOTA,
  type V2RevisionFailure,
} from '../../../src/content/v2-flow'
import {
  createFailureIdentity,
  createProtocolFailure,
} from '../../../src/diagnostics/incident'
import {
  V2RevisionCapacityCoordinator,
  V2RevisionCapacityWaitBudgetError,
  type V2ProtocolSessionReplacementWaiter,
  type V2RevisionCapacityClock,
  type V2RevisionCapacityTrace,
  type V2RevisionCapacityWaitSnapshot,
} from '../../../src/transfer/revision-capacity/public'

describe('browser revision capacity coordinator', () => {
  it('uses sender hint plus bounded additive jitter and retries successfully', async () => {
    const clock = new ManualClock()
    const traces: V2RevisionCapacityTrace[] = []
    const progress: V2RevisionCapacityWaitSnapshot[] = []
    let opens = 0
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: {
        open: async () => {
          opens += 1
          if (opens === 1) throw capacityError(100)
          return openedRevision()
        },
      },
      broker: emptyBroker(),
    }, {
      generation: new ManualGenerationWaiter(),
      clock,
      random: () => 0.999_999,
      randomBytes: waitIdentity,
      additiveJitterLimitMilliseconds: 20,
      visibilityThresholdMilliseconds: 50,
      onTrace: event => traces.push(event),
      onProgress: snapshot => progress.push(snapshot),
    })

    const running = coordinator.revisions.open(Uint8Array.of(1))
    await waitFor(() => traces.length === 1)

    expect(traces[0]).toMatchObject({
      transition: 'capacity_retry_scheduled',
      senderHintMilliseconds: 100,
      jitterMilliseconds: 20,
      delayMilliseconds: 120,
      activeWaiters: 1,
    })
    clock.advance(119)
    await flush()
    expect(opens).toBe(1)
    expect(progress.some(snapshot => snapshot.visible)).toBe(true)

    clock.advance(1)
    await expect(running).resolves.toMatchObject({ leaseId: Uint8Array.of(9) })
    expect(opens).toBe(2)
    expect(traces.at(-1)).toMatchObject({
      transition: 'capacity_retry_succeeded',
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 120,
    })
    expect(progress.at(-1)).toMatchObject({ activeWaiters: 0, visible: false })
  })

  it('preserves cancellation reason and removes both timer and generation waiters', async () => {
    const clock = new ManualClock()
    const generations = new ManualGenerationWaiter()
    const traces: V2RevisionCapacityTrace[] = []
    const controller = new AbortController()
    const reason = new DOMException('user paused', 'AbortError')
    const coordinator = coordinatorWithAlwaysBusy(clock, generations, traces)

    const running = coordinator.revisions.open(Uint8Array.of(1), controller.signal)
    await waitFor(() => traces.length === 1)
    clock.advance(0.25)
    controller.abort(reason)

    await expect(running).rejects.toBe(reason)
    expect(traces.at(-1)).toMatchObject({
      transition: 'capacity_wait_cancelled',
      accumulatedWaitMilliseconds: 1,
      activeWaiters: 0,
    })
    expect(clock.pending).toBe(0)
    expect(generations.pending).toBe(0)
    expect(coordinator.snapshot()).toMatchObject({ activeWaiters: 0, visible: false })
  })

  it('cancels the old timer and retries immediately after ProtocolSession replacement', async () => {
    const clock = new ManualClock()
    const generations = new ManualGenerationWaiter()
    const traces: V2RevisionCapacityTrace[] = []
    let opens = 0
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: {
        open: async () => {
          opens += 1
          if (opens === 1) throw capacityError(5_000)
          return openedRevision()
        },
      },
      broker: emptyBroker(),
    }, policy(clock, generations, traces))

    const running = coordinator.revisions.open(Uint8Array.of(1))
    await waitFor(() => generations.pending === 1)
    generations.replace()

    await expect(running).resolves.toBeDefined()
    expect(opens).toBe(2)
    expect(clock.now()).toBe(0)
    expect(clock.pending).toBe(0)
    expect(traces.map(event => event.transition)).toEqual([
      'capacity_retry_scheduled',
      'capacity_generation_replaced',
      'capacity_retry_succeeded',
    ])
    expect(traces[1]).toMatchObject({ activeWaiters: 0 })
  })

  it('pauses when the shared receive wait budget ends without issuing an early retry', async () => {
    const clock = new ManualClock()
    const generations = new ManualGenerationWaiter()
    const traces: V2RevisionCapacityTrace[] = []
    const coordinator = coordinatorWithAlwaysBusy(clock, generations, traces, 50)

    const running = coordinator.revisions.open(Uint8Array.of(1))
    await waitFor(() => traces.length === 1)
    expect(traces[0]).toMatchObject({ delayMilliseconds: 50 })
    clock.advance(50)

    const error = await running.catch(reason => reason as unknown)
    expect(error).toBeInstanceOf(V2RevisionCapacityWaitBudgetError)
    expect((error as V2RevisionCapacityWaitBudgetError).failureFact).toMatchObject({
      kind: 'protocol_failure',
      stage: 'protocol_operation',
      recoveryDisposition: 'resumable_receive',
    })
    expect(traces.at(-1)).toMatchObject({
      transition: 'capacity_wait_budget_paused',
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 50,
    })
  })

  it('charges concurrent waiters one union of wall-clock time', async () => {
    const clock = new ManualClock()
    const generations = new ManualGenerationWaiter()
    let opens = 0
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: {
        open: async () => {
          opens += 1
          throw capacityError(100)
        },
      },
      broker: emptyBroker(),
    }, {
      ...policy(clock, generations, []),
      waitBudgetMilliseconds: 50,
    })

    const first = coordinator.revisions.open(Uint8Array.of(1))
    const second = coordinator.revisions.open(Uint8Array.of(2))
    await waitFor(() => coordinator.snapshot().activeWaiters === 2)
    clock.advance(50)

    const results = await Promise.allSettled([first, second])
    expect(results.every(result =>
      result.status === 'rejected' &&
      result.reason instanceof V2RevisionCapacityWaitBudgetError)).toBe(true)
    expect(opens).toBe(2)
    expect(coordinator.snapshot()).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 50,
      attempts: 2,
    })
  })

  it('restarts a range at the exact next unconsumed offset after capacity recovery', async () => {
    const clock = new ManualClock()
    const generations = new ManualGenerationWaiter()
    const ranges: Array<readonly [bigint, bigint]> = []
    let reads = 0
    const broker: V2BlockRangeReader = {
      readRange: async function* (_descriptor, _lease, range) {
        ranges.push([range.start, range.end])
        reads += 1
        if (reads === 1) {
          yield { offset: 0n, data: Uint8Array.of(1, 2) }
          throw capacityError(10)
        }
        yield { offset: 2n, data: Uint8Array.of(3, 4) }
      },
    }
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: neverOpenedRevisions(),
      broker,
    }, policy(clock, generations, []))

    const slices: Uint8Array[] = []
    const running = (async () => {
      for await (const slice of coordinator.broker.readRange(
        {} as never,
        Uint8Array.of(1),
        byteRange(0n, 4n),
      )) slices.push(slice.data)
    })()
    await waitFor(() => clock.pending > 0)
    clock.advance(10)
    await running

    expect(ranges).toEqual([[0n, 4n], [2n, 4n]])
    expect(slices).toEqual([Uint8Array.of(1, 2), Uint8Array.of(3, 4)])
  })

  it('charges only capacity-blocked range segments across multiple mid-range denials', async () => {
    const clock = new ManualClock()
    const traces: V2RevisionCapacityTrace[] = []
    let reads = 0
    const coordinator = new V2RevisionCapacityCoordinator({
      revisions: neverOpenedRevisions(),
      broker: {
        readRange: async function* (_descriptor, _lease, range) {
          reads += 1
          if (reads === 1) throw capacityError(10)
          if (reads === 2) {
            yield { offset: range.start, data: Uint8Array.of(1, 2) }
            throw capacityError(100)
          }
          yield { offset: range.start, data: Uint8Array.of(3, 4) }
        },
      },
    }, {
      ...policy(clock, new ManualGenerationWaiter(), traces),
      waitBudgetMilliseconds: 50,
    })

    const iterator = coordinator.broker.readRange(
      {} as never,
      Uint8Array.of(1),
      byteRange(0n, 4n),
    )[Symbol.asyncIterator]()
    const firstSlice = iterator.next()
    await waitFor(() => clock.pending > 0)
    clock.advance(10)
    await expect(firstSlice).resolves.toMatchObject({
      done: false,
      value: { offset: 0n, data: Uint8Array.of(1, 2) },
    })
    expect(coordinator.snapshot()).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 10,
      attempts: 1,
    })

    clock.advance(25)
    expect(coordinator.snapshot()).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 10,
    })

    const nextSlice = iterator.next()
    await waitFor(() => traces.filter(
      event => event.transition === 'capacity_retry_scheduled',
    ).length === 2)
    expect(traces.at(-1)).toMatchObject({
      transition: 'capacity_retry_scheduled',
      delayMilliseconds: 40,
      accumulatedWaitMilliseconds: 10,
      activeWaiters: 1,
    })
    clock.advance(40)
    await expect(nextSlice).rejects.toBeInstanceOf(V2RevisionCapacityWaitBudgetError)
    expect(coordinator.snapshot()).toMatchObject({
      activeWaiters: 0,
      accumulatedWaitMilliseconds: 50,
      attempts: 2,
    })
  })

  it('does not retry generic errors and hides sub-threshold waits without flicker', async () => {
    const generic = new Error('ordinary source failure')
    let genericAttempts = 0
    const direct = new V2RevisionCapacityCoordinator({
      revisions: { open: async () => { genericAttempts += 1; throw generic } },
      broker: emptyBroker(),
    }, policy(new ManualClock(), new ManualGenerationWaiter(), []))
    await expect(direct.revisions.open(Uint8Array.of(1))).rejects.toBe(generic)
    expect(genericAttempts).toBe(1)

    const clock = new ManualClock()
    const progress: V2RevisionCapacityWaitSnapshot[] = []
    let attempts = 0
    const short = new V2RevisionCapacityCoordinator({
      revisions: {
        open: async () => {
          attempts += 1
          if (attempts === 1) throw capacityError(20)
          return openedRevision()
        },
      },
      broker: emptyBroker(),
    }, {
      ...policy(clock, new ManualGenerationWaiter(), []),
      visibilityThresholdMilliseconds: 50,
      onProgress: snapshot => progress.push(snapshot),
    })
    const running = short.revisions.open(Uint8Array.of(1))
    await waitFor(() => clock.pending > 0)
    clock.advance(20)
    await running
    expect(progress.some(snapshot => snapshot.visible)).toBe(false)
  })
})

function coordinatorWithAlwaysBusy(
  clock: ManualClock,
  generations: ManualGenerationWaiter,
  traces: V2RevisionCapacityTrace[],
  waitBudgetMilliseconds = 1_000,
): V2RevisionCapacityCoordinator {
  return new V2RevisionCapacityCoordinator({
    revisions: { open: async () => { throw capacityError(100) } },
    broker: emptyBroker(),
  }, {
    ...policy(clock, generations, traces),
    waitBudgetMilliseconds,
  })
}

function policy(
  clock: ManualClock,
  generation: V2ProtocolSessionReplacementWaiter,
  traces: V2RevisionCapacityTrace[],
) {
  return {
    generation,
    clock,
    random: () => 0,
    randomBytes: waitIdentity,
    additiveJitterLimitMilliseconds: 0,
    visibilityThresholdMilliseconds: 500,
    onTrace: (event: V2RevisionCapacityTrace) => traces.push(event),
  } as const
}

function capacityError(retryAfterMilliseconds: number): V2RevisionCapacityBusyError {
  const failure: V2RevisionFailure = Object.freeze({
    code: V2_REVISION_CODE_QUOTA,
    retryable: true,
    retryAfterMilliseconds,
  })
  const protocolFailure = createProtocolFailure({
    requestKind: 'open_revisions',
    wireScope: 'revision',
    wireCode: failure.code,
    retryable: true,
    retryAfterMilliseconds,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', identity(1)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(2)),
    },
  })
  return new V2RevisionCapacityBusyError(failure, protocolFailure)
}

function openedRevision(): V2OpenedRevision {
  return Object.freeze({
    descriptor: {} as never,
    leaseId: Uint8Array.of(9),
    release: async () => undefined,
  })
}

function emptyBroker(): V2BlockRangeReader {
  return { readRange: async function* () { /* no content */ } }
}

function neverOpenedRevisions(): V2RevisionReader {
  return { open: async () => { throw new Error('not used') } }
}

function waitIdentity(length: number): Uint8Array {
  return identity(9).slice(0, length)
}

function identity(first: number): Uint8Array {
  const bytes = new Uint8Array(16)
  bytes[0] = first
  return bytes
}

class ManualClock implements V2RevisionCapacityClock {
  #now = 0
  readonly #sleeps = new Set<{
    readonly deadline: number
    readonly signal: AbortSignal
    readonly resolve: () => void
    readonly reject: (reason: unknown) => void
    readonly abort: () => void
  }>()

  get pending(): number { return this.#sleeps.size }
  now(): number { return this.#now }

  sleep(milliseconds: number, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    return new Promise((resolve, reject) => {
      const abort = () => {
        this.#sleeps.delete(sleep)
        reject(signal.reason)
      }
      const sleep = { deadline: this.#now + milliseconds, signal, resolve, reject, abort }
      this.#sleeps.add(sleep)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }

  advance(milliseconds: number): void {
    this.#now += milliseconds
    for (const sleep of [...this.#sleeps]) {
      if (sleep.deadline > this.#now) continue
      this.#sleeps.delete(sleep)
      sleep.signal.removeEventListener('abort', sleep.abort)
      sleep.resolve()
    }
  }
}

class ManualGenerationWaiter implements V2ProtocolSessionReplacementWaiter {
  readonly #waiters = new Set<{
    readonly resolve: () => void
    readonly reject: (reason: unknown) => void
    readonly signal: AbortSignal
    readonly abort: () => void
  }>()

  get pending(): number { return this.#waiters.size }

  waitForProtocolSessionReplacement(
    _identity: Parameters<
      V2ProtocolSessionReplacementWaiter['waitForProtocolSessionReplacement']
    >[0],
    signal: AbortSignal,
  ): Promise<void> {
    signal.throwIfAborted()
    return new Promise((resolve, reject) => {
      const abort = () => {
        this.#waiters.delete(waiter)
        reject(signal.reason)
      }
      const waiter = { resolve, reject, signal, abort }
      this.#waiters.add(waiter)
      signal.addEventListener('abort', abort, { once: true })
      if (signal.aborted) abort()
    })
  }

  replace(): void {
    for (const waiter of this.#waiters) {
      waiter.signal.removeEventListener('abort', waiter.abort)
      waiter.resolve()
    }
    this.#waiters.clear()
  }
}

async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (predicate()) return
    await flush()
  }
  throw new Error('condition was not reached')
}

async function flush(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
}
