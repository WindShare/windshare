import { once } from 'node:events'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { connect, createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { afterEach, describe, expect, it } from 'vitest'

import {
  acquireWholeSampleResource,
  HotSwitchEvidenceCollector,
  WholeSampleDeadline,
  type HotSwitchPageEvent,
  type WholeSampleDeadlineClock,
  type WholeSampleDeadlineScheduler,
  type WholeSampleDeadlineTimer,
  type WholeSampleDeadlineTiming,
} from '../../e2e/fixtures/hot-switch-evidence'
import {
  FixtureInfrastructureError,
  ManagedProcess,
  containsFixtureInfrastructureFailure,
} from '../../e2e/fixtures/managed-process'
import { acquireTestIceTopology } from '../../e2e/fixtures/test-ice-topology-runtime'
import {
  RelayProxy,
  readSenderAttemptEvidenceSnapshot,
} from '../../e2e/fixtures/v2-real-stack'
import type { AttemptEvidence } from '../../scripts/browser-evidence/attempt-evidence'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  testIceTopologyResolutionSha256,
  testIceTopologySha256,
} from '../../scripts/browser-evidence/test-ice-topology'
import { admittedEvents, browserFailureEvents } from '../browser-evidence/fixtures'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const CANONICAL_TOPOLOGY_PROFILE = resolve(
  REPOSITORY_ROOT,
  'testdata/test-ice-topology/pr-same-host-kernel-route-ipv4.json',
)
const EXAMPLE_TOPOLOGY_RESOLUTION = resolve(
  REPOSITORY_ROOT,
  'testdata/test-ice-topology/pr-same-host-kernel-route-ipv4-resolution.json',
)
const WRONG_SHA256 = '0'.repeat(64)
const DIFFERENT_SHA256 = '1'.repeat(64)
const TOPOLOGY_ENV_NAMES = Object.freeze({
  profile: 'WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE',
  resolution: 'WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION',
  profileSha256: 'WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256',
  resolutionSha256: 'WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256',
})
// Context-only cases must not inherit the alternate full-suite authority from
// the process that happens to run Vitest.
const NO_PUBLISHED_TOPOLOGY_ENVIRONMENT = Object.freeze({})

describe('published product topology lock', () => {
  const cleanup: string[] = []

  afterEach(async () => {
    await Promise.all(cleanup.splice(0).map((path) => rm(path, { recursive: true, force: true })))
  })

  it('accepts an arbitrary absolute suite-local copy with canonical bytes and both digests', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup)

    const acquired = await acquireTestIceTopology(
      fixture.context,
      NO_PUBLISHED_TOPOLOGY_ENVIRONMENT,
    )

    expect(acquired.profilePath).toBe(fixture.context.topologyProfilePath)
    expect(acquired.resolutionPath).toBe(fixture.context.topologyResolutionPath)
    expect(acquired.lock.profileSha256).toBe(fixture.context.topologyProfileSha256)
    expect(acquired.lock.resolutionSha256).toBe(fixture.context.topologyResolutionSha256)
    await acquired.release()
  })

  it('uses the complete published lock environment when the full suite has no child context', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup)

    const acquired = await acquireTestIceTopology(
      undefined,
      publishedTopologyEnvironment(fixture.context),
    )

    expect(acquired.profilePath).toBe(fixture.context.topologyProfilePath)
    expect(acquired.resolutionPath).toBe(fixture.context.topologyResolutionPath)
    expect(acquired.lock.profileSha256).toBe(fixture.context.topologyProfileSha256)
    expect(acquired.lock.resolutionSha256).toBe(fixture.context.topologyResolutionSha256)
    await acquired.release()
  })

  it('rejects a partial published topology environment instead of rematerializing privately', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup)

    await expect(acquireTestIceTopology(undefined, {
      [TOPOLOGY_ENV_NAMES.profile]: fixture.context.topologyProfilePath,
    })).rejects.toThrow('must provide all four lock values')
  })

  it('rejects defined-but-empty lock variables instead of treating them as absent', async () => {
    await expect(acquireTestIceTopology(undefined, Object.fromEntries(
      Object.values(TOPOLOGY_ENV_NAMES).map((name) => [name, '']),
    ))).rejects.toThrow('must provide all four lock values')
  })

  it('rejects disagreement between child context and the published topology environment', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup)
    const environment = publishedTopologyEnvironment(fixture.context)
    environment[TOPOLOGY_ENV_NAMES.resolutionSha256] = DIFFERENT_SHA256

    await expect(acquireTestIceTopology(fixture.context, environment)).rejects.toThrow(
      'Child evidence context and topology environment identify different locks',
    )
  })

  it('rejects a suite-local profile whose bytes are not canonical', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup, { mutateProfile: true })

    await expect(acquireTestIceTopology(
      fixture.context,
      NO_PUBLISHED_TOPOLOGY_ENVIRONMENT,
    )).rejects.toThrow(
      'Current sample topology profile is not the canonical PR profile',
    )
  })

  it('rejects a canonical suite-local profile with the wrong published digest', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup, { wrongProfileDigest: true })

    await expect(acquireTestIceTopology(
      fixture.context,
      NO_PUBLISHED_TOPOLOGY_ENVIRONMENT,
    )).rejects.toThrow(
      'Current sample topology profile differs from its locked digest',
    )
  })

  it('rejects a canonical suite-local resolution with the wrong published digest', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup, { wrongResolutionDigest: true })

    await expect(acquireTestIceTopology(
      fixture.context,
      NO_PUBLISHED_TOPOLOGY_ENVIRONMENT,
    )).rejects.toThrow(
      'Current sample topology resolution differs from its locked digest',
    )
  })

  it('rejects a wrong digest supplied by the no-context full-suite environment', async () => {
    const fixture = await createPublishedTopologyFixture(cleanup)
    const environment = publishedTopologyEnvironment(fixture.context)
    environment[TOPOLOGY_ENV_NAMES.resolutionSha256] = WRONG_SHA256

    await expect(acquireTestIceTopology(undefined, environment)).rejects.toThrow(
      'Current sample topology resolution differs from its locked digest',
    )
  })
})

