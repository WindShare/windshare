import { createHash } from 'node:crypto'
import { mkdir } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'

import { expect, test as base } from '@playwright/test'
import type { Page, TestInfo } from '@playwright/test'

import type {
  AttemptEvidence,
  BrowserAttemptEvidence,
} from '../scripts/browser-evidence/attempt-evidence'
import {
  reducePeerAttemptOutcome,
  type LogicalAttempt,
} from '../scripts/browser-evidence/attempt-collector'
import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  ChildEvidenceReporter,
  publicBrowserDiagnosticSink,
} from '../scripts/browser-evidence/child-evidence'
import type { MainRouteEvidence } from '../scripts/browser-evidence/route-evidence'
import {
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
} from '../scripts/browser-evidence/result'
import {
  selectedPairAllowedByTopology,
  type VerifiedTestIceTopologyLock,
} from '../scripts/browser-evidence/test-ice-topology'
import { BROWSER_ENGINES, type RtcCapability } from '../scripts/browser-evidence/vocabulary'
import { classifyNativePeerConnection } from '../test/transport/webrtc/browser-capability'
import type { NativeRtcCapability } from '../test/transport/webrtc/browser-capability'
import {
  acquireWholeSampleResource,
  HotSwitchEvidenceCollector,
  WholeSampleDeadline,
  WholeSampleDeadlineExpiredError,
  type BrowserAttemptTerminal,
  type HotSwitchDeliveryTerminal,
  type HotSwitchPageEvent,
  type HotSwitchRuntimeTerminal,
  type ObservedTransferFailure,
} from './fixtures/hot-switch-evidence'
import { acquireTestIceTopology } from './fixtures/test-ice-topology-runtime'
import {
  V2RealStack,
  acquireRealStackBinaries,
  readSenderAttemptEvidenceSnapshot,
  releaseRealStackBinaries,
  replaceRelayHint,
} from './fixtures/v2-real-stack'
import {
  FixtureInfrastructureError,
  containsFixtureInfrastructureFailure,
} from './fixtures/managed-process'
import type { BinaryPaths } from './fixtures/windows-stable-runner'

const TRANSFER_BYTES = MAIN_TRANSFER_BYTES
const FIRST_RELAY_DISPATCH_DEADLINE_MS = 20_000
const PEER_TERMINAL_DEADLINE_MS = 15_000
const RELAY_INELIGIBILITY_DEADLINE_MS = 10_000
const POST_FENCE_PEER_DISPATCH_DEADLINE_MS = 15_000
const SENDER_EVIDENCE_TERMINAL_DEADLINE_MS = 10_000
const SAMPLE_TEARDOWN_HEADROOM_MS = 20_000
const SAMPLE_EVIDENCE_PUBLICATION_HEADROOM_MS = 2_000
const SAMPLE_PLAYWRIGHT_COMPLETION_HEADROOM_MS = 1_000
const FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH = 4
const HOT_SWITCH_TRACE_RELATIVE_PATH = 'main/hot-switch-trace.zip'

interface PromiseSettlement<T> {
  readonly value?: T
  readonly error?: unknown
}

interface RegisteredLateCleanup {
  readonly boundary: string
  readonly settlement: Promise<PromiseSettlement<unknown>>
}

type RegisterLateCleanup = (boundary: string, task: Promise<unknown>) => void

class IntermediateEvidenceGate {
  readonly #reporter: ChildEvidenceReporter | null
  #open = true

  constructor(reporter: ChildEvidenceReporter | null) {
    this.#reporter = reporter
  }

  close(): void {
    this.#open = false
  }

