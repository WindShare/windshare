import { afterEach, describe, expect, it, vi } from 'vitest'

import { recordOutputException } from '../../src/output/diagnostics'
import { normalizeV2FileTransferFailure } from '../../src/transfer/job/failures'
import { V2TransferAdmissionFailureError } from '../../src/transfer/v2-job'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type { V2BrowserReceiverGateway } from '../../src/ui/v2-gateway'
import {
  FakeGateway,
  FakeJoinedShare,
  FakeReceiveComposition,
  DIRECT_ZIP_ENVIRONMENT,
  FILE_ID,
  MANAGED_ENVIRONMENT,
  NO_DESTINATION_ENVIRONMENT,
  WORKSPACE_ENVIRONMENT,
  controllerFor,
  deferred,
  identityText,
  next,
  recordingIncidents,
  resetOrchestrationTestEnvironment,
  stableLifecycle,
  startTransfer,
  turns,
  waitFor,
} from './v2-receiver-orchestration-fixture'

afterEach(resetOrchestrationTestEnvironment)

describe('v2 receiver active operation orchestration', () => {
  it('runs projection as data, starts authority in the click stack, then freezes intent before transfer', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    expect(receive.startedAuthorities).toHaveLength(0)

    let inClickStack = true
    receive.clickStack = () => inClickStack
    choosePrimary(controller)
    inClickStack = false

    expect(receive.authorityStartStacks).toEqual([true])
    await waitFor(() => joined.transferRuns.length === 1)

    const authority = receive.startedAuthorities[0]
    const runtime = authority?.runtime
    const run = joined.transferRuns[0]
    if (authority === undefined || runtime === undefined || run === undefined) {
      throw new Error('receive runtime was not composed')
    }
    expect(run.plans).toBe(runtime.plans)
    expect(run.intent).toBe(runtime.intent)
    expect(controller.getSnapshot().output.receiveIntent?.digest).toBe(runtime.intent.digest)
    expect(controller.getSnapshot().output.lifecyclePresentation?.actions.map(action => action.kind))
      .toEqual(['pause', 'stop'])

    run.resolve(next(runtime.lifecycle, {
      kind: 'download-started',
      attemptKind: 'portable',
      attemptId: identityText(70),
    }))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'download-started')
    expect(controller.getSnapshot().output.lifecyclePresentation).toMatchObject({
      title: 'Download started',
      category: 'terminal',
      actions: [],
    })

    await controller.dispose()
    expect(runtime.detachments).toEqual(['detached'])
  })

  it('starts a fresh planning operation after the owned Direct ZIP target is deleted', async () => {
    const receive = new FakeReceiveComposition(DIRECT_ZIP_ENVIRONMENT)
    const joined = new FakeJoinedShare(true, [], 'tree')
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('Direct ZIP receive was not started')
    run.resolve(next(runtime.lifecycle, {
      kind: 'restart-required',
      reason: 'target-deleted',
      receiptDigest: identityText(77, 32),
    }))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'restart-required')
    const projectionCount = joined.projectionRequests.length

    controller.startNewReceiveOperation()
    await waitFor(() => joined.projectionRequests.length === projectionCount + 1)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    expect(runtime.detachments).toEqual(['detached'])
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().output.lifecycle).toBeNull()
    expect(receive.startedAuthorities).toHaveLength(1)
    await controller.dispose()
  })

  it('fences a stale catalog projection when selection mutation starts a new epoch', async () => {
    const firstProjection = deferred<void>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(false, [firstProjection.promise])
    const controller = controllerFor(joined, receive)

    await waitFor(() => joined.projectionRequests.length === 1)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('unselected')
    expect(receive.startedAuthorities).toHaveLength(0)

    controller.toggleSelection(FILE_ID)
    await waitFor(() => joined.projectionRequests.length === 2)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    const staleRequest = joined.projectionRequests[0]
    const currentEpoch = controller.getSnapshot().output.projection?.projection.epoch
    expect(staleRequest?.signal.aborted).toBe(true)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(currentEpoch).toBe(2n)
    expect(receive.startedAuthorities).toHaveLength(0)

    firstProjection.resolve()
    await turns()
    expect(controller.getSnapshot().output.projection?.projection.epoch).toBe(currentEpoch)
    expect(controller.getSnapshot().output.offerPresentation?.kind).toBe('choices')

    await controller.dispose()
  })

  it('reprojects a reconnected session before a stale action can start a picker', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')

    joined.replaceProtocolSession(identityText(4))
    choosePrimary(controller)

    expect(receive.startedAuthorities).toHaveLength(0)
    await waitFor(() => controller.getSnapshot().output.projection?.projection.epoch === 2n)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    expect(receive.startedAuthorities).toHaveLength(0)

    await controller.dispose()
  })

  it('rechecks current capability facts after acquisition and refuses an invalidated action', async () => {
    const authorityReady = deferred<void>()
    const receive = new FakeReceiveComposition(
      MANAGED_ENVIRONMENT,
      [MANAGED_ENVIRONMENT, NO_DESTINATION_ENVIRONMENT],
    )
    receive.authorityReady = authorityReady.promise
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    choosePrimary(controller)
    joined.replaceProtocolSession(identityText(5))
    await waitFor(() => (receive.startedAuthorities[0]?.releaseReasons.length ?? 0) > 0)
    await waitFor(() => {
      const presentation = controller.getSnapshot().output.offerPresentation
      return presentation?.kind === 'status' &&
        presentation.title === 'This browser cannot safely create the selected result.'
    })
    authorityReady.resolve()

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().output.chosenChoice).toBeNull()
    expect(controller.getSnapshot().output.offerPresentation).toMatchObject({
      kind: 'status',
      title: 'This browser cannot safely create the selected result.',
    })

    await controller.dispose()
  })

  it('preserves late authority readiness when a reauthenticated join proves the same semantics', async () => {
    const authorityReady = deferred<void>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.authorityReady = authorityReady.promise
    const first = new FakeJoinedShare(true)
    const second = new FakeJoinedShare(true)
    const gateway = new FakeGateway([first, second])
    const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
      receive,
    })
    controller.initialize({ capabilityInput: 'first', pageUrl: 'https://receiver.invalid/s/share' })

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    choosePrimary(controller)
    const late = receive.startedAuthorities[0]
    if (late === undefined) throw new Error('authority was not installed synchronously')
    controller.submitKey('second')
    await waitFor(() => gateway.joinCount === 2)
    await waitFor(() => first.closeCount === 1)

    authorityReady.resolve()
    await waitFor(() => second.transferRuns.length === 1)

    expect(first.transferRuns).toHaveLength(0)
    expect(late.releaseReasons).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).not.toBeNull()
    await controller.dispose()
  })

  it.each([
    ['pause', 'resumable-receive', 'Ready to continue receiving'],
    ['stop', 'restart-required', 'Start again required'],
  ] as const)('interrupts active %s synchronously and publishes %s', async (
    control,
    expectedKind,
    expectedTitle,
  ) => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)

    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    let inClickStack = true
    runtime.controlStack = () => inClickStack

    controller.performLifecycleAction(control)
    inClickStack = false

    expect(runtime.interruptions).toEqual([{ control, inClickStack: true }])
    expect(run.signal?.aborted).toBe(true)
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === expectedKind)
    expect(controller.getSnapshot().output.lifecyclePresentation?.title).toBe(expectedTitle)
    expect(runtime.admissionFailures).toEqual([])

    await controller.dispose()
  })

  it('publishes the original transfer failure after the job has already settled output', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    const failure = new Error('sender rejected the revision lease')
    const stable = stableLifecycle(runtime.lifecycle, 'resumable-receive')

    run.resolve(stable, failure)
    await waitFor(() => controller.getSnapshot().error === failure.message)

    expect(controller.getSnapshot().error).toBe(failure.message)
    expect(controller.getSnapshot().output.lifecycle).toEqual(stable)
    expect(runtime.admissionFailures).toEqual([])
    await controller.dispose()
  })

  it('keeps the initiating failure visible when admission settlement also fails', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    const failure = new Error('transfer job could not validate its session')
    const normalized = normalizeV2FileTransferFailure(failure)
    if (normalized.kind !== 'fault') throw new Error('expected reviewed admission failure')
    const classification = normalized.diagnostic.classification
    runtime.admissionSettlementError = new Error('secondary lifecycle settlement failed')

    run.reject(new V2TransferAdmissionFailureError({
      kind: 'fault',
      classification,
    }))
    await waitFor(() =>
      controller.getSnapshot().error === 'Transfer failed before output settlement completed')

    expect(controller.getSnapshot().error)
      .toBe('Transfer failed before output settlement completed')
    expect(runtime.admissionFailures).toEqual([classification])
    await controller.dispose()
  })

  it('settles canceled admission without publishing it as a receive failure', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    const priorError = controller.getSnapshot().error

    run.reject(new V2TransferAdmissionFailureError({ kind: 'canceled' }))
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'restart-required')

    expect(controller.getSnapshot().error).toBe(priorError)
    expect(runtime.admissionFailures[0]).toBeInstanceOf(V2TransferAdmissionFailureError)
    await controller.dispose()
  })

  it('does not reinterpret an unclassified job rejection as an admission failure', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    const failure = new Error('unexpected post-admission failure')

    run.reject(failure)
    await waitFor(() => controller.getSnapshot().error === failure.message)

    expect(runtime.admissionFailures).toEqual([])
    await controller.dispose()
  })
})

