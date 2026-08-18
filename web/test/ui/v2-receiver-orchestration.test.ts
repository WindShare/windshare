import { afterEach, describe, expect, it, vi } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy, type V2FrozenSelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2ConnectivityActivation } from '../../src/connectivity/v2-receiver-policy'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
  EnvironmentOffers,
} from '../../src/output/planning'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
  type ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import {
  createManagedAtomicReservation,
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { V2PlanExecutionAuthority } from '../../src/transfer/output-session'
import {
  createAuthenticatedProjectionEvidence,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import {
  V2TransferAdmissionFailureError,
  type TransferJobResult,
} from '../../src/transfer/v2-job'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from '../../src/ui/v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
  V2StartedArtifactAuthority,
} from '../../src/ui/v2-receive-runtime'
import {
  environment,
  handoffTarget,
  managedTarget,
  workspaceOffer,
} from '../output/planning/fixture'

const ROOT_ID = identityText(1)
const FILE_ID = identityText(2)
const SESSION_ID = identityText(3)
const MANAGED_ENVIRONMENT = environment({ targets: [managedTarget()] })
const WORKSPACE_ENVIRONMENT = environment({
  targets: [handoffTarget()],
  workspace: workspaceOffer(),
})
const NO_DESTINATION_ENVIRONMENT = environment()

const FILE_ENTRY: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identityBytes(2),
  idText: FILE_ID,
  name: 'report.txt',
  expectedSize: 128n,
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('v2 receiver product orchestration', () => {
  it('publishes retained v6 lifecycle inventory without acquiring receive authority', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.retainedOperations = Object.freeze([retainedOperation()])
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, onOutputTrace: (event) => traces.push(event.name) },
    )

    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    expect(controller.getSnapshot().retained.operations).toEqual(receive.retainedOperations)
    expect(receive.startedAuthorities).toHaveLength(0)
    expect(traces).toContain('receive.inventory.load.started')
    expect(traces).toContain('receive.inventory.load.completed')
    await controller.dispose()
  })

  it('consumes a retained action from the exact inventory and reloads after durable completion', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const operation = waitingRetainedOperation()
    receive.retainedOperations = Object.freeze([operation])
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = actionGate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, onOutputTrace: (event) => traces.push(event.name) },
    )
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    controller.performRetainedAction(Object.freeze({ ...operation }), 'save')
    expect(receive.retainedActionCalls).toHaveLength(0)

    controller.performRetainedAction(operation, 'save')
    expect(receive.retainedActionCalls).toMatchObject([{ operation, action: 'save' }])
    receive.retainedOperations = Object.freeze([])
    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await waitFor(() => receive.retainedSignals.length === 2 &&
      controller.getSnapshot().retained.kind === 'ready')

    expect(controller.getSnapshot().retained.operations).toEqual([])
    expect(traces).toContain('receive.inventory.action.started')
    expect(traces).toContain('receive.inventory.action.completed')
    expect(receive.retainedInventories[0]?.close).toHaveBeenCalledTimes(1)
    await controller.dispose()
  })

  it('fences a retained mutation that completes after controller disposal', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const operation = waitingRetainedOperation()
    receive.retainedOperations = Object.freeze([operation])
    const actionGate = deferred<V2RetainedReceiveActionResult>()
    receive.retainedActionGate = actionGate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, onOutputTrace: (event) => traces.push(event.name) },
    )
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')

    controller.performRetainedAction(operation, 'save')
    await controller.dispose()
    actionGate.resolve(Object.freeze({ kind: 'completed' }))
    await turns()

    expect(receive.retainedActionCalls[0]?.signal.aborted).toBe(true)
    expect(receive.retainedInventories[0]?.close).toHaveBeenCalledTimes(1)
    expect(traces).not.toContain('receive.inventory.action.completed')
  })

  it('adopts an exact fresh-page receive continuation without a projection-derived plan', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    receive.retainedOperations = Object.freeze([operation])
    receive.retainedActionGate = Promise.resolve(Object.freeze({
      kind: 'receive-continuation',
      runtime,
    }))
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

    controller.performRetainedAction(operation, 'continue')
    await waitFor(() => joined.transferRuns.length === 1)

    expect(joined.transferRuns[0]?.intent).toBe(intent)
    expect(controller.getSnapshot().output.projection).toBeNull()
    expect(controller.getSnapshot().output.receiveIntent?.digest).toBe(intent.digest)
    expect(controller.getSnapshot().output.lifecycle).toBe(runtime.lifecycle)
    expect(receive.startedAuthorities).toHaveLength(0)

    await controller.dispose()
    expect(runtime.detachments).toEqual(['detached'])
  })

  it('drops and closes a fresh-page continuation after reconnect changes its action epoch', async () => {
    const gate = deferred<V2RetainedReceiveActionResult>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    receive.retainedOperations = Object.freeze([operation])
    receive.retainedActionGate = gate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([joined]) as unknown as V2BrowserReceiverGateway,
      { receive, onOutputTrace: event => traces.push(event.name) },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

    controller.performRetainedAction(operation, 'continue')
    joined.protocolSessionId = identityText(4)
    gate.resolve(Object.freeze({ kind: 'receive-continuation', runtime }))
    await waitFor(() => runtime.detachments.length === 1)

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(traces).toContain('receive.inventory.action.failed')
    expect(traces).not.toContain('receive.inventory.action.completed')
    await controller.dispose()
  })

  it('rejects a continuation whose persisted operation identity changed', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const intent = await retainedWorkspaceIntent(joined)
    const { operation, runtime } = retainedReceiveContinuation(intent)
    const foreignOperation = Object.freeze({
      ...operation,
      operationId: identityText(99),
    })
    receive.retainedOperations = Object.freeze([foreignOperation])
    receive.retainedActionGate = Promise.resolve(Object.freeze({
      kind: 'receive-continuation',
      runtime,
    }))
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.kind === 'ready')
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

    controller.performRetainedAction(foreignOperation, 'continue')
    await waitFor(() => runtime.detachments.length === 1)

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    await controller.dispose()
  })

  it('fences a retained inventory result that resolves after controller disposal', async () => {
    const gate = deferred<V2RetainedReceiveInventory>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.retainedGate = gate.promise
    const traces: string[] = []
    const controller = new V2ReceiverController(
      new FakeGateway([]) as unknown as V2BrowserReceiverGateway,
      { receive, onOutputTrace: (event) => traces.push(event.name) },
    )

    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => receive.retainedSignals.length === 1)
    await controller.dispose()
    const lateInventory = retainedInventory(Object.freeze([retainedOperation()]))
    gate.resolve(lateInventory)
    await turns()

    expect(receive.retainedSignals[0]?.aborted).toBe(true)
    expect(lateInventory.close).toHaveBeenCalledTimes(1)
    expect(controller.getSnapshot().retained.kind).toBe('loading')
    expect(traces).toEqual(['receive.inventory.load.started'])
  })

})