describe('event-driven hot-switch fixtures', () => {
  it('does not let relay fallback activity settle the peer terminal barrier', async () => {
    const collector = new HotSwitchEvidenceCollector()
    collector.acceptPageEvent(dispatchEvent(1, 'relay', 1, 0))
    const terminal = collector.waitForBrowserTerminal(1_000)

    await expect(Promise.race([
      terminal.then(() => 'terminal'),
      Promise.resolve('still-pending'),
    ])).resolves.toBe('still-pending')

    for (const evidence of browserFailureEvents()) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    await expect(terminal).resolves.toMatchObject({ stage: 'failed' })
    expect(collector.finalizeAttempts()).toMatchObject([{ outcome: 'failed' }])
    expect(collector.routeEvidence('relay-only')).toMatchObject({
      mode: 'relay-only',
      observations: [{ kind: 'dispatch', route: 'relay', dispatchSequence: 1 }],
    })
  })

  it('builds a contiguous relay-admission-fence-peer proof from producer events', async () => {
    const collector = new HotSwitchEvidenceCollector()
    collector.acceptPageEvent(dispatchEvent(1, 'relay', 1, 0))
    const events = admittedEvents()
    for (const evidence of sideEvents(events, 'browser')) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    const terminal = await collector.waitForBrowserTerminal(1_000)
    expect(terminal.stage).toBe('admitted')
    collector.recordRelayCutFence(1, {
      proxyAccepting: false,
      receiverRelayEligible: false,
    })
    collector.acceptPageEvent(dispatchEvent(2, 'peer', 7, 9))
    await expect(collector.waitForPostFencePeerDispatch({ laneId: 7, laneEpoch: 9 }, 1, 1_000))
      .resolves.toMatchObject({ dispatchSequence: 2, route: 'peer' })

    collector.ingestSenderEvidence(sideEvents(events, 'sender'))
    expect(collector.finalizeAttempts()).toMatchObject([{ outcome: 'admitted' }])
    expect(collector.routeEvidence('hot-switch')).toMatchObject({
      mode: 'hot-switch',
      observations: [
        { kind: 'dispatch', route: 'relay', dispatchSequence: 1 },
        { kind: 'peer-admitted', lane: { laneId: 7, laneEpoch: 9 } },
        { kind: 'relay-cut-fence', dispatchSequenceBoundary: 1 },
        { kind: 'dispatch', route: 'peer', dispatchSequence: 2 },
      ],
    })
  })

  it('rejects an admission that the producer emitted before the first relay dispatch', () => {
    const collector = new HotSwitchEvidenceCollector()
    for (const evidence of sideEvents(admittedEvents(), 'browser')) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    collector.acceptPageEvent(dispatchEvent(1, 'relay', 1, 0))
    collector.recordRelayCutFence(1, {
      proxyAccepting: false,
      receiverRelayEligible: false,
    })
    collector.acceptPageEvent(dispatchEvent(2, 'peer', 7, 9))

    expect(() => collector.routeEvidence('hot-switch')).toThrow(
      /does not prove relay, admission, cut fence, and post-fence peer dispatch/u,
    )
  })

  it('rejects relay dispatch that the producer emits after the completed fence', () => {
    const collector = new HotSwitchEvidenceCollector()
    collector.acceptPageEvent(dispatchEvent(1, 'relay', 1, 0))
    for (const evidence of sideEvents(admittedEvents(), 'browser')) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    collector.recordRelayCutFence(1, {
      proxyAccepting: false,
      receiverRelayEligible: false,
    })
    collector.acceptPageEvent(dispatchEvent(2, 'peer', 7, 9))
    collector.acceptPageEvent(dispatchEvent(3, 'relay', 1, 0))

    expect(() => collector.routeEvidence('hot-switch')).toThrow(
      /does not prove relay, admission, cut fence, and post-fence peer dispatch/u,
    )
  })

  it('settles outstanding evidence waits from the producer runtime terminal', async () => {
    const collector = new HotSwitchEvidenceCollector()
    const peer = collector.waitForBrowserTerminal(1_000)
    const delivery = collector.waitForDelivery(1_000)
    const runtime = collector.waitForRuntimeSettlement(1_000)
    const peerTerminal = expect(peer).rejects.toThrow(
      'Hot-switch runtime failed: simulated runtime failure',
    )
    const deliveryTerminal = expect(delivery).rejects.toThrow(
      'Hot-switch runtime failed: simulated runtime failure',
    )

    collector.acceptPageEvent({ kind: 'runtime-settled', error: 'simulated runtime failure' })

    await expect(runtime).resolves.toEqual({
      kind: 'runtime-settled',
      error: 'simulated runtime failure',
    })
    await Promise.all([peerTerminal, deliveryTerminal])
  })

  it('does not let stale lane detachment satisfy the post-cut receiver fence', async () => {
    const collector = new HotSwitchEvidenceCollector()
    collector.acceptPageEvent(laneEvent('lane-admitted', 'relay', 1, 0))
    collector.acceptPageEvent(laneEvent('lane-detached', 'relay', 1, 0))
    collector.acceptPageEvent(laneEvent('lane-admitted', 'relay', 2, 0))
    const ineligible = collector.waitForRelayIneligibility(1_000)

    await expect(Promise.race([
      ineligible.then(() => 'ineligible'),
      Promise.resolve('replacement-still-eligible'),
    ])).resolves.toBe('replacement-still-eligible')

    collector.acceptPageEvent({ kind: 'relay-ineligible' })
    await expect(ineligible).resolves.toBe(true)
    expect(() => collector.acceptPageEvent(
      laneEvent('lane-admitted', 'relay', 3, 0),
    )).toThrow('admitted a relay lane after publishing relay ineligibility')
  })
})