function choosePrimary(controller: V2ReceiverController): void {
  const offers = controller.getSnapshot().output.offers
  if (offers?.kind !== 'artifact-actions') throw new Error('primary artifact choice is unavailable')
  controller.chooseArtifact(offers.primary.choice.choiceId)
}

describe('v2 receive attempt observability', () => {
  it('links detach cleanup after the sealed native transfer trigger without reopening contributors', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const diagnostics = recordingIncidents()
    const controller = controllerFor(joined, receive, diagnostics.port)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('active receive was not started')
    const trigger = new DOMException('private settlement detail', 'AbortError')
    const consequence = new DOMException('private detach detail', 'InvalidStateError')
    recordOutputException(runtime.currentOutputFailures?.settlement, trigger)
    run.reject(trigger)
    await waitFor(() => controller.getSnapshot().error === trigger.message)
    runtime.detachFailure = consequence

    await expect(controller.dispose()).resolves.toBeUndefined()

    const nativeFacts = diagnostics.facts.filter(
      observation => observation.fact.kind === 'native_output_failure',
    )
    expect(nativeFacts).toMatchObject([
      {
        scopeKind: 'receive',
        fact: {
          kind: 'native_output_failure',
          stage: 'settlement',
          payload: { nativeOutputFailure: { nativeClass: 'abort' } },
        },
        relation: 'contributor',
      },
      {
        scopeKind: 'receive',
        fact: {
          kind: 'native_output_failure',
          stage: 'cleanup',
          payload: { nativeOutputFailure: { nativeClass: 'invalid_state' } },
        },
        relation: 'consequence',
      },
    ])
    expect(nativeFacts[0]!.scopeSequence).toBe(nativeFacts[1]!.scopeSequence)
    const triggerIndex = diagnostics.order.indexOf('native:settlement:contributor')
    const decisionIndex = diagnostics.order.indexOf('decision:incident', triggerIndex)
    const consequenceIndex = diagnostics.order.indexOf('native:cleanup:consequence')
    expect(triggerIndex).toBeLessThan(decisionIndex)
    expect(decisionIndex).toBeLessThan(consequenceIndex)
  })

  it.each([
    ['resumable-receive', 'discard'],
    ['waiting-to-save', 'save'],
    ['download-started', 'redownload'],
  ] as const)('starts %s action %s inside the rendered click stack', async (stableKind, action) => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)

    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('workspace receive was not started')
    const stable = stableLifecycle(runtime.lifecycle, stableKind)
    run.resolve(stable)
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === stableKind)
    expect(controller.getSnapshot().output.lifecyclePresentation?.usage?.ownedBytes).toBe(512n)

    let inClickStack = true
    runtime.lifecycleActionStack = () => inClickStack
    controller.performLifecycleAction(action)
    inClickStack = false

    expect(runtime.lifecycleActions.at(-1)).toEqual({ action, inClickStack: true })
    await waitFor(() => controller.getSnapshot().output.lifecycle?.generation === stable.generation + 1n)
    await controller.dispose()
  })

  it('keeps a native AbortError from lifecycle publication visible with its native trigger', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const diagnostics = recordingIncidents()
    const controller = controllerFor(joined, receive, diagnostics.port)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('workspace receive was not started')
    const stable = stableLifecycle(runtime.lifecycle, 'waiting-to-save')
    run.resolve(stable)
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'waiting-to-save')
    const nativeAbort = new DOMException('private publication detail', 'AbortError')
    runtime.nextLifecycleAction = () => {
      recordOutputException(runtime.currentOutputFailures?.publication, nativeAbort)
      throw nativeAbort
    }

    controller.performLifecycleAction('save')
    await waitFor(() => controller.getSnapshot().error === nativeAbort.message)

    expect(diagnostics.facts.find(
      observation => observation.scopeKind === 'lifecycle_action' &&
        observation.fact.kind === 'native_output_failure',
    )).toMatchObject({
      fact: {
        kind: 'native_output_failure',
        stage: 'publication',
        payload: { nativeOutputFailure: { nativeClass: 'abort' } },
      },
      relation: 'contributor',
    })
    expect(diagnostics.decisions.find(
      decision => decision.kind === 'incident' &&
        decision.boundary === 'lifecycle_action',
    )).toMatchObject({
      kind: 'incident',
      outcome: 'failed',
    })
    expect(JSON.stringify(diagnostics.facts.map(observation => observation.fact)))
      .not.toContain('private publication detail')
    await controller.dispose()
  })

  it('excludes an invisible expiry AbortError without publishing a product error', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const diagnostics = recordingIncidents()
    const controller = controllerFor(joined, receive, diagnostics.port)
    await startTransfer(controller, joined)
    vi.useFakeTimers()
    vi.setSystemTime(1_000_000)
    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('workspace receive was not started')
    run.resolve(next(runtime.lifecycle, {
      kind: 'waiting-to-save',
      packageDigest: identityText(71, 32),
      expiresAt: 1_001_000,
    }))
    await turns()
    const priorError = controller.getSnapshot().error
    const priorDecisionCount = diagnostics.decisions.length
    runtime.expiryFailure = new DOMException('private expiry detail', 'AbortError')

    await vi.advanceTimersByTimeAsync(1_000)
    await turns()

    expect(controller.getSnapshot().error).toBe(priorError)
    expect(diagnostics.decisions.slice(priorDecisionCount)).toEqual([
      expect.objectContaining({
        kind: 'excluded',
        boundary: 'lifecycle_action',
        reason: 'not_user_visible',
      }),
    ])
    expect(diagnostics.facts.at(-1)).toMatchObject({
      scopeKind: 'lifecycle_action',
      fact: {
        kind: 'native_output_failure',
        stage: 'checkpoint',
        payload: { nativeOutputFailure: { nativeClass: 'abort' } },
      },
    })
    expect(diagnostics.decisions.slice(priorDecisionCount))
      .not.toEqual(expect.arrayContaining([expect.objectContaining({ kind: 'incident' })]))
    await controller.dispose()
  })

  it('observes retained expiry, exposes cleanup only when required, and leaves NeedsAttention inert', async () => {
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    vi.useFakeTimers()
    vi.setSystemTime(1_000_000)

    const runtime = receive.startedAuthorities[0]?.runtime
    const run = joined.transferRuns[0]
    if (runtime === undefined || run === undefined) throw new Error('workspace receive was not started')
    run.resolve(next(runtime.lifecycle, {
      kind: 'waiting-to-save',
      packageDigest: identityText(71, 32),
      expiresAt: 1_001_000,
    }))
    await turns()
    expect(controller.getSnapshot().output.lifecyclePresentation?.actions.map(action => action.kind))
      .toEqual(['save', 'delete'])

    await vi.advanceTimersByTimeAsync(1_000)
    await turns()
    expect(runtime.expiryObservations).toHaveLength(1)
    expect(controller.getSnapshot().output.lifecyclePresentation).toMatchObject({
      stateKind: 'expired',
      title: 'Task expired',
      actions: [{ kind: 'delete' }],
    })

    const expired = controller.getSnapshot().output.lifecycle
    if (expired === null) throw new Error('expiry state was not published')
    runtime.nextLifecycleAction = (_action, lifecycle) => ({
      lifecycle: next(lifecycle, {
        kind: 'needs-attention',
        reason: 'cleanup-unknown',
        lastVerifiedRecordDigest: identityText(72, 32),
      }),
      workspaceUsage: { ownedBytes: 512n, maximumBytes: 4_096n },
    })
    controller.performLifecycleAction('delete')
    await turns()
    expect(controller.getSnapshot().output.lifecyclePresentation).toMatchObject({
      stateKind: 'needs-attention',
      title: 'Needs attention',
      actions: [],
    })

    await controller.dispose()
  })
})