describe('v2 receiver active operation orchestration', () => {
  it('runs projection as data, starts authority in the click stack, then freezes intent before transfer', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')
    expect(receive.startedAuthorities).toHaveLength(0)

    let inClickStack = true
    receive.clickStack = () => inClickStack
    controller.chooseArtifact('download-original')
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
    expect(controller.getSnapshot().output.lifecyclePresentation?.actions.map((action) => action.kind))
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
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

    const staleRequest = joined.projectionRequests[0]
    const currentEpoch = controller.getSnapshot().output.projection?.projection.epoch
    expect(staleRequest?.signal.aborted).toBe(true)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(currentEpoch).toBe(2n)
    expect(receive.startedAuthorities).toHaveLength(0)

    firstProjection.resolve()
    await turns()
    expect(controller.getSnapshot().output.projection?.projection.epoch).toBe(currentEpoch)
    expect(controller.getSnapshot().output.offerPresentation?.kind).toBe('actions')

    await controller.dispose()
  })

  it('reprojects a reconnected session before a stale action can start a picker', async () => {
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')

    joined.protocolSessionId = identityText(4)
    controller.chooseArtifact('download-original')

    expect(receive.startedAuthorities).toHaveLength(0)
    await waitFor(() => controller.getSnapshot().output.projection?.projection.epoch === 2n)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')
    expect(receive.startedAuthorities).toHaveLength(0)

    await controller.dispose()
  })

  it('rechecks current capability facts after acquisition and refuses an invalidated action', async () => {
    const finalCapability = deferred<EnvironmentOffers>()
    const receive = new FakeReceiveComposition(
      MANAGED_ENVIRONMENT,
      [MANAGED_ENVIRONMENT, finalCapability.promise, NO_DESTINATION_ENVIRONMENT],
    )
    const joined = new FakeJoinedShare(true)
    const controller = controllerFor(joined, receive)

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')
    controller.chooseArtifact('download-original')
    await waitFor(() => receive.environmentCalls >= 2)

    finalCapability.resolve(NO_DESTINATION_ENVIRONMENT)
    await waitFor(() => (receive.startedAuthorities[0]?.releaseReasons.length ?? 0) > 0)
    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'status')

    expect(joined.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().output.chosenAction).toBeNull()
    expect(controller.getSnapshot().output.offerPresentation).toMatchObject({
      kind: 'status',
      title: 'This browser cannot safely create the selected result.',
    })

    await controller.dispose()
  })

  it('releases a late authority result after a newer join owns the receiver', async () => {
    const authorityGate = deferred<V2StartedArtifactAuthority>()
    const receive = new FakeReceiveComposition(MANAGED_ENVIRONMENT)
    receive.authorityGate = authorityGate.promise
    const first = new FakeJoinedShare(true)
    const second = new FakeJoinedShare(true)
    const gateway = new FakeGateway([first, second])
    const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
      receive,
    })
    controller.initialize({ capabilityInput: 'first', pageUrl: 'https://receiver.invalid/s/share' })

    await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')
    controller.chooseArtifact('download-original')
    controller.submitKey('second')
    await waitFor(() => gateway.joinCount === 2)
    await waitFor(() => first.closeCount === 1)

    const late = new FakeStartedAuthority(
      receive.startedActions[0] ?? missingAction(),
      () => new FakeBoundRuntime(),
    )
    authorityGate.resolve(late)
    await waitFor(() => late.releaseReasons.length === 1)

    expect(first.transferRuns).toHaveLength(0)
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
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
    runtime.admissionSettlementError = new Error('secondary lifecycle settlement failed')

    run.reject(new V2TransferAdmissionFailureError(failure))
    await waitFor(() => controller.getSnapshot().error === failure.message)

    expect(controller.getSnapshot().error).toBe(failure.message)
    expect(runtime.admissionFailures).toEqual([failure])
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
    expect(controller.getSnapshot().output.lifecyclePresentation?.actions.map((action) => action.kind))
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

