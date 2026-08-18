import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import type { AcquiredFSAParentAuthority } from '../../output/capability/contract'
import {
  createFileSystemAccessSettlementAuthority,
  type FileSystemAccessOperationSettlementAuthority,
} from '../../output/file-system-access/settlement'
import {
  bindNewFileSystemAccessOutput,
  reopenFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../../output/file-system-access/session'
import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
} from '../../output/planning'
import type { ReopenedDirectTreeOperation } from '../../output/resume/reopen-authority'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { createPersistentDirectTreeExecution } from '../../transfer/settlement/persistent-execution'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import {
  createV2PlanExecutionAuthority,
} from '../../transfer/settlement/v2-plan-authority'
import {
  createOperationID,
  createOutputSessionID,
  createTransferJobID,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  TransferPauseRequestedError,
  outputSessionIdentity,
  type V2PlanExecutionAuthority,
} from '../../transfer/output-session'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
} from '../v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2StartedArtifactAuthority,
} from '../v2-receive-runtime'
import {
  operationDigest,
  readLifecycle,
  requireDirectoryArtifact,
  transitionLifecycle,
  unavailableRoute,
} from './shared'

export class StartedFSAReceive implements V2StartedArtifactAuthority {
  readonly #action: ArtifactAction
  readonly #picked: Promise<AcquiredFSAParentAuthority>
  #released = false
  #claimed = false

  constructor(
    action: ArtifactAction,
    picked: Promise<AcquiredFSAParentAuthority>,
  ) {
    this.#action = action
    this.#picked = picked
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    const authority = await this.#picked
    signal.throwIfAborted()
    this.#requireLive()
    const artifact = requireDirectoryArtifact(this.#action)
    const repository = await IndexedDbReceiveOperationRepository.open()
    let session: FileSystemAccessOutputSession | undefined
    let lease: BrowserReceiveOperationLease | undefined
    try {
      session = await bindNewFileSystemAccessOutput({
        authority,
        artifact,
        operationRepository: repository,
        operationId: createOperationID(),
        freezeIntent: async (reservation) => {
          signal.throwIfAborted()
          this.#requireLive()
          return freezeIntent(Object.freeze({
            kind: 'destination-reservation',
            environmentTargetOfferId: authority.environmentTargetOfferId,
            reservation,
          }))
        },
      })
      signal.throwIfAborted()
      const lifecycle = initialReceiveLifecycleState({
        operationId: session.intent.operationId,
        receiveIntentDigest: session.intent.digest,
      })
      lease = await acquireBrowserReceiveOperationLease(
        repository,
        session.intent.operationId,
        { acquireTransition: { lifecycle } },
      )
      const runtime = await FSAReceiveOperation.create({
        intent: session.intent,
        lifecycle,
        session,
        repository,
        lease,
      })
      signal.throwIfAborted()
      return runtime
    } catch (error) {
      await session?.close().catch(() => undefined)
      await lease?.release().catch(() => undefined)
      repository.close()
      throw error
    }
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) throw new DOMException('Artifact authority was already finalized', 'InvalidStateError')
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }
}

