import { createHash } from 'node:crypto'
import { mkdir, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import { expect, test as base } from '@playwright/test'
import type { Page, TestInfo } from '@playwright/test'

import type { LogicalAttempt } from '../scripts/browser-evidence/attempt-collector'
import type { ChildEvidenceReporter } from '../scripts/browser-evidence/child-evidence'
import type { MainRouteEvidence } from '../scripts/browser-evidence/route-evidence'
import {
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
} from '../scripts/browser-evidence/result'
import { BROWSER_ENGINES } from '../scripts/browser-evidence/vocabulary'
import { classifyNativePeerConnection } from '../test/transport/webrtc/browser-capability'
import {
  HotSwitchEvidenceCollector,
  WholeSampleDeadline,
  type BrowserAttemptTerminal,
} from './fixtures/hot-switch-evidence'
import {
  releasePageOutput,
  sealPageRelayCut,
  startPageTransfer,
} from './fixtures/hot-switch-page-transfer'
import {
  closeProgressiveCatalog,
  enumerateProgressiveCatalog,
  openProgressiveCatalogDescriptor,
} from './fixtures/progressive-catalog-page'
import {
  acquireFixtureWork,
  aggregateFailure,
  drainLateCleanupTasks,
  drainTimedOutPlaywrightOwners,
  fixtureCleanup,
  fixtureValue,
  fixtureWork,
  IntermediateEvidenceGate,
  observePublicBrowserDiagnostics,
  optionalChildReporter,
  publishChildBoundary,
  releaseOwnedBinaries,
  runWithSanitizedFailureTrace,
  settle,
  validateReporterIdentity,
  type PromiseSettlement,
  type RegisteredLateCleanup,
  type RegisterLateCleanup,
} from './fixtures/hot-switch-sample-boundary'
import {
  assertAcceptedSample,
  recordCompletedAuthorities,
  validateReporterTopology,
  type CollectedSample,
} from './fixtures/hot-switch-sample-verdict'
import { acquireTestIceTopology } from './fixtures/test-ice-topology-runtime'
import {
  V2RealStack,
  acquireRealStackBinaries,
  readSenderAttemptEvidenceSnapshot,
  releaseRealStackBinaries,
  replaceRelayHint,
  type BinaryPaths,
} from './fixtures/v2-real-stack'
import {
  FixtureInfrastructureError,
} from './fixtures/managed-process'
import { createInheritedChildProcessBackend } from '../scripts/browser-evidence/process/inherited-child-process.mjs'

const TRANSFER_BYTES = MAIN_TRANSFER_BYTES
const FIRST_RELAY_DISPATCH_DEADLINE_MS = 20_000
const PEER_TERMINAL_DEADLINE_MS = 15_000
const RELAY_INELIGIBILITY_DEADLINE_MS = 10_000
const POST_FENCE_PEER_DISPATCH_DEADLINE_MS = 15_000
const SENDER_EVIDENCE_TERMINAL_DEADLINE_MS = 10_000
const SAMPLE_TEARDOWN_HEADROOM_MS = 20_000
const SAMPLE_EVIDENCE_PUBLICATION_HEADROOM_MS = 2_000
const SAMPLE_PLAYWRIGHT_COMPLETION_HEADROOM_MS = 1_000
const PROGRESSIVE_CATALOG_INVENTORY = Object.freeze([
  'directory:tree',
  'directory:tree/nested',
  'directory:tree/nested/empty-dir',
  'file:tree/nested/a.txt:5',
  'file:tree/stable.txt:6',
  'file:tree/zero.bin:0',
])

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

test.use({
  // The separate capability key enters the page before tracing starts. Failure
  // evidence remains useful without serializing the secret-bearing setup call.
  trace: 'off',
  screenshot: 'only-on-failure',
  video: 'retain-on-failure',
})

test('proves focused real-stack browser contracts', async ({
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
      const sample = await assertHotSwitchSample({
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
      expect(sample.expectedSha256).toBe(MAIN_TRANSFER_SHA256)
      if (browserName === 'chromium') {
        await assertProgressiveCatalog({
          baseURL,
          binaries: ownedBinaries,
          deadline,
          page,
        })
      }
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

async function assertProgressiveCatalog(options: {
  readonly baseURL: string
  readonly binaries: BinaryPaths
  readonly deadline: WholeSampleDeadline
  readonly page: Page
}): Promise<void> {
  const { baseURL, binaries, deadline, page } = options
  const reporter = optionalChildReporter()
  const lateCleanupTasks: RegisteredLateCleanup[] = []
  const registerLateCleanup: RegisterLateCleanup = (boundary, task) => {
    lateCleanupTasks.push(Object.freeze({ boundary, settlement: settle(task) }))
  }
  const failures: unknown[] = []
  let operationFailure: unknown
  let topology: Awaited<ReturnType<typeof acquireTestIceTopology>> | undefined
  let stack: V2RealStack | undefined
  let browserShareOpen = false

  try {
    topology = await acquireFixtureWork(
      deadline,
      'Progressive catalog topology acquisition failed',
      (signal) => acquireTestIceTopology(reporter?.context, process.env, signal),
      'Late progressive catalog topology rollback failed',
      (acquired) => acquired.release(),
      registerLateCleanup,
    )
    stack = new V2RealStack(
      binaries,
      topology,
      createInheritedChildProcessBackend(),
    )
    await fixtureWork(deadline, 'Progressive catalog relay startup failed', (signal) => stack?.start({
      signal,
      timeoutMilliseconds: deadline.remainingWork(),
    }))

    const directoryPath = await fixtureWork(
      deadline,
      'Progressive catalog directory fixture creation failed',
      (signal) => stack?.createDirectory('tree', { signal }),
    )
    if (directoryPath === undefined) {
      throw new Error('Progressive catalog directory fixture has no path')
    }
    await fixtureWork(deadline, 'Progressive catalog inventory fixture creation failed', async (signal) => {
      const nestedPath = join(directoryPath, 'nested')
      signal.throwIfAborted()
      await mkdir(join(nestedPath, 'empty-dir'), { recursive: true })
      signal.throwIfAborted()
      await Promise.all([
        writeFile(join(nestedPath, 'a.txt'), 'alpha', { signal }),
        writeFile(join(directoryPath, 'stable.txt'), 'stable', { signal }),
        writeFile(join(directoryPath, 'zero.bin'), new Uint8Array(), { signal }),
      ])
    })

    const share = await fixtureWork(
      deadline,
      'Progressive catalog sender startup failed',
      (signal) => stack?.shareWithCatalogGate(directoryPath, baseURL, {
        signal,
        timeoutMilliseconds: deadline.remainingWork(),
      }),
    )
    if (share === undefined) throw new Error('Progressive catalog sender returned no share')
    expect(new URL(share.bareLink).protocol).toMatch(/^https?:$/u)

    await fixtureWork(deadline, 'Progressive catalog receiver navigation failed', () => page.goto(
      baseURL,
      { timeout: deadline.remainingWork() },
    ))
    const descriptor = await fixtureWork(
      deadline,
      'Progressive catalog descriptor authentication failed',
      () => openProgressiveCatalogDescriptor(page, {
        key: share.key,
        receiverLink: share.bareLink,
      }),
    )
    browserShareOpen = true
    expect(descriptor).toMatchObject({
      descriptorOpened: true,
      wireVersion: 2,
      suite: 2,
      selectedName: 'tree',
    })
    expect(descriptor.shareInstanceId).not.toBe('')
    expect(descriptor.syntheticRootId).not.toBe('')

    const blocked = await fixtureWork(
      deadline,
      'Progressive catalog pre-release observation failed',
      (signal) => share.catalogGate.assertBlocked({
        signal,
        timeoutMilliseconds: deadline.remainingWork(),
      }),
    )
    expect(blocked.released).toBe(false)
    expect(blocked.blockedRequests).toBeGreaterThanOrEqual(1)

    const released = await fixtureWork(
      deadline,
      'Progressive catalog release failed',
      (signal) => stack?.releaseCatalogGate(share.catalogGate, {
        signal,
        timeoutMilliseconds: deadline.remainingWork(),
      }),
    )
    expect(released.released).toBe(true)
    expect(released.blockedRequests).toBeGreaterThanOrEqual(blocked.blockedRequests)

    const inventory = await fixtureWork(
      deadline,
      'Progressive catalog browser enumeration failed',
      () => enumerateProgressiveCatalog(page),
    )
    expect(inventory).toEqual(PROGRESSIVE_CATALOG_INVENTORY)
    await fixtureWork(
      deadline,
      'Progressive catalog capability privacy proof failed',
      (signal) => stack?.assertCatalogGatePrivate(share.catalogGate, page.url(), { signal }),
    )
  } catch (error) {
    operationFailure = error
  } finally {
    if (browserShareOpen && !page.isClosed()) {
      try {
        await fixtureCleanup(
          deadline,
          'Progressive catalog browser cleanup failed',
          () => closeProgressiveCatalog(page),
        )
      } catch (error) {
        failures.push(error)
      }
    }
    if (stack !== undefined) {
      try {
        await fixtureCleanup(
          deadline,
          'Progressive catalog stack cleanup failed',
          (signal) => stack?.dispose({ signal }),
        )
      } catch (error) {
        failures.push(error)
      }
    }
    if (topology !== undefined) {
      try {
        await fixtureCleanup(
          deadline,
          'Progressive catalog topology cleanup failed',
          () => topology?.release(),
        )
      } catch (error) {
        failures.push(error)
      }
    }
    failures.push(...await drainLateCleanupTasks(deadline, lateCleanupTasks))
  }

  if (operationFailure !== undefined) failures.unshift(operationFailure)
  if (failures.length > 0) {
    throw aggregateFailure(failures, 'Progressive catalog sample boundary failed')
  }
}

async function assertHotSwitchSample(options: {
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
      () => new V2RealStack(
        options.binaries,
        topology,
        createInheritedChildProcessBackend(),
      ),
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

function deterministicBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  let state = 0x6d2b79f5
  for (let index = 0; index < bytes.length; index += 1) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
    bytes[index] = state >>> 24
  }
  return bytes
}