class FakeReceiveComposition implements V2ReceiveCompositionPort {
  readonly startedActions: ArtifactAction[] = []
  readonly startedAuthorities: FakeStartedAuthority[] = []
  readonly authorityStartStacks: boolean[] = []
  readonly retainedSignals: AbortSignal[] = []
  readonly retainedActionCalls: Array<Readonly<{
    operation: V2RetainedReceiveOperation
    action: V2RetainedReceiveAction
    signal: AbortSignal
  }>> = []
  readonly retainedInventories: V2RetainedReceiveInventory[] = []
  retainedOperations: readonly V2RetainedReceiveOperation[] = Object.freeze([])
  retainedGate: PromiseLike<V2RetainedReceiveInventory> | undefined
  retainedActionGate: PromiseLike<V2RetainedReceiveActionResult> | undefined
  readonly retained = Object.freeze({
    list: (signal: AbortSignal): PromiseLike<V2RetainedReceiveInventory> => {
      this.retainedSignals.push(signal)
      if (this.retainedGate !== undefined) return this.retainedGate
      const inventory = retainedInventory(
        this.retainedOperations,
        (operation, action, actionSignal) => {
          this.retainedActionCalls.push(Object.freeze({
            operation,
            action,
            signal: actionSignal,
          }))
          return this.retainedActionGate ?? Promise.resolve(Object.freeze({ kind: 'completed' }))
        },
      )
      this.retainedInventories.push(inventory)
      return Promise.resolve(inventory)
    },
  })
  readonly #fallback: EnvironmentOffers
  readonly #environmentQueue: Array<EnvironmentOffers | PromiseLike<EnvironmentOffers>>
  environmentCalls = 0
  clickStack: () => boolean = () => true
  authorityGate: PromiseLike<V2StartedArtifactAuthority> | undefined

