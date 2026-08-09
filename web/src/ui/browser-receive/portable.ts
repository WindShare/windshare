import { createWindowBrowserHandoffPublisher } from '../../output/portable/browser-download'
import {
  createPortableExecutionRoutes,
  type PortableAbortRecord,
  type PortableAdmissionRejectionRecord,
  type PortableDownloadStartedRecord,
  type PortableExecutionLifecycleAuthority,
} from '../../output/portable/preparation'
import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
} from '../../output/planning'
import { IndexedDbZipCentralDirectorySpool } from '../../output/streams/zip-spool'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../../output/workspace/lifecycle'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import {
  createV2PlanExecutionAuthority,
  type V2UnopenedExecutionLifecycle,
} from '../../transfer/settlement/v2-plan-authority'
import {
  createOperationID,
  createPortableBinding,
  createPortablePlanID,
  createTransferJobID,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  TransferPauseRequestedError,
  type ExactPreparationEvidence,
  type ExecutionAdmissionResult,
  type PortableExecution,
  type V2PlanExecutionAuthority,
} from '../../transfer/output-session'
import type { V2ActiveReceiveControl } from '../v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import {
  operationDigest,
  requirePortableAction,
  requireSameIntent,
  unavailableRoute,
} from './shared'

type LifecycleEventPayload = LifecycleEvent extends infer Event
  ? Event extends LifecycleEvent
    ? Omit<Event, 'expectedGeneration' | 'leaseId'>
    : never
  : never

export class StartedPortableReceive implements V2StartedArtifactAuthority {
  readonly #window: BrowserReceiveWindow
  readonly #action: ArtifactAction
  #released = false
  #claimed = false

  constructor(windowPort: BrowserReceiveWindow, action: ArtifactAction) {
    this.#window = windowPort
    this.#action = action
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    signal.throwIfAborted()
    const action = requirePortableAction(this.#action)
    const portable = await createPortableBinding({
      operationId: createOperationID(),
      portablePlanId: createPortablePlanID(),
      artifact: action.artifact,
    })
    const intent = await freezeIntent(Object.freeze({
      kind: 'portable-binding',
      portableOfferId: action.plan.portable.id,
      handoffTargetOfferId: action.plan.handoffTarget.id,
      portable,
    }))
    signal.throwIfAborted()
    this.#requireLive()
    return PortableReceiveOperation.create(this.#window, action, intent)
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) {
      throw new DOMException('Portable artifact authority was already finalized', 'InvalidStateError')
    }
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }
}

class PortableReceiveOperation implements
V2BoundReceiveOperation,
PortableExecutionLifecycleAuthority,
V2UnopenedExecutionLifecycle {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['stop'] as const)
  readonly initialWorkspaceUsage = null
  readonly #leaseId = createOperationID()
  readonly #attemptId = createOperationID()
  #state: ReceiveLifecycleState
  #plans!: V2PlanExecutionAuthority
  #transferJobId = createTransferJobID()
  #preparationStarted = false
  #detached = false