export class FSAReceiveOperation implements V2BoundReceiveOperation {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['pause'] as const)
  readonly initialWorkspaceUsage = null
  readonly #repository: ReceiveOperationRepository
  readonly #lease: BrowserReceiveOperationLease
  readonly #closeAuthority: (() => Promise<void>) | undefined
  #session: FileSystemAccessOutputSession
  #settlement: FileSystemAccessOperationSettlementAuthority
  #plans: V2PlanExecutionAuthority
  #transferJobId: string
  #detached = false

  private constructor(input: {
    intent: ReceiveIntent
    lifecycle: ReceiveLifecycleState
    repository: ReceiveOperationRepository
    lease: BrowserReceiveOperationLease
    session: FileSystemAccessOutputSession
    settlement: FileSystemAccessOperationSettlementAuthority
    closeAuthority?: () => Promise<void>
    plans: V2PlanExecutionAuthority
    transferJobId: string
  }) {
    this.intent = input.intent
    this.lifecycle = input.lifecycle
    this.#repository = input.repository
    this.#lease = input.lease
    this.#closeAuthority = input.closeAuthority
    this.#session = input.session
    this.#settlement = input.settlement
    this.#plans = input.plans
    this.#transferJobId = input.transferJobId
  }

  static async create(input: {
    intent: ReceiveIntent
    lifecycle: ReceiveLifecycleState
    repository: IndexedDbReceiveOperationRepository
    lease: BrowserReceiveOperationLease
    session: FileSystemAccessOutputSession
  }): Promise<FSAReceiveOperation> {
    const attempt = await createFSAAttempt(input.intent, input.repository, input.lease, input.session, 'start')
    return new FSAReceiveOperation({ ...input, ...attempt })
  }

  static async reopen(operation: ReopenedDirectTreeOperation): Promise<FSAReceiveOperation> {
    if (operation.lifecycle.kind !== 'receiving') {
      throw new TypeError('Direct-tree continuation requires active receive lifecycle state')
    }
    const admissionFallback = operation.receiveAdmissionFallback
    if (admissionFallback === undefined) {
      throw new TypeError('Direct-tree continuation omitted its admission fallback')
    }
    const attemptAuthority = await createFSAAttemptSettlement(
      operation.intent,
      operation.repository,
      operation.lease,
      admissionFallback,
    )
    let session: FileSystemAccessOutputSession | undefined
    try {
      session = await reopenFileSystemAccessOutput({
        intent: operation.intent,
        operationRepository: operation.repository,
      })
      const plans = await createFSAPlanAuthority(
        operation.intent,
        operation.repository,
        operation.lease,
        session,
        'resume',
        attemptAuthority.settlement,
      )
      return new FSAReceiveOperation({
        intent: operation.intent,
        lifecycle: operation.lifecycle,
        repository: operation.repository,
        lease: operation.lease,
        session,
        closeAuthority: () => operation.close(),
        settlement: attemptAuthority.settlement,
        plans,
        transferJobId: attemptAuthority.transferJobId,
      })
    } catch (error) {
      await session?.close().catch(() => undefined)
      try {
        await attemptAuthority.settlement.settleExecutionAdmissionFailure(
          operation.intent,
          error,
          new AbortController().signal,
        )
      } catch (settlementError) {
        throw new V2TransferFailureSettlementError(error, [settlementError])
      }
      throw error
    }
  }

  get plans(): V2PlanExecutionAuthority {
    return this.#plans
  }

  get transferJobId(): string {
    return this.#transferJobId
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    if (control !== 'pause') throw unavailableRoute()
    transfer.abort(new TransferPauseRequestedError())
  }

  async startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    if (action !== 'continue' || lifecycle.kind !== 'resumable-receive') throw unavailableRoute()
    const session = await reopenFileSystemAccessOutput({
      intent: this.intent,
      operationRepository: this.#repository,
    })
    try {
      // Acquire the attempt before leaving the stable state so setup failures retain
      // the exact checkpoint deadline and never require compensating durable writes.
      const attempt = await createFSAAttempt(
        this.intent,
        this.#repository,
        this.#lease,
        session,
        'resume',
        lifecycle,
      )
      const resumed = await transitionLifecycle(
        this.#repository,
        this.intent,
        this.#lease.leaseId,
        { kind: 'resume-started' },
        lifecycle,
      )
      this.#session = session
      this.#settlement = attempt.settlement
      this.#plans = attempt.plans
      this.#transferJobId = attempt.transferJobId
      return Object.freeze({
        lifecycle: resumed,
        activeControls: this.activeControls,
        resumeTransfer: true,
      })
    } catch (error) {
      await session.close().catch(() => undefined)
      throw error
    }
  }

  async observeExpiry(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const expiryReceiptDigest = await operationDigest(this.intent, 'fsa-expiry')
    const expired = await transitionLifecycle(
      this.#repository,
      this.intent,
      this.#lease.leaseId,
      {
        kind: 'expiry-observed',
        expiryReceiptDigest,
        cleanupState: 'clean',
      },
      lifecycle,
    )
    return Object.freeze({ lifecycle: expired, workspaceUsage: null })
  }

  resolveWorkspaceUsage(): null {
    return null
  }

  async settleTransferAdmissionFailure(reason: unknown): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const controller = new AbortController()
    const lifecycle = await this.#settlement.settleExecutionAdmissionFailure(
      this.intent,
      reason,
      controller.signal,
    )
    return Object.freeze({ lifecycle, workspaceUsage: null })
  }

  async detach(): Promise<void> {
    if (this.#detached) return
    this.#detached = true
    const failures: unknown[] = []
    await this.#session.close().catch((error: unknown) => failures.push(error))
    await (this.#closeAuthority?.() ?? this.#lease.release())
      .catch((error: unknown) => failures.push(error))
    if (this.#closeAuthority === undefined) this.#repository.close()
    if (failures.length > 0) {
      throw new AggregateError(failures, 'FSA receive resources did not detach')
    }
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}

async function createFSAAttempt(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  session: FileSystemAccessOutputSession,
  lifecycleEntry: 'start' | 'resume',
  admissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
): Promise<Readonly<{
  settlement: FileSystemAccessOperationSettlementAuthority
  plans: V2PlanExecutionAuthority
  transferJobId: string
}>> {
  const attemptAuthority = await createFSAAttemptSettlement(
    intent,
    repository,
    lease,
    admissionFallback,
  )
  const plans = await createFSAPlanAuthority(
    intent,
    repository,
    lease,
    session,
    lifecycleEntry,
    attemptAuthority.settlement,
  )
  return Object.freeze({ ...attemptAuthority, plans })
}

async function createFSAAttemptSettlement(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  admissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
): Promise<Readonly<{
  settlement: FileSystemAccessOperationSettlementAuthority
  transferJobId: string
}>> {
  const transferJobId = createTransferJobID()
  const settlement = await createFileSystemAccessSettlementAuthority({
    intent,
    repository,
    lifecycleLeaseId: lease.leaseId,
    transferJobId,
    ...(admissionFallback === undefined ? {} : { admissionFallback }),
  })
  return Object.freeze({ settlement, transferJobId })
}

async function createFSAPlanAuthority(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  session: FileSystemAccessOutputSession,
  lifecycleEntry: 'start' | 'resume',
  settlement: FileSystemAccessOperationSettlementAuthority,
): Promise<V2PlanExecutionAuthority> {
  const plans = await createV2PlanExecutionAuthority({
    intent,
    routes: {
      directTree: {
        open: async (boundIntent, signal) => {
          if (lifecycleEntry === 'start') {
            await transitionLifecycle(repository, intent, lease.leaseId, { kind: 'receive-started' })
          } else {
            const lifecycle = await readLifecycle(repository, intent.operationId)
            if (lifecycle.kind !== 'receiving' || lifecycle.activeLeaseId !== lease.leaseId) {
              throw new DOMException('Direct-tree continuation lost its active lifecycle lease', 'InvalidStateError')
            }
          }
          signal.throwIfAborted()
          return createPersistentDirectTreeExecution({
            intent: boundIntent,
            materialization: session,
            outputIdentity: outputSessionIdentity({
              backend: 'browser-fsa-tree',
              outputSessionId: createOutputSessionID(),
            }),
            settlement: settlement.bindMaterialization(session),
          })
        },
      },
      lifecycle: settlement,
    },
  })
  return plans
}