  constructor(
    fallback: EnvironmentOffers,
    environmentQueue: Array<EnvironmentOffers | PromiseLike<EnvironmentOffers>> = [],
  ) {
    this.#fallback = fallback
    this.#environmentQueue = [...environmentQueue]
  }

  environment(): EnvironmentOffers | PromiseLike<EnvironmentOffers> {
    this.environmentCalls += 1
    return this.#environmentQueue.shift() ?? this.#fallback
  }

  startArtifactAuthority(
    action: ArtifactAction,
  ): V2StartedArtifactAuthority | PromiseLike<V2StartedArtifactAuthority> {
    this.startedActions.push(action)
    this.authorityStartStacks.push(this.clickStack())
    if (this.authorityGate !== undefined) return this.authorityGate
    const authority = new FakeStartedAuthority(action, (intent) => new FakeBoundRuntime(intent))
    this.startedAuthorities.push(authority)
    return authority
  }
}

class FakeStartedAuthority implements V2StartedArtifactAuthority {
  readonly releaseReasons: unknown[] = []
  readonly #action: ArtifactAction
  readonly #runtime: (intent: ReceiveIntent) => FakeBoundRuntime
  runtime: FakeBoundRuntime | undefined

  constructor(
    action: ArtifactAction,
    runtime: (intent: ReceiveIntent) => FakeBoundRuntime,
  ) {
    this.#action = action
    this.#runtime = runtime
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    signal.throwIfAborted()
    const acquired = await acquiredAuthority(this.#action)
    signal.throwIfAborted()
    const intent = await freezeIntent(acquired)
    signal.throwIfAborted()
    this.runtime = this.#runtime(intent)
    return this.runtime
  }

  release(reason: unknown): void {
    this.releaseReasons.push(reason)
  }
}

class FakeTransferControlError extends DOMException {
  readonly control: V2ActiveReceiveControl

  constructor(control: V2ActiveReceiveControl) {
    super(`${control} requested`, 'AbortError')
    this.control = control
  }
}

class FakeBoundRuntime implements V2BoundReceiveOperation {
  readonly plans = Object.freeze({}) as V2PlanExecutionAuthority
  readonly transferJobId = identityText(76)
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls: readonly V2ActiveReceiveControl[] = Object.freeze(['pause', 'stop'])
  readonly initialWorkspaceUsage: WorkspaceUsage = Object.freeze({
    ownedBytes: 512n,
    maximumBytes: 4_096n,
  })
  readonly interruptions: Array<{ control: V2ActiveReceiveControl; inClickStack: boolean }> = []
  readonly lifecycleActions: Array<{ action: LifecycleUserAction; inClickStack: boolean }> = []
  readonly expiryObservations: ReceiveLifecycleState[] = []
  readonly detachments: unknown[] = []
  readonly admissionFailures: unknown[] = []
  controlStack: () => boolean = () => true
  lifecycleActionStack: () => boolean = () => true
  lastControl: V2ActiveReceiveControl | undefined
  admissionSettlementError: unknown
  nextLifecycleAction: (
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ) => V2LifecycleMutation = defaultLifecycleAction