  private constructor(intent: ReceiveIntent) {
    this.intent = intent
    this.lifecycle = initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    })
    this.#state = this.lifecycle
  }

  static async create(
    windowPort: BrowserReceiveWindow,
    action: ArtifactAction,
    intent: ReceiveIntent,
  ): Promise<PortableReceiveOperation> {
    const owner = new PortableReceiveOperation(intent)
    owner.#plans = await portablePlanAuthority(windowPort, action, intent, owner)
    return owner
  }

  get plans(): V2PlanExecutionAuthority {
    return this.#plans
  }

  get transferJobId(): string {
    return this.#transferJobId
  }

  get attemptId(): string {
    return this.#attemptId
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    if (control !== 'stop') throw unavailableRoute()
    transfer.abort(new TransferPauseRequestedError('Portable receive stopped by receiver'))
  }

  startLifecycleAction(): V2LifecycleMutation {
    throw unavailableRoute()
  }

  observeExpiry(): Promise<V2LifecycleMutation> {
    return Promise.reject(unavailableRoute())
  }

  resolveWorkspaceUsage(): null {
    return null
  }

  async abandon(reason: unknown): Promise<V2LifecycleMutation> {
    const state = await this.abortUnopened(this.intent, reason, new AbortController().signal)
    return Object.freeze({ lifecycle: state, workspaceUsage: null })
  }

  detach(): void {
    this.#detached = true
  }

  async beginPreparation(): Promise<void> {
    if (this.#preparationStarted) return
    this.#preparationStarted = true
    this.#state = this.#reduce({
      kind: 'receive-started',
      preparationId: this.#attemptId,
    })
  }

  admitPreparation(): void {
    this.#state = this.#reduce({ kind: 'preparation-admitted' })
  }

  async rejectAdmission(
    record: PortableAdmissionRejectionRecord,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { kind: 'discarded' | 'needs-attention' }>> {
    requireSameIntent(this.intent, record.intent)
    signal.throwIfAborted()
    await this.beginPreparation()
    this.#state = this.#reduce({
      kind: 'preparation-rejected',
      reason: record.reason,
      cleanupReceiptDigest: await operationDigest(this.intent, `portable-rejection:${record.reason}`),
    })
    return this.#state as Extract<ReceiveLifecycleState, { kind: 'discarded' | 'needs-attention' }>
  }

  async recordDownloadStarted(
    record: PortableDownloadStartedRecord,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, {
    kind: 'download-started'
    attemptKind: 'portable'
  }>> {
    requireSameIntent(this.intent, record.intent)
    signal.throwIfAborted()
    this.#state = this.#reduce({
      kind: 'handoff-requested',
      attemptKind: 'portable',
      attemptId: record.attemptId,
    })
    this.#state = this.#reduce({ kind: 'handoff-started' })
    return this.#state as Extract<ReceiveLifecycleState, {
      kind: 'download-started'
      attemptKind: 'portable'
    }>
  }

  async recordAbort(
    record: PortableAbortRecord,
  ): Promise<Extract<ReceiveLifecycleState, {
    kind: 'restart-required' | 'discarded' | 'needs-attention'
  }>> {
    requireSameIntent(this.intent, record.intent)
    this.#state = this.#reduce({
      kind: 'restart-boundary-verified',
      reason: record.reason,
      receiptDigest: await operationDigest(
        this.intent,
        `portable-abort:${record.attemptId}:${record.cleanup}`,
      ),
    })
    return this.#state as Extract<ReceiveLifecycleState, {
      kind: 'restart-required' | 'discarded' | 'needs-attention'
    }>
  }

  async abortUnopened(
    intent: ReceiveIntent,
    ...[, signal]: [reason: unknown, signal: AbortSignal]
  ): Promise<ReceiveLifecycleState> {
    requireSameIntent(this.intent, intent)
    signal.throwIfAborted()
    if (this.#state.kind === 'intent-frozen') {
      this.#state = this.#reduce({
        kind: 'cleanup-verified',
        cleanupReceiptDigest: await operationDigest(this.intent, 'portable-unopened'),
      })
      return this.#state
    }
    if (this.#state.kind === 'preparing') {
      return this.rejectAdmission({
        intent: this.intent,
        reason: 'generation-mismatch',
      }, signal)
    }
    if (this.#state.kind === 'receiving') {
      return this.recordAbort({
        intent: this.intent,
        attemptId: this.#attemptId,
        reason: 'portable-aborted',
        cleanup: 'clean',
      })
    }
    return this.#state
  }

  async recordSettlementUnknown(
    intent: ReceiveIntent,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>> {
    requireSameIntent(this.intent, intent)
    this.#state = this.#reduce({
      kind: 'ownership-unknown',
      lastVerifiedRecordDigest: await operationDigest(this.intent, 'portable-settlement-unknown'),
    })
    return this.#state as Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>
  }

  #reduce(event: LifecycleEventPayload): ReceiveLifecycleState {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
    const reduction = reduceReceiveLifecycle(this.#state, {
      ...event,
      expectedGeneration: this.#state.generation,
      leaseId: this.#leaseId,
    } as LifecycleEvent, {
      planKind: 'portable-handoff',
      preparationRequired: true,
      activeLeaseId: this.#leaseId,
      nowMilliseconds: Date.now(),
    })
    if (reduction.status !== 'applied') throw new TypeError('portable lifecycle transition became stale')
    return reduction.state
  }
}

async function portablePlanAuthority(
  windowPort: BrowserReceiveWindow,
  action: ArtifactAction,
  intent: ReceiveIntent,
  owner: PortableReceiveOperation,
): Promise<V2PlanExecutionAuthority> {
  const portableAction = requirePortableAction(action)
  const routes = createPortableExecutionRoutes({
    environment: {
      portable: portableAction.plan.portable,
      handoffTarget: portableAction.plan.handoffTarget,
    },
    attemptId: owner.attemptId,
    publisher: createWindowBrowserHandoffPublisher(windowPort),
    assembly: {
      Blob: windowPort.Blob,
      WritableStream: windowPort.WritableStream,
    },
    lifecycle: owner,
    createZipSpool: () => new IndexedDbZipCentralDirectorySpool({
      namespace: `portable-${intent.operationId}`,
    }),
  })
  const wrap = (
    route: NonNullable<typeof routes.portableOriginal> | NonNullable<typeof routes.portableZip>,
  ) => Object.freeze({
    prepare: async (
      boundIntent: Parameters<typeof route.prepare>[0],
      evidence: ExactPreparationEvidence,
      signal: AbortSignal,
    ): Promise<ExecutionAdmissionResult<PortableExecution>> => {
      await owner.beginPreparation()
      const result = await route.prepare(
        boundIntent as never,
        evidence,
        signal,
      )
      if (result.kind === 'accepted') owner.admitPreparation()
      return result
    },
  })
  return createV2PlanExecutionAuthority({
    intent,
    routes: {
      ...(routes.portableOriginal === undefined
        ? {}
        : { portableOriginal: wrap(routes.portableOriginal) }),
      ...(routes.portableZip === undefined
        ? {}
        : { portableZip: wrap(routes.portableZip) }),
      lifecycle: owner,
    },
  })
}