  publish(operation: (reporter: ChildEvidenceReporter) => void): void {
    if (!this.#open || this.#reporter === null) return
    operation(this.#reporter)
  }

  recordAttempt(evidence: AttemptEvidence): void {
    this.publish((reporter) => reporter.recordAttempt(evidence))
  }
}

interface HotSwitchTestFixtures {
  readonly wholeSampleDeadline: WholeSampleDeadline
}

const test = base.extend<HotSwitchTestFixtures>({
  wholeSampleDeadline: [async ({ browserName }, use, testInfo) => {
    // Automatic test fixtures are resolved before the non-auto page/context
    // fixtures. Anchoring here preserves teardown time consumed by browser setup.
    if (!BROWSER_ENGINES.includes(browserName)) {
      throw new Error(`unsupported browser engine ${JSON.stringify(browserName)}`)
    }
    const deadline = new WholeSampleDeadline({
      totalTimeoutMs: testInfo.timeout,
      teardownReserveMs: SAMPLE_TEARDOWN_HEADROOM_MS,
      evidencePublicationMs: SAMPLE_EVIDENCE_PUBLICATION_HEADROOM_MS,
      completionMarginMs: SAMPLE_PLAYWRIGHT_COMPLETION_HEADROOM_MS,
    })
    try {
      await use(deadline)
    } finally {
      deadline.dispose()
    }
  }, { auto: true }],
})

interface CollectedSample {
  readonly capability: NativeRtcCapability
  readonly expectedSha256: string
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly attempts: readonly LogicalAttempt[]
  readonly peerTerminal: PromiseSettlement<BrowserAttemptTerminal> | null
  readonly delivery: PromiseSettlement<HotSwitchDeliveryTerminal>
  readonly runtime: PromiseSettlement<HotSwitchRuntimeTerminal>
  readonly routeEvidence: MainRouteEvidence
  readonly routeError?: unknown
  readonly attemptError?: unknown
  readonly orchestrationErrors: readonly unknown[]
}

test.use({
  // The separate capability key enters the page before tracing starts. Failure
  // evidence remains useful without serializing the secret-bearing setup call.
  trace: 'off',
  screenshot: 'only-on-failure',
  video: 'retain-on-failure',
})

test('proves classified product hot-switch or exact relay fallback', async ({
  baseURL,
  browserName,
  page,
  wholeSampleDeadline: deadline,
}, testInfo) => {
  const reporter = optionalChildReporter()
  const intermediateEvidence = new IntermediateEvidenceGate(reporter)
  const collector = new HotSwitchEvidenceCollector(intermediateEvidence)
  const diagnostics = observePublicBrowserDiagnostics(page, reporter, (error) => collector.abort(error))
  const failures: unknown[] = []
  const lateCleanupTasks: RegisteredLateCleanup[] = []
  const registerLateCleanup: RegisterLateCleanup = (boundary, task) => {
    // Attach rejection handling at registration time; the cleanup phase may not
    // begin draining this task until after other resource owners have stopped.
    lateCleanupTasks.push(Object.freeze({ boundary, settlement: settle(task) }))
  }
  let forcedPageClose: Promise<PromiseSettlement<void>> | undefined
  let forcedContextClose: Promise<PromiseSettlement<void>> | undefined
  const abortWork = () => {
    intermediateEvidence.close()
    collector.abort(deadline.workSignal.reason)
    diagnostics.expectTargetClose()
    forcedPageClose ??= settle(page.close({ runBeforeUnload: false }))
  }
  const abortCleanup = () => {
    // Closing the context is the cancellation authority for Playwright tracing
    // and evaluate calls, which do not accept an AbortSignal themselves.
    diagnostics.expectTargetClose()
    forcedContextClose ??= settle(page.context().close())
  }
  deadline.workSignal.addEventListener('abort', abortWork, { once: true })
  deadline.cleanupSignal.addEventListener('abort', abortCleanup, { once: true })
  if (deadline.workSignal.aborted) abortWork()
  if (deadline.cleanupSignal.aborted) abortCleanup()
  let ownedBinaries: Awaited<ReturnType<typeof acquireRealStackBinaries>> | undefined
  try {
    try {
      fixtureValue('Child evidence identity validation failed', () => {
        validateReporterIdentity(reporter, browserName, testInfo)
      })
      if (baseURL === undefined) {
        throw new FixtureInfrastructureError('Real-stack browser project requires a base URL')
      }
      ownedBinaries = await acquireFixtureWork(
        deadline,
        'Real-stack binary acquisition failed',
        (signal) => acquireRealStackBinaries(signal),
        'Late real-stack binary acquisition rollback failed',
        releaseRealStackBinaries,
        registerLateCleanup,
      )
      await runHotSwitchSample({
        baseURL,
        binaries: ownedBinaries,
        collector,
        deadline,
        intermediateEvidence,
        page,
        reporter,
        registerLateCleanup,
        testInfo,
      })
    } catch (error) {
      failures.push(error)
    }
    failures.push(...await releaseOwnedBinaries(deadline, ownedBinaries))
    failures.push(...await drainLateCleanupTasks(deadline, lateCleanupTasks))
    failures.push(...await drainTimedOutPlaywrightOwners({
      abortCleanup,
      abortWork,
      deadline,
      pendingContextClose: () => forcedContextClose,
      pendingPageClose: () => forcedPageClose,
    }))
    // Publication is the final writer. Work-cutoff paths close this gate earlier;
    // normal failures close it only after every bounded producer has drained.
    intermediateEvidence.close()
    diagnostics.close()
    failures.push(...await publishChildBoundary(deadline, reporter, failures))
    if (failures.length > 0) throw aggregateFailure(failures, 'Hot-switch sample boundary failed')
  } finally {
    diagnostics.close()
    deadline.workSignal.removeEventListener('abort', abortWork)
    deadline.cleanupSignal.removeEventListener('abort', abortCleanup)
  }
})

async function releaseOwnedBinaries(
  deadline: WholeSampleDeadline,
  binaries: Awaited<ReturnType<typeof acquireRealStackBinaries>> | undefined,
): Promise<readonly unknown[]> {
  if (binaries === undefined) return []
  try {
    await fixtureCleanup(
      deadline,
      'Real-stack binary release failed',
      () => releaseRealStackBinaries(binaries),
    )
    return []
  } catch (error) {
    return [error]
  }
}

async function drainLateCleanupTasks(
  deadline: WholeSampleDeadline,
  tasks: readonly RegisteredLateCleanup[],
): Promise<readonly unknown[]> {
  if (tasks.length === 0) return []
  try {
    const settlements = await fixtureCleanup(
      deadline,
      'Late resource rollback drain failed',
      () => Promise.all(tasks.map((task) => task.settlement)),
    )
    return settlements.flatMap((settlement, index) => {
      if (settlement.error === undefined) return []
      return [new FixtureInfrastructureError(
        tasks[index]?.boundary ?? 'Late resource rollback failed',
        settlement.error,
      )]
    })
  } catch (error) {
    return [error]
  }
}

async function drainTimedOutPlaywrightOwners(options: {
  readonly abortCleanup: () => void
  readonly abortWork: () => void
  readonly deadline: WholeSampleDeadline
  readonly pendingContextClose: () => Promise<PromiseSettlement<void>> | undefined
  readonly pendingPageClose: () => Promise<PromiseSettlement<void>> | undefined
}): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  // Work can no longer create page ownership. Keep the cleanup listener live
  // while page close drains so context close remains available at the cutoff.
  options.deadline.workSignal.removeEventListener('abort', options.abortWork)
  failures.push(...await drainForcedPlaywrightClose(
    options.deadline,
    'Timed-out receiver page close failed',
    options.pendingPageClose(),
  ))
  options.deadline.cleanupSignal.removeEventListener('abort', options.abortCleanup)
  // abortCleanup may have created this owner during the preceding await.
  failures.push(...await drainForcedPlaywrightClose(
    options.deadline,
    'Timed-out receiver context close failed',
    options.pendingContextClose(),
  ))
  return failures
}

async function drainForcedPlaywrightClose(
  deadline: WholeSampleDeadline,
  boundary: string,
  pending: Promise<PromiseSettlement<void>> | undefined,
): Promise<readonly unknown[]> {
  if (pending === undefined) return []
  try {
    const closed = await fixtureCleanup(deadline, boundary, () => pending)
    return closed.error === undefined
      ? []
      : [new FixtureInfrastructureError(boundary, closed.error)]
  } catch (error) {
    return [error]
  }
}

async function publishChildBoundary(
  deadline: WholeSampleDeadline,
  reporter: ChildEvidenceReporter | null,
  failures: readonly unknown[],
): Promise<readonly unknown[]> {
  const publicationFailures: unknown[] = []
  const observedFailure = failures.length === 0
    ? undefined
    : aggregateFailure(failures, 'Hot-switch sample boundary failed')
  if (observedFailure !== undefined) {
    try {
      await fixturePublication(deadline, 'Child failure evidence publication failed', () => {
        if (containsFixtureInfrastructureFailure(observedFailure)) {
          reporter?.recordInfrastructureFailure(observedFailure)
        }
      })
    } catch (error) {
      publicationFailures.push(error)
    }
  }
  if (reporter !== null) {
    try {
      await fixturePublication(deadline, 'Child lifecycle publication failed', async () => {
        reporter.completeLifecycle()
        await reporter.flush()
      })
    } catch (error) {
      publicationFailures.push(error)
    }
  }
  return publicationFailures
}

async function runHotSwitchSample(options: {
  readonly baseURL: string
  readonly binaries: BinaryPaths
  readonly collector: HotSwitchEvidenceCollector
  readonly deadline: WholeSampleDeadline
  readonly intermediateEvidence: IntermediateEvidenceGate
  readonly page: Page
  readonly reporter: ChildEvidenceReporter | null
  readonly registerLateCleanup: RegisterLateCleanup
  readonly testInfo: TestInfo
}): Promise<CollectedSample> {
  const capability = await fixtureWork(
    options.deadline,
    'RTC capability classification failed',
    () => classifyNativePeerConnection(options.page),
  )
  fixtureValue('Capability evidence publication failed', () => {
    options.intermediateEvidence.publish((reporter) => reporter.recordCapability(capability.evidence))
  })
  const topology = await acquireFixtureWork(
    options.deadline,
    'Test ICE topology acquisition failed',
    (signal) => acquireTestIceTopology(options.reporter?.context, process.env, signal),
    'Late test ICE topology acquisition rollback failed',
    (acquired) => acquired.release(),
    options.registerLateCleanup,
  )
  let stack: V2RealStack
  let expected: Uint8Array
  let expectedHash: string
  try {
    fixtureValue('Published topology validation failed', () => {
      validateReporterTopology(options.reporter, topology.lock)
    })
    expected = fixtureValue(
      'Transfer payload fixture creation failed',
      () => deterministicBytes(TRANSFER_BYTES),
    )
    expectedHash = fixtureValue(
      'Transfer payload digest creation failed',
      () => {
        const actual = createHash('sha256').update(expected).digest('hex')
        if (actual !== MAIN_TRANSFER_SHA256) {
          throw new Error('deterministic transfer payload differs from its semantic authority')
        }
        return actual
      },
    )
    stack = fixtureValue(
      'Real-stack construction failed',
      () => new V2RealStack(options.binaries, topology),
    )
  } catch (error) {
    try {
      await fixtureCleanup(options.deadline, 'Test ICE topology release failed', topology.release)
    } catch (releaseError) {
      throw aggregateFailure(
        [error, releaseError],
        'Hot-switch initialization and topology cleanup failed',
      )
    }
    throw error
  }
  let collected: CollectedSample | undefined
  let operationError: unknown
  let operationFailed = false
  let cleanupStarted = false
  const beginFixtureCleanup = (): readonly Promise<unknown>[] => {
    cleanupStarted = true
    const stackDisposal = settle(stack.dispose({
      signal: options.deadline.cleanupSignal,
    }))
    // All owners enter teardown in the same turn. Trace retention can be slow
    // on a sick browser and must not postpone local child termination.
    return [
      fixtureCleanup(
        options.deadline,
        'Receiver output cleanup failed',
        () => releasePageOutput(options.page),
      ),
      fixtureCleanup(
        options.deadline,
        'Real-stack cleanup failed',
        async () => {
          const disposal = await stackDisposal
          if (disposal.error !== undefined) throw disposal.error
        },
      ),
      fixtureCleanup(
        options.deadline,
        'Test ICE topology cleanup failed',
        async () => {
          // A partially started sender may still be opening these files. Await
          // actual child close, not the deadline wrapper, before deleting them.
          await stackDisposal
          await topology.release()
        },
      ),
    ]
  }
  try {
    await fixtureWork(options.deadline, 'Relay startup failed', (signal) => stack.start({
      signal,
      timeoutMilliseconds: options.deadline.remainingWork(),
    }))
    const proxy = await fixtureWork(
      options.deadline,
      'Relay proxy startup failed',
      (signal) => stack.createRelayProxy({ signal }),
    )
    const filePath = await fixtureWork(
      options.deadline,
      'Shared-file fixture creation failed',
      (signal) => stack.createFile('hot-switch.bin', expected, { signal }),
    )
    const share = await fixtureWork(
      options.deadline,
      'Sender startup failed',
      (signal) => stack.share(filePath, options.baseURL, {
        signal,
        timeoutMilliseconds: options.deadline.remainingWork(),
      }),
    )
    const receiverLink = replaceRelayHint(share.bareLink, proxy.url)

    await fixtureWork(
      options.deadline,
      'Hot-switch evidence bridge installation failed',
      () => options.page.exposeFunction(
        '__windshareHotSwitchEvent',
        (event: unknown) => options.collector.acceptPageEvent(event),
      ),
    )
    await fixtureWork(options.deadline, 'Receiver navigation failed', () => options.page.goto(
      receiverLink,
      { timeout: options.deadline.remainingWork() },
    ))
    await fixtureWork(
      options.deadline,
      'Hot-switch page runtime initialization failed',
      () => startPageTransfer(options.page, {
        expectedHash,
        key: share.key,
        rtcConfiguration: topology.rtcConfiguration,
      }),
    )
    const releaseOutput = () => fixtureWork(
      options.deadline,
      'Receiver output release failed',
      () => releasePageOutput(options.page),
    )

    collected = await runWithSanitizedFailureTrace(
      {
        deadline: options.deadline,
        page: options.page,
        reporter: options.reporter,
        registerLateCleanup: options.registerLateCleanup,
        testInfo: options.testInfo,
        beginFixtureCleanup,
      },
      async () => {
        // Delivery and runtime share one absolute sample budget. Starting both
        // now prevents sequential evidence gaps from reaching Playwright's
        // global timeout before the terminal authorities are observed.
        const deliveryWait = settle(options.collector.waitForDelivery(
          options.deadline.remainingWork(),
        ))
        const runtimeWait = settle(options.collector.waitForRuntimeSettlement(
          options.deadline.remainingWork(),
        ))
        // Settle rather than throw at the first barrier. The collector retains
        // producer events, so a missing relay dispatch can still proceed through
        // full peer, delivery, and runtime terminal collection without starting
        // their named deadlines before the semantic barrier they bound.
        const firstRelayWait = settle(options.collector.waitForFirstRelayDispatch(
          options.deadline.remainingWork(FIRST_RELAY_DISPATCH_DEADLINE_MS),
        ))
        const firstRelay = await firstRelayWait
        const peerWait = capability.evidence.apiPresence === 'present'
          ? settle(options.collector.waitForBrowserTerminal(
              options.deadline.remainingWork(PEER_TERMINAL_DEADLINE_MS),
            ))
          : null
        let peerTerminal: PromiseSettlement<BrowserAttemptTerminal> | null = null
        const orchestrationErrors: unknown[] = []
        let cutCompleted = false

        if (firstRelay.error !== undefined || firstRelay.value === undefined) {
          orchestrationErrors.push(
            firstRelay.error ?? new Error('The first relay dispatch settled without evidence'),
          )
          await releaseOutput().catch((error) => orchestrationErrors.push(error))
        } else if (capability.rtcCapability === 'available' && peerWait !== null) {
          peerTerminal = await peerWait
          const terminal = peerTerminal.value
          if (terminal?.stage === 'admitted') {
            try {
              const fence = await fixtureWork(
                options.deadline,
                'Relay cut fence failed',
                (signal) => proxy.cutAndWait(async () => {
                  const receiverRelayIneligible = settle(
                    options.collector.waitForRelayIneligibility(
                      options.deadline.remainingWork(RELAY_INELIGIBILITY_DEADLINE_MS),
                    ),
                  )
                  await sealPageRelayCut(options.page)
                  const receiver = await receiverRelayIneligible
                  if (receiver.error !== undefined) throw receiver.error
                }, signal),
              )
              const boundary = options.collector.latestDispatchSequence()
              options.collector.recordRelayCutFence(boundary, fence)
              cutCompleted = true
              const postFencePeer = settle(
                options.collector.waitForPostFencePeerDispatch(
                  terminal.lane,
                  boundary,
                  options.deadline.remainingWork(POST_FENCE_PEER_DISPATCH_DEADLINE_MS),
                ),
              )
              await releaseOutput()
              const postFence = await postFencePeer
              if (postFence.error !== undefined) orchestrationErrors.push(postFence.error)
            } catch (error) {
              orchestrationErrors.push(error)
              await releaseOutput().catch((releaseError) => {
                orchestrationErrors.push(releaseError)
              })
            }
          } else {
            if (peerTerminal.error !== undefined) orchestrationErrors.push(peerTerminal.error)
            await releaseOutput().catch((error) => orchestrationErrors.push(error))
          }
        } else {
          // Probe failure never suppresses the real product attempt. It only means
          // output must remain available to the relay while that attempt terminates.
          await releaseOutput().catch((error) => orchestrationErrors.push(error))
        }

        const pendingPeer = peerTerminal === null && peerWait !== null
          ? peerWait
          : Promise.resolve(peerTerminal)
        const [settledPeer, delivery, runtime] = await Promise.all([
          pendingPeer,
          deliveryWait,
          runtimeWait,
        ])
        peerTerminal = settledPeer

        let routeEvidence: MainRouteEvidence
        let routeError: unknown
        try {
          routeEvidence = options.collector.routeEvidence(cutCompleted ? 'hot-switch' : 'relay-only')
        } catch (error) {
          routeError = error
          routeEvidence = Object.freeze({ mode: 'relay-only', observations: Object.freeze([]) })
        }

        let attempts: readonly LogicalAttempt[] = Object.freeze([])
        let attemptError: unknown
        try {
          const senderEvidence = await options.collector.waitForSenderTerminals(
            () => fixtureWork(
              options.deadline,
              'Sender attempt evidence read failed',
              (signal) => readSenderAttemptEvidenceSnapshot(share.senderEvidencePath, signal),
            ),
            options.deadline.remainingWork(SENDER_EVIDENCE_TERMINAL_DEADLINE_MS),
          )
          options.collector.ingestSenderEvidence(senderEvidence)
          attempts = options.collector.finalizeAttempts()
        } catch (error) {
          attemptError = error
        }

        // Sender JSONL is replayed only after its producer terminal is stable.
        // Publish delivery and route after that replay so the child log keeps
        // the contract order: capability, complete attempts, delivery, route.
        fixtureValue('Completed child authority publication failed', () => {
          recordCompletedAuthorities(
            options.intermediateEvidence,
            delivery.value,
            routeError === undefined ? routeEvidence : undefined,
          )
        })

        const sample = Object.freeze({
          capability,
          expectedSha256: expectedHash,
          topologyLock: topology.lock,
          attempts,
          peerTerminal,
          delivery,
          runtime,
          routeEvidence,
          ...(routeError === undefined ? {} : { routeError }),
          ...(attemptError === undefined ? {} : { attemptError }),
          orchestrationErrors: Object.freeze(orchestrationErrors),
        })
        // Acceptance belongs inside the sanitized trace boundary. Otherwise a
        // truthful peer, route, or delivery failure would discard its trace
        // before the test turns that evidence into a blocking verdict.
        assertAcceptedSample(sample)
        return sample
      },
    )
  } catch (error) {
    operationFailed = true
    operationError = error
  }
  if (!cleanupStarted) {
    const cleanup = await Promise.allSettled(beginFixtureCleanup())
    const cleanupErrors = cleanup.flatMap((result) =>
      result.status === 'rejected'
        ? [new FixtureInfrastructureError('Hot-switch fixture cleanup failed', result.reason)]
        : [],
    )
    if (operationFailed || cleanupErrors.length > 0) {
      throw aggregateFailure(
        [...(operationFailed ? [operationError] : []), ...cleanupErrors],
        !operationFailed
          ? 'Hot-switch fixture cleanup failed'
          : 'Hot-switch sample and cleanup failed',
      )
    }
  }
  if (operationFailed) throw operationError
  if (collected === undefined) throw new Error('Hot-switch sample produced no collected evidence')
  return collected
}

async function startPageTransfer(
  page: Page,
  input: {
    readonly expectedHash: string
    readonly key: string
    readonly rtcConfiguration: RTCConfiguration
  },
): Promise<void> {
  await page.evaluate(startPageTransferInBrowser, {
    ...input,
    failureDiagnosticMaximumDepth: FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH,
    transferBytes: TRANSFER_BYTES,
  })
}

function startPageTransferInBrowser({
  expectedHash,
  failureDiagnosticMaximumDepth,
  key,
  rtcConfiguration,
  transferBytes,
}: {
  readonly expectedHash: string
  readonly failureDiagnosticMaximumDepth: number
  readonly key: string
  readonly rtcConfiguration: RTCConfiguration
  readonly transferBytes: number
}): void {
      interface HotSwitchWindow extends Window {
        __windshareHotSwitchEvent?: (event: HotSwitchPageEvent) => Promise<void>
        __windshareReleaseHotSwitchOutput?: () => void
        __windshareSealHotSwitchRelayCut?: () => Promise<void>
      }

      const hotSwitchWindow = window as HotSwitchWindow
      const bridge = hotSwitchWindow.__windshareHotSwitchEvent
      if (bridge === undefined) throw new Error('Hot-switch evidence bridge is unavailable')

      const describeFailure = (reason: unknown, depth = 0): string => {
        if (depth >= failureDiagnosticMaximumDepth) return '[nested failure truncated]'
        if (reason instanceof AggregateError) {
          const nested = reason.errors.map((error) => describeFailure(error, depth + 1))
          const summary = `${reason.name}: ${reason.message}`
          const failures = nested.length === 0 ? summary : `${summary}; errors=[${nested.join(' | ')}]`
          return reason.cause === undefined
            ? failures
            : `${failures}; cause=${describeFailure(reason.cause, depth + 1)}`
        }
        if (reason instanceof Error) {
          const summary = `${reason.name}: ${reason.message}`
          return reason.cause === undefined
            ? summary
            : `${summary}; cause=${describeFailure(reason.cause, depth + 1)}`
        }
        try {
          return String(reason)
        } catch {
          return '[unprintable non-Error failure]'
        }
      }

      let bridgeQueue = Promise.resolve()
      let bridgeFailure: string | undefined
      let runtimeTerminalPublished = false
      const publish = (event: HotSwitchPageEvent): Promise<void> => {
        bridgeQueue = bridgeQueue
          .then(() => bridge(event))
          .catch((error: unknown) => {
            bridgeFailure ??= describeFailure(error)
          })
        return bridgeQueue
      }

      const activeRelayLanes = new Set<string>()
      let relayCutSealed = false
      let relayIneligibilityPublished = false
      const laneKey = (lane: { readonly laneId: number; readonly laneEpoch: number }) =>
        `${lane.laneId}/${lane.laneEpoch}`
      const publishRelayIneligibility = async (): Promise<void> => {
        if (
          !relayCutSealed || relayIneligibilityPublished || activeRelayLanes.size !== 0
        ) return
        relayIneligibilityPublished = true
        await publish({ kind: 'relay-ineligible' })
      }
      hotSwitchWindow.__windshareSealHotSwitchRelayCut = async () => {
        relayCutSealed = true
        await publishRelayIneligibility()
      }

      let releasePeer!: () => void
      let peerReleased = false
      const peerRelease = new Promise<void>((resolveRelease) => {
        releasePeer = () => {
          if (peerReleased) return
          peerReleased = true
          resolveRelease()
        }
      })
      const waitForPeerRelease = async (signal: AbortSignal): Promise<void> => {
        signal.throwIfAborted()
        await new Promise<void>((resolveRelease, rejectRelease) => {
        const aborted = () => {
            cleanup()
            rejectRelease(signal.reason ?? new DOMException('Peer release aborted', 'AbortError'))
          }
          const released = () => {
            cleanup()
            resolveRelease()
          }
          const cleanup = () => signal.removeEventListener('abort', aborted)
          signal.addEventListener('abort', aborted, { once: true })
          if (signal.aborted) {
            aborted()
            return
          }
          peerRelease.then(released, rejectRelease)
        })
      }

      let releaseOutput!: () => void
      let outputReleased = false
      const outputRelease = new Promise<void>((resolveRelease) => {
        releaseOutput = () => {
          if (outputReleased) return
          outputReleased = true
          resolveRelease()
        }
      })
      hotSwitchWindow.__windshareReleaseHotSwitchOutput = releaseOutput

      const transferTask = (async () => {
        const gatewayPath = '/src/ui/v2-gateway.ts'
        const offerPath = '/src/connectivity/peer-offer.ts'
        const streamPath = '/src/output/streams/single-file.ts'
        const gatewayModule = await import(gatewayPath) as typeof import('../src/ui/v2-gateway')
        const offerModule = await import(offerPath) as typeof import('../src/connectivity/peer-offer')
        const streamModule = await import(streamPath) as typeof import(
          '../src/output/streams/single-file'
        )
        let joined: Awaited<ReturnType<
          InstanceType<typeof gatewayModule.V2BrowserReceiverGateway>['join']
        >> | undefined
        let activation: ReturnType<
          NonNullable<typeof joined>['beginDownloadConnectivity']
        > | undefined
        const chunks: Uint8Array[] = []
        let deliveryStarted = false
        let runtimeError: string | undefined

        const snapshotDelivery = async () => {
          const length = chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
          const bytes = new Uint8Array(length)
          let offset = 0
          for (const chunk of chunks) {
            bytes.set(chunk, offset)
            offset += chunk.byteLength
          }
          const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
          return {
            bytes: length,
            sha256: Array.from(digest, (byte) => byte.toString(16).padStart(2, '0')).join(''),
          }
        }

        try {
          const realOffers = new offerModule.BrowserOfferChannelFactory({ configuration: rtcConfiguration })
          const gatedOffers = {
            offer: async (
              route: Parameters<typeof realOffers.offer>[0],
              signal: AbortSignal,
              observer?: Parameters<typeof realOffers.offer>[2],
            ) => {
              const [peer] = await Promise.all([
                realOffers.offer(route, signal, observer),
                waitForPeerRelease(signal),
              ])
              return peer
            },
          }
          const gateway = new gatewayModule.V2BrowserReceiverGateway({
            offersFactory: () => gatedOffers,
            connectivityObserver: (evidence: BrowserAttemptEvidence) => {
              publish({ kind: 'attempt', evidence }).catch(() => undefined)
            },
            onBlockDispatched: (observation) => {
              publish({
                kind: 'dispatch',
                observation: {
                  dispatchSequence: observation.dispatchSequence,
                  laneId: observation.laneId,
                  laneEpoch: observation.laneEpoch,
                  route: observation.route,
                },
              }).catch(() => undefined)
              if (observation.route === 'relay') releasePeer()
            },
            onContentLaneAdmitted: (observation) => {
              if (observation.route === 'relay') activeRelayLanes.add(laneKey(observation))
              publish({ kind: 'lane-admitted', observation }).catch(() => undefined)
            },
            onContentLaneDetached: (observation) => {
              if (observation.route === 'relay') activeRelayLanes.delete(laneKey(observation))
              publish({ kind: 'lane-detached', observation })
                .then(publishRelayIneligibility)
                .catch(() => undefined)
            },
          })
          joined = await gateway.join(key, window.location.href)
          activation = joined.beginDownloadConnectivity('large')
          let outputFenceUsed = false
          const output = new streamModule.SingleFileStreamOutputSession(
            `browser-${crypto.randomUUID()}`,
            new WritableStream<Uint8Array>({
              async write(chunk) {
                if (!outputFenceUsed) {
                  outputFenceUsed = true
                  await outputRelease
                }
                chunks.push(chunk.slice())
              },
            }),
          )
          deliveryStarted = true
          const result = await joined.transferJob(output, activation).run()
          const received = await snapshotDelivery()
          const jobOutcome = {
            status: result.outcome.status,
            failures: result.outcome.failures.map((failure): ObservedTransferFailure => (
              failure.kind === 'file'
                ? {
                    kind: 'file',
                    id: failure.fileId,
                    reason: describeFailure(failure.reason),
                  }
                : {
                    kind: 'directory',
                    id: failure.directoryId,
                    reason: describeFailure(failure.reason),
                  }
            )),
            failureCount: result.outcome.failureCount,
            omittedFailureCount: result.outcome.omittedFailureCount,
          } as const
          const succeeded = jobOutcome.status === 'Succeeded' &&
            received.bytes === transferBytes && received.sha256 === expectedHash
          await publish({
            kind: 'delivery',
            outcome: succeeded ? 'succeeded' : 'failed',
            evidence: {
              expectedBytes: transferBytes,
              receivedBytes: received.bytes,
              expectedSha256: expectedHash,
              receivedSha256: received.sha256,
              terminal: succeeded ? 'succeeded' : 'failed',
            },
            jobOutcome,
          })
        } catch (error) {
          runtimeError = describeFailure(error)
          if (deliveryStarted) {
            const received = await snapshotDelivery()
            await publish({
              kind: 'delivery',
              outcome: 'failed',
              evidence: {
                expectedBytes: transferBytes,
                receivedBytes: received.bytes,
                expectedSha256: expectedHash,
                receivedSha256: received.sha256,
                terminal: 'failed',
              },
              failureMessage: runtimeError,
            })
          }
        } finally {
          try {
            activation?.close()
          } catch (error) {
            runtimeError ??= describeFailure(error)
          }
          try {
            await joined?.close()
          } catch (error) {
            runtimeError ??= describeFailure(error)
          }
          // Observer callbacks intentionally do not block product control flow.
          // Drain their serialized bridge before deciding whether the runtime
          // terminal is healthy so a rejected observation cannot disappear.
          await bridgeQueue
          runtimeError ??= bridgeFailure
          runtimeTerminalPublished = true
          await publish({
            kind: 'runtime-settled',
            ...(runtimeError === undefined ? {} : { error: runtimeError }),
          })
        }
      })()
      transferTask.catch(async (error: unknown) => {
        if (runtimeTerminalPublished) return
        runtimeTerminalPublished = true
        await publish({ kind: 'runtime-settled', error: describeFailure(error) })
      }).catch(() => undefined)
}

function assertAcceptedSample(sample: CollectedSample): void {
  if (sample.delivery.error !== undefined) throw sample.delivery.error
  const delivery = sample.delivery.value
  if (delivery === undefined) throw new Error('Delivery terminal disappeared after settlement')
  expect(delivery.outcome).toBe('succeeded')
  expect(delivery.evidence).toEqual({
    expectedBytes: TRANSFER_BYTES,
    receivedBytes: TRANSFER_BYTES,
    expectedSha256: MAIN_TRANSFER_SHA256,
    receivedSha256: MAIN_TRANSFER_SHA256,
    terminal: 'succeeded',
  })
  expect(delivery.jobOutcome).toEqual({
    status: 'Succeeded',
    failures: [],
    failureCount: 0,
    omittedFailureCount: 0,
  })
  if (sample.runtime.error !== undefined) throw sample.runtime.error
  expect(sample.runtime.value?.error).toBeUndefined()
  if (sample.attemptError !== undefined) throw sample.attemptError
  if (sample.routeError !== undefined) throw sample.routeError
  if (sample.orchestrationErrors.length > 0) {
    throw aggregateFailure(sample.orchestrationErrors, 'Hot-switch orchestration evidence failed')
  }

  const attemptOutcome = reducePeerAttemptOutcome(sample.attempts)
  if (sample.capability.rtcCapability === 'available') {
    if (sample.peerTerminal?.error !== undefined) throw sample.peerTerminal.error
    expect(sample.peerTerminal?.value?.stage).toBe('admitted')
    expect(attemptOutcome).toBe('admitted')
    expect(sample.routeEvidence.mode).toBe('hot-switch')
    assertDirectPairProof(sample.attempts, sample.capability.rtcCapability, sample.topologyLock)
    return
  }
  if (sample.capability.rtcCapability === 'unavailable') {
    expect(sample.peerTerminal).toBeNull()
    expect(sample.attempts).toEqual([])
    expect(attemptOutcome).toBe('not-started')
    expect(sample.routeEvidence.mode).toBe('relay-only')
    expect(sample.routeEvidence.observations.every(
      (observation) => observation.kind === 'dispatch' && observation.route === 'relay',
    )).toBe(true)
    return
  }

  expect(sample.routeEvidence.mode).toBe('relay-only')
  expect(sample.routeEvidence.observations.every(
    (observation) => observation.kind === 'dispatch' && observation.route === 'relay',
  )).toBe(true)
  throw new Error(
    `RTC capability ${sample.capability.rtcCapability} blocks acceptance after exact relay fallback`,
  )
}

function assertDirectPairProof(
  attempts: readonly LogicalAttempt[],
  rtcCapability: RtcCapability,
  topology: VerifiedTestIceTopologyLock,
): void {
  expect(rtcCapability).toBe('available')
  const admitted = attempts.filter((attempt) => attempt.outcome === 'admitted')
  expect(admitted).toHaveLength(1)
  const browser = admitted[0]?.events.find(({ evidence }) =>
    evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
  const sender = admitted[0]?.events.find(({ evidence }) =>
    evidence.side === 'sender' && evidence.stage === 'admitted')?.evidence
  if (browser?.side !== 'browser' || browser.stage !== 'admitted') {
    throw new Error('Admitted attempt lacks its browser terminal')
  }
  if (sender?.side !== 'sender' || sender.stage !== 'admitted') {
    throw new Error('Admitted attempt lacks its sender terminal')
  }
  expect(browser.selectedPair).not.toBeNull()
  expect(sender.selectedPair).not.toBeNull()
  if (browser.selectedPair === null || sender.selectedPair === null) return

  expect(selectedPairAllowedByTopology(
    browser.selectedPair,
    topology.profile,
    topology.resolution,
  )).toBe(true)
  expect(selectedPairAllowedByTopology(
    sender.selectedPair,
    topology.profile,
    topology.resolution,
  )).toBe(true)
}

function validateReporterTopology(
  reporter: ChildEvidenceReporter | null,
  topology: VerifiedTestIceTopologyLock,
): void {
  if (
    reporter !== null &&
    (reporter.context.topologyProfileSha256 !== topology.profileSha256 ||
      reporter.context.topologyResolutionSha256 !== topology.resolutionSha256)
  ) {
    throw new Error('Main product topology digests differ from the parent evidence context')
  }
}

function recordCompletedAuthorities(
  gate: IntermediateEvidenceGate,
  delivery: HotSwitchDeliveryTerminal | undefined,
  route: MainRouteEvidence | undefined,
): void {
  gate.publish((reporter) => {
    if (delivery !== undefined) reporter.recordDelivery(delivery.outcome, delivery.evidence)
    if (route !== undefined) reporter.recordRoute(route)
  })
}

async function sealPageRelayCut(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const seal = (
      window as Window & { __windshareSealHotSwitchRelayCut?: () => Promise<void> }
    ).__windshareSealHotSwitchRelayCut
    if (seal === undefined) throw new Error('Hot-switch relay-cut seal is unavailable')
    await seal()
  })
}