  constructor(
    intent: ReceiveIntent = {} as ReceiveIntent,
    lifecycle?: ReceiveLifecycleState,
  ) {
    this.intent = intent
    this.lifecycle = lifecycle ?? initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    })
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    this.lastControl = control
    this.interruptions.push({ control, inClickStack: this.controlStack() })
    transfer.abort(new FakeTransferControlError(control))
  }

  startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): V2LifecycleMutation {
    this.lifecycleActions.push({ action, inClickStack: this.lifecycleActionStack() })
    return this.nextLifecycleAction(action, lifecycle)
  }

  async observeExpiry(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    this.expiryObservations.push(lifecycle)
    const expiresAt = lifecycleDeadlineForTest(lifecycle)
    return {
      lifecycle: next(lifecycle, {
        kind: 'expired',
        priorStableState: stableKindForExpiry(lifecycle),
        expiresAt,
        cleanupState: 'cleanup-pending',
        expiryReceiptDigest: identityText(73, 32),
      }),
      workspaceUsage: this.initialWorkspaceUsage,
    }
  }

  resolveWorkspaceUsage(): WorkspaceUsage {
    return this.initialWorkspaceUsage
  }

  settleTransferAdmissionFailure(reason: unknown): V2LifecycleMutation {
    this.admissionFailures.push(reason)
    if (this.admissionSettlementError !== undefined) throw this.admissionSettlementError
    if (this.lastControl === 'pause') {
      return {
        lifecycle: next(this.lifecycle, {
          kind: 'resumable-receive',
          checkpointSetDigest: identityText(74, 32),
          completedFileCount: 0n,
          completedBytes: 0n,
          expiresAt: Date.now() + 60_000,
        }),
        workspaceUsage: this.initialWorkspaceUsage,
      }
    }
    return {
      lifecycle: next(this.lifecycle, {
        kind: 'restart-required',
        reason: 'direct-atomic-rolled-back',
        receiptDigest: identityText(75, 32),
      }),
    }
  }

  detach(): void {
    this.detachments.push('detached')
  }
}

class FakeJoinedShare {
  readonly descriptor = Object.freeze({
    shareInstance: identityBytes(10),
    shareInstanceId: identityText(10),
    syntheticRoot: identityBytes(1),
    syntheticRootId: ROOT_ID,
  })
  readonly recoveryIdentity = 'test-share'
  protocolSessionId = SESSION_ID
  readonly selection: V2SelectionPolicy
  readonly projectionRequests: Array<{ readonly signal: AbortSignal }> = []
  readonly transferRuns: FakeTransferRun[] = []
  readonly #projectionGates: Array<PromiseLike<void> | undefined>
  closeCount = 0

  constructor(defaultSelected: boolean, projectionGates: Array<PromiseLike<void> | undefined> = []) {
    this.selection = new V2SelectionPolicy(defaultSelected)
    this.#projectionGates = [...projectionGates]
  }

  rootDirectory(): V2BrowseDirectory {
    return Object.freeze({
      id: identityBytes(1),
      idText: ROOT_ID,
      name: 'Shared files',
      path: Object.freeze([]),
      ancestry: Object.freeze([ROOT_ID]),
    })
  }

  async page(directory: V2BrowseDirectory) {
    return Object.freeze({
      directory,
      pageIndex: 0,
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      entries: Object.freeze([FILE_ENTRY]),
    })
  }

  subscribeCatalogScanProgress(): () => void {
    return () => undefined
  }

  projectionSource(selection: V2FrozenSelectionPolicy): AuthenticatedDiscoverySource {
    const gate = this.#projectionGates.shift()
    const requests = this.projectionRequests
    return Object.freeze({
      discover: async function* (request: AuthenticatedDiscoveryRequest) {
        requests.push({ signal: request.signal })
        await gate
        const selected = selection.selected(FILE_ENTRY, [ROOT_ID])
        yield createAuthenticatedProjectionEvidence({
          generations: [{
            directoryId: ROOT_ID,
            generation: identityText(20 + requests.length),
          }],
          metrics: {
            fileCountLowerBound: selected ? 1 : 0,
            directoryCountLowerBound: 0,
            byteCountLowerBound: selected ? FILE_ENTRY.expectedSize : 0n,
          },
          ...(selected
            ? {
                representativeFile: {
                  fileId: FILE_ID,
                  sourcePath: FILE_ENTRY.name,
                  portableName: FILE_ENTRY.name,
                },
              }
            : {}),
          settledTargets: request.unsettledTargets,
        })
        return Object.freeze({ settledTargets: request.unsettledTargets })
      },
    })
  }