describe('bounded sample authority', () => {
  it('aborts work at its absolute cutoff and never manufactures post-expiry time', async () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)
    runtime.advanceTo(10_590)
    expect(deadline.remainingWork(25)).toBe(10)

    const running = deadline.runWork(() => new Promise<never>(() => {}))
    const cutoff = expect(running).rejects.toMatchObject({
      name: 'WholeSampleDeadlineExpiredError',
      phase: 'work',
      cutoffAtMs: 10_600,
    })
    runtime.advanceTo(10_599)
    expect(deadline.workSignal.aborted).toBe(false)
    expect(deadline.remainingWork()).toBe(1)

    runtime.advanceTo(10_600)
    expect(deadline.workSignal.aborted).toBe(true)
    expect(() => deadline.remainingWork()).toThrow('work phase reached its absolute cutoff')
    await cutoff

    let expiredWorkInvoked = false
    await expect(deadline.runWork(async () => {
      expiredWorkInvoked = true
      return 'created late ownership'
    })).rejects.toMatchObject({ phase: 'work', cutoffAtMs: 10_600 })
    expect(expiredWorkInvoked).toBe(false)
    deadline.dispose()
  })

  it('does not acquire or register rollback when work is already expired', async () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)
    runtime.advanceTo(10_600)
    let acquisitionInvoked = false
    const lateCleanup: Promise<unknown>[] = []

    await expect(acquireWholeSampleResource(
      deadline,
      async () => {
        acquisitionInvoked = true
        return 'late resource'
      },
      'late resource rollback',
      async () => undefined,
      (_boundary, task) => lateCleanup.push(task),
    )).rejects.toMatchObject({ phase: 'work', cutoffAtMs: 10_600 })

    expect(acquisitionInvoked).toBe(false)
    expect(lateCleanup).toEqual([])
    deadline.dispose()
  })

  it('registers compensation before a late acquisition settles', async () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)
    let resolveAcquisition!: (resource: string) => void
    const acquisition = new Promise<string>((resolveResource) => {
      resolveAcquisition = resolveResource
    })
    const registered: Array<{ readonly boundary: string; readonly task: Promise<unknown> }> = []
    const rolledBack: string[] = []
    const running = acquireWholeSampleResource(
      deadline,
      () => acquisition,
      'late resource rollback',
      async (resource, signal) => {
        expect(signal.aborted).toBe(false)
        rolledBack.push(resource)
      },
      (boundary, task) => registered.push({ boundary, task }),
    )

    runtime.advanceTo(10_600)
    await expect(running).rejects.toMatchObject({ phase: 'work', cutoffAtMs: 10_600 })
    expect(registered).toHaveLength(1)
    expect(registered[0]?.boundary).toBe('late resource rollback')

    resolveAcquisition('resource-after-cutoff')
    await registered[0]?.task
    expect(rolledBack).toEqual(['resource-after-cutoff'])
    deadline.dispose()
  })

  it('keeps cleanup alive after work, then preserves a dedicated publication slice', async () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)

    runtime.advanceTo(10_600)
    expect(deadline.workSignal.aborted).toBe(true)
    expect(deadline.cleanupSignal.aborted).toBe(false)
    expect(deadline.publicationSignal.aborted).toBe(false)
    await expect(deadline.runCleanup(async (signal) => {
      expect(signal.aborted).toBe(false)
      return 'cleaned'
    })).resolves.toBe('cleaned')

    const cleanupStillRunning = deadline.runCleanup(() => new Promise<never>(() => {}))
    const cleanupCutoff = expect(cleanupStillRunning).rejects.toMatchObject({
      phase: 'cleanup',
      cutoffAtMs: 10_850,
    })
    runtime.advanceTo(10_850)
    expect(deadline.cleanupSignal.aborted).toBe(true)
    expect(deadline.publicationSignal.aborted).toBe(false)
    expect(deadline.remainingPublication()).toBe(100)
    await cleanupCutoff

    let expiredCleanupInvoked = false
    await expect(deadline.runCleanup(async (signal) => {
      expiredCleanupInvoked = true
      expect(signal.aborted).toBe(true)
      return 'closed synchronously'
    })).rejects.toMatchObject({ phase: 'cleanup', cutoffAtMs: 10_850 })
    expect(expiredCleanupInvoked).toBe(true)

    const publishing = deadline.runPublication(() => new Promise<never>(() => {}))
    const publicationCutoff = expect(publishing).rejects.toMatchObject({
      phase: 'publication',
      cutoffAtMs: 10_950,
    })
    runtime.advanceTo(10_949)
    expect(deadline.publicationSignal.aborted).toBe(false)
    expect(deadline.remainingPublication()).toBe(1)
    runtime.advanceTo(10_950)
    expect(deadline.publicationSignal.aborted).toBe(true)
    await publicationCutoff

    let expiredPublicationInvoked = false
    await expect(deadline.runPublication(async () => {
      expiredPublicationInvoked = true
      return 'published after cutoff'
    })).rejects.toMatchObject({ phase: 'publication', cutoffAtMs: 10_950 })
    expect(expiredPublicationInvoked).toBe(false)
    deadline.dispose()
  })

  it('observes an operation that rejects after its deadline wrapper settles', async () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)
    const unhandledRejections: unknown[] = []
    const captureUnhandled = (reason: unknown): void => {
      unhandledRejections.push(reason)
    }
    process.on('unhandledRejection', captureUnhandled)

    try {
      let rejectOperation!: (reason: unknown) => void
      const running = deadline.runWork(() => new Promise<never>((_resolve, reject) => {
        rejectOperation = reject
      }))
      const cutoff = expect(running).rejects.toMatchObject({ phase: 'work' })
      runtime.advanceTo(10_600)
      await cutoff

      rejectOperation(new Error('late operation failure'))
      await new Promise<void>((resolveTurn) => {
        setImmediate(resolveTurn)
      })
      expect(unhandledRejections).toEqual([])
    } finally {
      process.off('unhandledRejection', captureUnhandled)
      deadline.dispose()
    }
  })

  it.each([
    [
      'non-positive duration',
      { totalTimeoutMs: 0 },
      'Whole-sample totalTimeoutMs must be a positive safe integer',
    ],
    [
      'fractional duration',
      { evidencePublicationMs: 100.5 },
      'Whole-sample evidencePublicationMs must be a positive safe integer',
    ],
    [
      'unsafe duration',
      { completionMarginMs: Number.MAX_SAFE_INTEGER + 1 },
      'Whole-sample completionMarginMs must be a positive safe integer',
    ],
    [
      'work cutoff at sample start',
      { teardownReserveMs: 1_000 },
      'Whole-sample teardown reserve must be smaller than the total timeout',
    ],
    [
      'cleanup cutoff equal to work cutoff',
      { teardownReserveMs: 150, evidencePublicationMs: 100, completionMarginMs: 50 },
      'Whole-sample teardown reserve must exceed evidence publication plus completion margin',
    ],
  ] as const)('rejects invalid deadline geometry: %s', (_case, replacement, message) => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const timing = { ...WHOLE_SAMPLE_TIMING, ...replacement }

    expect(() => new WholeSampleDeadline(timing, {
      clock: runtime,
      scheduler: runtime,
    })).toThrow(message)
    expect(runtime.pendingTimerCount()).toBe(0)
  })

  it('disposes all active phase timers idempotently', () => {
    const runtime = new ManualDeadlineRuntime(10_000)
    const deadline = createWholeSampleDeadline(runtime)
    expect(runtime.pendingTimerCount()).toBe(3)

    deadline.dispose()
    deadline.dispose()

    expect(runtime.pendingTimerCount()).toBe(0)
    expect(deadline.workSignal.aborted).toBe(true)
    expect(deadline.cleanupSignal.aborted).toBe(true)
    expect(deadline.publicationSignal.aborted).toBe(true)
  })

  it('recognizes infrastructure authority through aggregate causes', () => {
    const failure = new AggregateError([
      new Error('product assertion'),
      new Error('cleanup wrapper', {
        cause: new FixtureInfrastructureError('runner guard disconnected'),
      }),
    ])

    expect(containsFixtureInfrastructureFailure(failure)).toBe(true)
    expect(containsFixtureInfrastructureFailure(new Error('product assertion'))).toBe(false)
  })

  it('drains diagnostic streams before treating the child as terminal', async () => {
    const writeTail = [
      'const { writeSync } = require("node:fs")',
      'writeSync(1, Buffer.alloc(2 * 1024 * 1024, 65))',
      'writeSync(1, "TAIL")',
    ].join('; ')
    const child = new ManagedProcess(process.execPath, ['-e', writeTail])

    await expect(child.waitFor('stdout', /TAIL/u, 2_000)).resolves.toBeDefined()
    await expect(child.stop()).resolves.toBeUndefined()
  })

  it('cancels process readiness at the whole-sample work cutoff', async () => {
    const child = new ManagedProcess(process.execPath, [
      '-e',
      'setInterval(() => undefined, 1000)',
    ])
    const cutoff = new AbortController()
    const reason = new Error('simulated work cutoff')
    const readiness = child.waitFor('stdout', /NEVER/u, 30_000, cutoff.signal)

    cutoff.abort(reason)

    await expect(readiness).rejects.toBe(reason)
    await expect(child.stop()).resolves.toBeUndefined()
  })

  it('kills an owned child even when cleanup is already out of time', async () => {
    const child = new ManagedProcess(process.execPath, [
      '-e',
      'process.stdout.write("READY\\n"); setInterval(() => undefined, 1000)',
    ])
    await child.waitFor('stdout', /^READY$/mu)
    const cutoff = new AbortController()
    const reason = new Error('simulated cleanup cutoff')
    cutoff.abort(reason)

    await expect(child.stop(10_000, cutoff.signal)).rejects.toBe(reason)
    await expect(child.waitForExit()).resolves.toBeUndefined()
  })
})