async function releasePageOutput(page: Page): Promise<void> {
  if (page.isClosed()) return
  await page.evaluate(() => {
    const release = (
      window as Window & { __windshareReleaseHotSwitchOutput?: () => void }
    ).__windshareReleaseHotSwitchOutput
    release?.()
  })
}

async function runWithSanitizedFailureTrace<T>(
  options: {
    readonly deadline: WholeSampleDeadline
    readonly page: Page
    readonly reporter: ChildEvidenceReporter | null
    readonly registerLateCleanup: RegisterLateCleanup
    readonly testInfo: TestInfo
    readonly beginFixtureCleanup: () => readonly Promise<unknown>[]
  },
  operation: () => Promise<T>,
): Promise<T> {
  const tracing = options.page.context().tracing
  await acquireFixtureWork(
    options.deadline,
    'Sanitized trace startup failed',
    () => tracing.start({ screenshots: true, snapshots: true, sources: true }),
    'Late sanitized trace startup rollback failed',
    () => tracing.stop(),
    options.registerLateCleanup,
  )
  let result: T | undefined
  let operationError: unknown
  let operationFailed = false
  let operationDrain: Promise<unknown> | undefined
  let operationTask: Promise<T> | undefined
  try {
    // The outer work race closes the small synchronous gaps between named
    // evidence waits, so a verdict cannot resolve just after the absolute cutoff.
    result = await options.deadline.runWork(() => {
      operationTask = Promise.resolve().then(operation)
      return operationTask
    })
  } catch (error) {
    operationFailed = true
    operationError = error instanceof WholeSampleDeadlineExpiredError
      ? new FixtureInfrastructureError(
          'Hot-switch product operation exceeded its work authority',
          error,
        )
      : error
    if (error instanceof WholeSampleDeadlineExpiredError && operationTask !== undefined) {
      const admittedOperation = operationTask
      operationDrain = fixtureCleanup(
        options.deadline,
        'Late hot-switch product operation drain failed',
        async () => {
          const late = await settle(admittedOperation)
          if (late.error !== undefined && late.error !== error) {
            throw new FixtureInfrastructureError(
              'Hot-switch product operation failed after its work cutoff',
              late.error,
            )
          }
        },
      )
    }
  }
  const ownerCleanup = options.beginFixtureCleanup()
  const traceFinalization = operationFailed
    ? fixtureCleanup(
        options.deadline,
        'Sanitized failure trace retention failed',
        async (signal) => {
          signal.throwIfAborted()
          const tracePath = options.reporter === null
            ? options.testInfo.outputPath('hot-switch-trace.zip')
            : resolve(
                options.reporter.context.artifactRoot,
                ...HOT_SWITCH_TRACE_RELATIVE_PATH.split('/'),
              )
          await mkdir(dirname(tracePath), { recursive: true })
          signal.throwIfAborted()
          await tracing.stop({ path: tracePath })
          signal.throwIfAborted()
          await options.testInfo.attach('hot-switch-trace', {
            path: tracePath,
            contentType: 'application/zip',
          })
          signal.throwIfAborted()
          options.reporter?.recordArtifact({
            kind: 'trace',
            relativePath: HOT_SWITCH_TRACE_RELATIVE_PATH,
            mediaType: 'application/zip',
          })
        },
      )
    : fixtureCleanup(
        options.deadline,
        'Sanitized trace finalization failed',
        () => tracing.stop(),
      )
  const cleanup = await Promise.allSettled([
    ...ownerCleanup,
    traceFinalization,
    ...(operationDrain === undefined ? [] : [operationDrain]),
  ])
  const cleanupErrors = cleanup.flatMap((settlement) =>
    settlement.status === 'rejected' ? [settlement.reason] : [],
  )
  if (operationFailed || cleanupErrors.length > 0) {
    throw aggregateFailure(
      [
        ...(operationFailed ? [operationError] : []),
        ...cleanupErrors,
      ],
      operationFailed
        ? 'Hot-switch sample and cleanup failed'
        : 'Hot-switch fixture cleanup failed',
    )
  }
  return result as T
}