  beginDownloadConnectivity(): V2ConnectivityActivation {
    return {
      routes: Object.freeze({}) as never,
      close: () => undefined,
    }
  }

  transferJob(
    plans: V2PlanExecutionAuthority,
    intent: ReceiveIntent,
  ) {
    const run = new FakeTransferRun(plans, intent)
    this.transferRuns.push(run)
    return Object.freeze({ run: (signal?: AbortSignal) => run.start(signal) })
  }

  async close(): Promise<void> {
    this.closeCount += 1
  }
}

class FakeTransferRun {
  readonly plans: V2PlanExecutionAuthority
  readonly intent: ReceiveIntent
  readonly #result = deferred<TransferJobResult>()
  signal: AbortSignal | undefined

  constructor(plans: V2PlanExecutionAuthority, intent: ReceiveIntent) {
    this.plans = plans
    this.intent = intent
  }

  start(signal?: AbortSignal): Promise<TransferJobResult> {
    this.signal = signal
    signal?.addEventListener('abort', () => {
      const reason = signal.reason
      if (!(reason instanceof FakeTransferControlError)) {
        this.#result.reject(reason)
        return
      }
      const lifecycle = initialReceiveLifecycleState({
        operationId: this.intent.operationId,
        receiveIntentDigest: this.intent.digest,
      })
      this.resolve(reason.control === 'pause'
        ? next(lifecycle, {
            kind: 'resumable-receive',
            checkpointSetDigest: identityText(74, 32),
            completedFileCount: 0n,
            completedBytes: 0n,
            expiresAt: Date.now() + 60_000,
          })
        : next(lifecycle, {
            kind: 'restart-required',
            reason: 'direct-atomic-rolled-back',
            receiptDigest: identityText(75, 32),
          }), reason)
    }, { once: true })
    return this.#result.promise
  }

  resolve(lifecycle: ReceiveLifecycleState, abortReason?: unknown): void {
    this.#result.resolve({
      worker: Object.freeze({}) as never,
      lifecycle,
      measure: Object.freeze({}) as never,
      transferJobId: identityText(76),
      intent: this.intent,
      ...(abortReason === undefined ? {} : { abortReason }),
    })
  }

  reject(reason: unknown): void {
    this.#result.reject(reason)
  }
}

class FakeGateway {
  readonly #joined: FakeJoinedShare[]
  joinCount = 0

  constructor(joined: FakeJoinedShare[]) {
    this.#joined = joined
  }

  async join(): Promise<V2JoinedBrowserShare> {
    const joined = this.#joined[this.joinCount]
    this.joinCount += 1
    if (joined === undefined) throw new Error('unexpected join')
    return joined as unknown as V2JoinedBrowserShare
  }
}

function controllerFor(
  joined: FakeJoinedShare,
  receive: V2ReceiveCompositionPort,
): V2ReceiverController {
  const gateway = new FakeGateway([joined])
  const controller = new V2ReceiverController(gateway as unknown as V2BrowserReceiverGateway, {
    receive,
  })
  controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
  return controller
}

async function startTransfer(
  controller: V2ReceiverController,
  joined: FakeJoinedShare,
): Promise<void> {
  await waitFor(() => controller.getSnapshot().output.offerPresentation?.kind === 'actions')
  controller.chooseArtifact('download-original')
  await waitFor(() => joined.transferRuns.length === 1)
}