const WHOLE_SAMPLE_TIMING: WholeSampleDeadlineTiming = Object.freeze({
  totalTimeoutMs: 1_000,
  teardownReserveMs: 400,
  evidencePublicationMs: 100,
  completionMarginMs: 50,
})

interface ManualDeadlineTimer {
  readonly sequence: number
  readonly dueAt: number
  readonly callback: () => void
}

class ManualDeadlineRuntime implements WholeSampleDeadlineClock, WholeSampleDeadlineScheduler {
  #now: number
  #nextSequence = 1
  readonly #timers = new Map<number, ManualDeadlineTimer>()

  constructor(startedAt: number) {
    this.#now = startedAt
  }

  now(): number {
    return this.#now
  }

  schedule(callback: () => void, delayMs: number): WholeSampleDeadlineTimer {
    const timer: ManualDeadlineTimer = {
      sequence: this.#nextSequence,
      dueAt: this.#now + delayMs,
      callback,
    }
    this.#nextSequence += 1
    this.#timers.set(timer.sequence, timer)
    return { cancel: () => this.#timers.delete(timer.sequence) }
  }

  advanceTo(target: number): void {
    if (target < this.#now) throw new RangeError('Manual deadline clock cannot move backwards')
    for (;;) {
      const due = [...this.#timers.values()]
        .filter((timer) => timer.dueAt <= target)
        .sort((left, right) => left.dueAt - right.dueAt || left.sequence - right.sequence)[0]
      if (due === undefined) break
      this.#now = due.dueAt
      this.#timers.delete(due.sequence)
      due.callback()
    }
    this.#now = target
  }

  pendingTimerCount(): number {
    return this.#timers.size
  }
}

function createWholeSampleDeadline(runtime: ManualDeadlineRuntime): WholeSampleDeadline {
  return new WholeSampleDeadline(WHOLE_SAMPLE_TIMING, {
    clock: runtime,
    scheduler: runtime,
  })
}

describe('sender evidence terminal barrier', () => {
  const cleanup: string[] = []

  afterEach(async () => {
    await Promise.all(cleanup.splice(0).map((path) => rm(path, { recursive: true, force: true })))
  })

  it('waits for the sender terminal of every participating browser attempt', async () => {
    const collector = new HotSwitchEvidenceCollector()
    const events = admittedEvents()
    for (const evidence of sideEvents(events, 'browser')) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    const sender = sideEvents(events, 'sender')
    let reads = 0

    const complete = await collector.waitForSenderTerminals(async () => {
      reads += 1
      return {
        records: reads === 1 ? sender.slice(0, -1) : sender,
        hasUnterminatedFinalRecord: false,
      }
    }, 1_000)

    expect(reads).toBe(2)
    expect(complete).toEqual(sender)
  })

  it('does not invent a sender terminal for a browser-only pre-offer failure', async () => {
    const collector = new HotSwitchEvidenceCollector()
    for (const evidence of browserFailureEvents()) {
      collector.acceptPageEvent({ kind: 'attempt', evidence })
    }
    let reads = 0

    const complete = await collector.waitForSenderTerminals(async () => {
      reads += 1
      return { records: [], hasUnterminatedFinalRecord: false }
    }, 1_000)

    expect(reads).toBe(1)
    expect(complete).toEqual([])
  })

  it('keeps an unterminated JSONL fragment outside the parsed snapshot', async () => {
    const directory = await mkdtemp(join(tmpdir(), 'windshare-sender-evidence-test-'))
    cleanup.push(directory)
    const path = join(directory, 'attempts.jsonl')
    const sender = sideEvents(admittedEvents(), 'sender')
    const encoded = `${sender.map((event) => JSON.stringify(event)).join('\n')}\n`
    const truncated = encoded.slice(0, -8)
    await writeFile(path, truncated, 'utf8')

    const incomplete = await readSenderAttemptEvidenceSnapshot(path)
    expect(incomplete.hasUnterminatedFinalRecord).toBe(true)
    expect(incomplete.records).toHaveLength(sender.length - 1)

    await writeFile(path, encoded, 'utf8')
    const complete = await readSenderAttemptEvidenceSnapshot(path)
    expect(complete.hasUnterminatedFinalRecord).toBe(false)
    expect(complete.records).toEqual(sender)
  })

  it('rejects malformed newline-terminated sender evidence immediately', async () => {
    const directory = await mkdtemp(join(tmpdir(), 'windshare-sender-evidence-test-'))
    cleanup.push(directory)
    const path = join(directory, 'attempts.jsonl')
    await writeFile(path, '{"not":}\n', 'utf8')

    await expect(readSenderAttemptEvidenceSnapshot(path)).rejects.toThrow(
      'Sender attempt evidence line 1 is invalid JSON',
    )
  })
})

describe('relay proxy cut fence', () => {
  const cleanup: Array<() => Promise<unknown> | unknown> = []

  afterEach(async () => {
    await Promise.allSettled(cleanup.splice(0).reverse().map((operation) => operation()))
  })

  it('waits for receiver ineligibility after closing acceptance and active paths', async () => {
    const upstream = createServer((socket) => socket.resume())
    cleanup.push(() => new Promise<void>((resolveClose) => upstream.close(() => resolveClose())))
    await new Promise<void>((resolveListen, rejectListen) => {
      upstream.once('error', rejectListen)
      upstream.listen(0, '127.0.0.1', () => {
        upstream.off('error', rejectListen)
        resolveListen()
      })
    })
    const upstreamAddress = upstream.address()
    if (upstreamAddress === null || typeof upstreamAddress === 'string') {
      throw new Error('Test upstream did not expose a TCP address')
    }

    const proxy = await RelayProxy.start(`ws://127.0.0.1:${upstreamAddress.port}`)
    cleanup.push(() => proxy.close())
    const client = connect(Number(new URL(proxy.url).port), '127.0.0.1')
    cleanup.push(() => client.destroy())
    await once(client, 'connect')
    const clientClosed = once(client, 'close')

    let detach!: () => void
    const receiverRelayIneligible = new Promise<void>((resolveDetach) => { detach = resolveDetach })
    const fence = proxy.cutAndWait(() => receiverRelayIneligible)
    expect(proxy.accepting).toBe(false)
    await expect(clientClosed).resolves.toBeDefined()

    const refused = connect(Number(new URL(proxy.url).port), '127.0.0.1')
    cleanup.push(() => refused.destroy())
    const refusal = once(refused, 'error')
    await expect(refusal).resolves.toBeDefined()
    await expect(Promise.race([
      fence.then(() => 'settled'),
      Promise.resolve('waiting-for-receiver'),
    ])).resolves.toBe('waiting-for-receiver')

    detach()
    await expect(fence).resolves.toEqual({
      proxyAccepting: false,
      receiverRelayEligible: false,
    })
  })

  it('destroys proxy ownership when the work cutoff has already elapsed', async () => {
    const upstream = createServer((socket) => socket.resume())
    cleanup.push(() => new Promise<void>((resolveClose) => upstream.close(() => resolveClose())))
    await new Promise<void>((resolveListen, rejectListen) => {
      upstream.once('error', rejectListen)
      upstream.listen(0, '127.0.0.1', () => {
        upstream.off('error', rejectListen)
        resolveListen()
      })
    })
    const upstreamAddress = upstream.address()
    if (upstreamAddress === null || typeof upstreamAddress === 'string') {
      throw new Error('Test upstream did not expose a TCP address')
    }
    const proxy = await RelayProxy.start(`ws://127.0.0.1:${upstreamAddress.port}`)
    cleanup.push(() => proxy.close())
    const client = connect(Number(new URL(proxy.url).port), '127.0.0.1')
    cleanup.push(() => client.destroy())
    await once(client, 'connect')
    const clientClosed = once(client, 'close')
    const cutoff = new AbortController()
    const reason = new Error('simulated work cutoff')
    cutoff.abort(reason)
    let receiverSealInvoked = false

    const fence = proxy.cutAndWait(() => {
      receiverSealInvoked = true
      return Promise.resolve()
    }, cutoff.signal)

    expect(proxy.accepting).toBe(false)
    await expect(fence).rejects.toBe(reason)
    expect(receiverSealInvoked).toBe(false)
    await expect(clientClosed).resolves.toBeDefined()
  })
})

function dispatchEvent(
  dispatchSequence: number,
  route: 'relay' | 'peer',
  laneId: number,
  laneEpoch: number,
): HotSwitchPageEvent {
  return {
    kind: 'dispatch',
    observation: { dispatchSequence, route, laneId, laneEpoch },
  }
}

function laneEvent(
  kind: 'lane-admitted' | 'lane-detached',
  route: 'relay' | 'peer',
  laneId: number,
  laneEpoch: number,
): HotSwitchPageEvent {
  return { kind, observation: { route, laneId, laneEpoch } }
}

function sideEvents(
  events: readonly AttemptEvidence[],
  side: AttemptEvidence['side'],
): readonly AttemptEvidence[] {
  return events.filter((event) => event.side === side)
}

async function createPublishedTopologyFixture(
  cleanup: string[],
  options: {
    readonly mutateProfile?: boolean
    readonly wrongProfileDigest?: boolean
    readonly wrongResolutionDigest?: boolean
  } = {},
): Promise<{
  readonly context: {
    readonly topologyProfilePath: string
    readonly topologyResolutionPath: string
    readonly topologyProfileSha256: string
    readonly topologyResolutionSha256: string
  }
}> {
  const directory = await mkdtemp(join(tmpdir(), 'windshare-published-topology-test-'))
  cleanup.push(directory)
  const [encodedProfile, encodedResolution] = await Promise.all([
    readFile(CANONICAL_TOPOLOGY_PROFILE, 'utf8'),
    readFile(EXAMPLE_TOPOLOGY_RESOLUTION, 'utf8'),
  ])
  const profile = parseTestIceTopologyJson(encodedProfile)
  const profileSha256 = await testIceTopologySha256(profile)
  const resolution = parseTestIceTopologyResolutionJson(
    encodedResolution,
    profile,
    profileSha256,
  )
  const resolutionSha256 = await testIceTopologyResolutionSha256(
    resolution,
    profile,
    profileSha256,
  )
  const topologyProfilePath = join(directory, 'published-profile.json')
  const topologyResolutionPath = join(directory, 'published-resolution.json')
  await Promise.all([
    writeFile(
      topologyProfilePath,
      options.mutateProfile === true ? `${encodedProfile}\n` : encodedProfile,
      'utf8',
    ),
    writeFile(topologyResolutionPath, encodedResolution, 'utf8'),
  ])
  return Object.freeze({
    context: Object.freeze({
      topologyProfilePath,
      topologyResolutionPath,
      topologyProfileSha256: options.wrongProfileDigest === true ? WRONG_SHA256 : profileSha256,
      topologyResolutionSha256: options.wrongResolutionDigest === true
        ? WRONG_SHA256
        : resolutionSha256,
    }),
  })
}

function publishedTopologyEnvironment(context: {
  readonly topologyProfilePath: string
  readonly topologyResolutionPath: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
}): Record<string, string> {
  return {
    [TOPOLOGY_ENV_NAMES.profile]: context.topologyProfilePath,
    [TOPOLOGY_ENV_NAMES.resolution]: context.topologyResolutionPath,
    [TOPOLOGY_ENV_NAMES.profileSha256]: context.topologyProfileSha256,
    [TOPOLOGY_ENV_NAMES.resolutionSha256]: context.topologyResolutionSha256,
  }
}