function optionalChildReporter(): ChildEvidenceReporter | null {
  const encoded = process.env[CHILD_EVIDENCE_CONTEXT_ENV]
  return encoded === undefined || encoded === '' ? null : new ChildEvidenceReporter()
}

function validateReporterIdentity(
  reporter: ChildEvidenceReporter | null,
  browserName: string,
  testInfo: TestInfo,
): void {
  if (reporter === null) return
  if (reporter.context.suite !== 'main') {
    throw new Error('Product hot-switch evidence context must identify the main suite')
  }
  if (reporter.context.browser !== browserName) {
    throw new Error('Product hot-switch evidence browser differs from the Playwright project')
  }
  if (testInfo.project.retries !== 0 || testInfo.retry !== 0) {
    throw new Error('Product hot-switch evidence prohibits Playwright retries')
  }
  if (testInfo.project.repeatEach !== 1 || testInfo.repeatEachIndex !== 0) {
    throw new Error('Product hot-switch evidence requires one sample per child process')
  }
}

function observePublicBrowserDiagnostics(
  page: Page,
  reporter: ChildEvidenceReporter | null,
  failRuntime: (error: Error) => void,
): {
  readonly close: () => void
  readonly expectTargetClose: () => void
} {
  const sink = reporter === null ? null : publicBrowserDiagnosticSink(reporter)
  const browser = page.context().browser()
  let targetCloseExpected = false
  const crashed = () => {
    const error = new Error('Playwright page crash event')
    sink?.pageCrashed(error)
    failRuntime(error)
  }
  const pageError = (error: Error) => sink?.pageError(error)
  const closed = () => {
    if (targetCloseExpected) return
    const error = new Error('Playwright page closed unexpectedly during the product sample')
    sink?.targetCrashed(error)
    failRuntime(error)
  }
  const consoleMessage = (message: { readonly type: () => string; readonly text: () => string }) => {
    sink?.console(message.type(), message.text())
  }
  const disconnected = () => {
    const error = new Error('Playwright browser disconnected during the product sample')
    sink?.browserDisconnected(false, error)
    failRuntime(error)
  }
  page.on('crash', crashed)
  page.on('close', closed)
  page.on('pageerror', pageError)
  page.on('console', consoleMessage)
  browser?.on('disconnected', disconnected)
  return Object.freeze({
    expectTargetClose: () => { targetCloseExpected = true },
    close: () => {
      page.off('crash', crashed)
      page.off('close', closed)
      page.off('pageerror', pageError)
      page.off('console', consoleMessage)
      browser?.off('disconnected', disconnected)
    },
  })
}