async function acquiredAuthority(action: ArtifactAction): Promise<AcquiredMaterializationAuthority> {
  const artifact = action.artifact
  if (artifact === null) throw new Error('test action does not have a settled artifact')
  switch (action.plan.kind) {
    case 'direct-atomic': {
      const reservation = await createManagedAtomicReservation({
        operationId: identityText(40),
        reservationId: identityText(41),
        artifact,
        authorityRef: identityText(42, 32),
        nameAuthority: action.plan.target.guarantees.nameAuthority as
          'application-chosen' | 'user-chosen',
        requestedName: action.suggestedName ?? FILE_ENTRY.name,
        reservedName: action.suggestedName ?? FILE_ENTRY.name,
        collisionIndex: 0,
      })
      return Object.freeze({
        kind: 'destination-reservation',
        environmentTargetOfferId: action.plan.target.id,
        reservation,
      })
    }
    case 'workspace-then-publish': {
      const workspace = await createWorkspaceBinding({
        operationId: identityText(43),
        workspaceId: identityText(44),
        artifact,
        repositoryRef: identityText(45, 32),
      })
      return Object.freeze({
        kind: 'workspace-binding',
        workspaceOfferId: action.plan.workspace.id,
        workspace,
      })
    }
    case 'direct-tree':
    case 'portable-handoff':
      throw new Error(`unsupported test plan ${action.plan.kind}`)
  }
}

async function retainedWorkspaceIntent(joined: FakeJoinedShare): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createOriginalFileArtifact({
    fileId: FILE_ID,
    sourcePath: FILE_ENTRY.name,
    suggestedName: FILE_ENTRY.name,
  })
  const workspace = await createWorkspaceBinding({
    operationId: identityText(90),
    workspaceId: identityText(91),
    artifact,
    repositoryRef: identityText(92, 32),
  })
  return createReceiveIntent({
    selection,
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
}

function retainedReceiveContinuation(intent: ReceiveIntent): Readonly<{
  operation: V2RetainedReceiveOperation
  runtime: FakeBoundRuntime
}> {
  const initial = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  const retained = next(initial, {
    kind: 'resumable-receive',
    checkpointSetDigest: identityText(93, 32),
    completedFileCount: 1n,
    completedBytes: 64n,
    expiresAt: Date.now() + 60_000,
  })
  const receiving = next(retained, {
    kind: 'receiving',
    activeLeaseId: identityText(94),
  })
  return Object.freeze({
    operation: Object.freeze({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      lifecycleGeneration: retained.generation,
      lifecycle: retained,
      continuation: 'resume-receive',
      actions: Object.freeze(['continue', 'discard'] as const),
    }),
    runtime: new FakeBoundRuntime(intent, receiving),
  })
}

function retainedOperation(): V2RetainedReceiveOperation {
  const lifecycle: ReceiveLifecycleState = Object.freeze({
    kind: 'needs-attention',
    operationId: identityText(80),
    receiveIntentDigest: identityText(81, 32),
    generation: 7n,
    reason: 'target-ownership-unknown',
    lastVerifiedRecordDigest: identityText(82, 32),
  })
  return Object.freeze({
    operationId: lifecycle.operationId,
    receiveIntentDigest: lifecycle.receiveIntentDigest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation: 'needs-attention',
    actions: Object.freeze([]),
    unavailableReason: 'Ownership needs attention; no automatic action is safe.',
  })
}

function waitingRetainedOperation(): V2RetainedReceiveOperation {
  const lifecycle: ReceiveLifecycleState = Object.freeze({
    kind: 'waiting-to-save',
    operationId: identityText(83),
    receiveIntentDigest: identityText(84, 32),
    generation: 4n,
    packageDigest: identityText(85, 32),
    expiresAt: Date.now() + 60_000,
  })
  return Object.freeze({
    operationId: lifecycle.operationId,
    receiveIntentDigest: lifecycle.receiveIntentDigest,
    lifecycleGeneration: lifecycle.generation,
    lifecycle,
    continuation: 'save-artifact',
    actions: Object.freeze(['save', 'discard'] as const),
  })
}

function retainedInventory(
  operations: readonly V2RetainedReceiveOperation[],
  act: V2RetainedReceiveInventory['act'] = () =>
    Promise.resolve(Object.freeze({ kind: 'completed' })),
): V2RetainedReceiveInventory {
  let open = true
  return Object.freeze({
    operations,
    act: (
      operation: V2RetainedReceiveOperation,
      action: V2RetainedReceiveAction,
      signal: AbortSignal,
    ) => {
      if (!open) return Promise.reject(new DOMException('Inventory is closed', 'InvalidStateError'))
      return act(operation, action, signal)
    },
    close: vi.fn(() => {
      open = false
    }),
  })
}

function stableLifecycle(
  lifecycle: ReceiveLifecycleState,
  kind: 'resumable-receive' | 'waiting-to-save' | 'download-started',
): ReceiveLifecycleState {
  switch (kind) {
    case 'resumable-receive':
      return next(lifecycle, {
        kind,
        checkpointSetDigest: identityText(50, 32),
        completedFileCount: 1n,
        completedBytes: 128n,
        expiresAt: Date.now() + 60_000,
      })
    case 'waiting-to-save':
      return next(lifecycle, {
        kind,
        packageDigest: identityText(51, 32),
        expiresAt: Date.now() + 60_000,
      })
    case 'download-started':
      return next(lifecycle, {
        kind,
        attemptKind: 'workspace',
        attemptId: identityText(52),
        packageDigest: identityText(53, 32),
        retryableUntil: Date.now() + 60_000,
      })
  }
}

function defaultLifecycleAction(
  action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
  lifecycle: ReceiveLifecycleState,
): V2LifecycleMutation {
  switch (action) {
    case 'continue':
      return {
        lifecycle: next(lifecycle, { kind: 'receiving', activeLeaseId: identityText(60) }),
        activeControls: Object.freeze(['pause', 'stop']),
        resumeTransfer: true,
      }
    case 'save':
      return {
        lifecycle: next(lifecycle, {
          kind: 'publishing-managed',
          activeLeaseId: identityText(61),
          packageDigest: identityText(62, 32),
          publicationAttemptId: identityText(63),
        }),
      }
    case 'redownload':
    case 'change-location':
      return {
        lifecycle: next(lifecycle, {
          kind: 'handing-off',
          activeLeaseId: identityText(64),
          attemptKind: 'workspace',
          attemptId: identityText(65),
          packageDigest: identityText(66, 32),
          retainedDeadline: Date.now() + 60_000,
        }),
      }
    case 'discard':
    case 'delete':
      return {
        lifecycle: next(lifecycle, {
          kind: 'discarded',
          cleanupReceiptDigest: identityText(67, 32),
        }),
      }
  }
}

function next(
  lifecycle: ReceiveLifecycleState,
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  return nextReceiveLifecycleState(lifecycle, payload)
}

function lifecycleDeadlineForTest(lifecycle: ReceiveLifecycleState): number {
  if (lifecycle.kind === 'resumable-receive' || lifecycle.kind === 'resumable-package' ||
      lifecycle.kind === 'waiting-to-save') return lifecycle.expiresAt
  if (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace') {
    return lifecycle.retryableUntil
  }
  throw new Error('test lifecycle does not have a stable deadline')
}

function stableKindForExpiry(
  lifecycle: ReceiveLifecycleState,
): Extract<ReceiveLifecycleState, { kind: 'expired' }>['priorStableState'] {
  if (lifecycle.kind === 'resumable-receive' || lifecycle.kind === 'resumable-package' ||
      lifecycle.kind === 'waiting-to-save') return lifecycle.kind
  if (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace') {
    return lifecycle.kind
  }
  throw new Error('test lifecycle cannot expire')
}

function missingAction(): ArtifactAction {
  throw new Error('authority action was not captured')
}

function identityBytes(seed: number, width = 16): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(width)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return value
}

function identityText(seed: number, width = 16): string {
  return encodeBase64Url(identityBytes(seed, width))
}

interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T | PromiseLike<T>): void
  reject(reason: unknown): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, decline) => {
    resolve = accept
    reject = decline
  })
  return { promise, resolve, reject }
}

async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return
    await new Promise<void>((resolve) => setTimeout(resolve, 1))
  }
  throw new Error('timed out waiting for controller state')
}

async function turns(): Promise<void> {
  for (let index = 0; index < 24; index += 1) await Promise.resolve()
}