async function fixtureOperation<T>(
  boundary: string,
  operation: () => Promise<T>,
): Promise<T> {
  try {
    return await operation()
  } catch (cause) {
    throw new FixtureInfrastructureError(boundary, cause)
  }
}

function fixtureWork<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runWork(operation))
}

async function acquireFixtureWork<T>(
  deadline: WholeSampleDeadline,
  acquisitionBoundary: string,
  acquire: (signal: AbortSignal) => T | PromiseLike<T>,
  rollbackBoundary: string,
  rollback: (resource: T, signal: AbortSignal) => unknown | PromiseLike<unknown>,
  registerLateCleanup: RegisterLateCleanup,
): Promise<T> {
  return fixtureOperation(acquisitionBoundary, () => acquireWholeSampleResource(
    deadline,
    acquire,
    rollbackBoundary,
    rollback,
    registerLateCleanup,
  ))
}

function fixtureCleanup<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runCleanup(operation))
}

function fixturePublication<T>(
  deadline: WholeSampleDeadline,
  boundary: string,
  operation: (signal: AbortSignal) => T | PromiseLike<T>,
): Promise<T> {
  return fixtureOperation(boundary, () => deadline.runPublication(operation))
}

function fixtureValue<T>(boundary: string, operation: () => T): T {
  try {
    return operation()
  } catch (cause) {
    throw new FixtureInfrastructureError(boundary, cause)
  }
}

async function settle<T>(promise: Promise<T>): Promise<PromiseSettlement<T>> {
  try {
    return Object.freeze({ value: await promise })
  } catch (error) {
    return Object.freeze({ error })
  }
}

function aggregateFailure(failures: readonly unknown[], message: string): unknown {
  if (failures.length === 1) return failures[0]
  return new AggregateError(failures, message)
}

function deterministicBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  let state = 0x6d2b79f5
  for (let index = 0; index < bytes.length; index += 1) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
    bytes[index] = state >>> 24
  }
  return bytes
}
