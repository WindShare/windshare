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
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
} from '../../output/planning'
import type { ReopenedDirectTreeOperation } from '../../output/resume/reopen-authority'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../output/workspace/state'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { classificationForTransferFailure } from '../../transfer/job/failures'
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
import { V2PresentationSourceError } from '../v2-receive-runtime'
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
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #released = false
  #claimed = false

  constructor(
    action: ArtifactAction,
    picked: Promise<AcquiredFSAParentAuthority>,
    diagnostics?: OutputDiagnosticsPorts,
  ) {
    this.#action = action
    this.#picked = picked
    this.#diagnostics = diagnostics
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    const authority = await this.#picked.catch((error: unknown) => {
      observeFSAReservationFailure(this.#diagnostics, error)
      throw error instanceof DOMException && error.name === 'AbortError'
        ? new V2PresentationSourceError('picker_refused', error)
        : error
    })
    signal.throwIfAborted()
    this.#requireLive()
    const artifact = requireDirectoryArtifact(this.#action)
    const repository = await IndexedDbReceiveOperationRepository.open().catch((error: unknown) => {
      observeFSAReservationFailure(this.#diagnostics, error)
      throw error
    })
    let session: FileSystemAccessOutputSession | undefined
    let lease: BrowserReceiveOperationLease | undefined
    try {
      session = await bindNewFileSystemAccessOutput({
        authority,
        artifact,
        operationRepository: repository,
        operationId: createOperationID(),
        ...(this.#diagnostics === undefined
          ? {}
          : { diagnostics: this.#diagnostics }),
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
        ...(this.#diagnostics === undefined
          ? {}
          : { diagnostics: this.#diagnostics }),
      })
      signal.throwIfAborted()
      return runtime
    } catch (error) {
      const cleanupFailures: unknown[] = []
      let outerCleanupFailureObserved = false
      try {
        await session?.close()
      } catch (cleanupFailure) {
        // The FSA session owns native cleanup classification.
        cleanupFailures.push(cleanupFailure)
      }
      try {
        await lease?.release()
      } catch (cleanupFailure) {
        cleanupFailures.push(cleanupFailure)
        outerCleanupFailureObserved = true
        recordOutputException(this.#diagnostics?.failures?.cleanup, cleanupFailure)
      }
      try {
        repository.close()
      } catch (cleanupFailure) {
        cleanupFailures.push(cleanupFailure)
        outerCleanupFailureObserved = true
        recordOutputException(this.#diagnostics?.failures?.cleanup, cleanupFailure)
      }
      if (outerCleanupFailureObserved) {
        emitOutputTrace(this.#diagnostics?.trace, () =>
          outputTraceEvent('cleanup', {
            backend: 'file_system_access',
            transition: 'failed',
          }))
      }
      if (cleanupFailures.length !== 0) {
        throw new AggregateError(
          [error, ...cleanupFailures],
          'FSA receive activation failed and could not release all authorities',
          { cause: error },
        )
      }
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
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
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
    diagnostics?: OutputDiagnosticsPorts
  }) {
    this.intent = input.intent
    this.lifecycle = input.lifecycle
    this.#repository = input.repository
    this.#lease = input.lease
    this.#closeAuthority = input.closeAuthority
    this.#diagnostics = input.diagnostics
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
    diagnostics?: OutputDiagnosticsPorts
  }): Promise<FSAReceiveOperation> {
    const attempt = await createFSAAttempt(
      input.intent,
      input.repository,
      input.lease,
      input.session,
      'start',
      undefined,
      input.diagnostics,
    )
    return new FSAReceiveOperation({ ...input, ...attempt })
  }

  static async reopen(
    operation: ReopenedDirectTreeOperation,
    diagnostics?: OutputDiagnosticsPorts,
  ): Promise<FSAReceiveOperation> {
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
      diagnostics,
    )
    let session: FileSystemAccessOutputSession | undefined
    try {
      session = await reopenFileSystemAccessOutput({
        intent: operation.intent,
        operationRepository: operation.repository,
        ...(diagnostics === undefined ? {} : { diagnostics }),
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
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
    } catch (error) {
      return settleFailedFSAReopen(
        attemptAuthority.settlement,
        operation.intent,
        session,
        error,
      )
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
      ...(this.#diagnostics === undefined
        ? {}
        : { diagnostics: this.#diagnostics }),
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
        this.#diagnostics,
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
      return closeFSAContinuationAfterFailure(session, error)
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
    let outerFailureObserved = false
    await this.#session.close().catch((error: unknown) => {
      // The output session owns classification for file, checkpoint, and root release.
      failures.push(error)
    })
    await (this.#closeAuthority?.() ?? this.#lease.release())
      .catch((error: unknown) => {
        failures.push(error)
        outerFailureObserved = true
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      })
    if (this.#closeAuthority === undefined) {
      try {
        this.#repository.close()
      } catch (error) {
        failures.push(error)
        outerFailureObserved = true
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      }
    }
    if (failures.length > 0) {
      const failure = new AggregateError(failures, 'FSA receive resources did not detach')
      if (outerFailureObserved) {
        emitOutputTrace(this.#diagnostics?.trace, () =>
          outputTraceEvent('cleanup', {
            backend: 'file_system_access',
            transition: 'failed',
          }))
      }
      throw failure
    }
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}

async function settleFailedFSAReopen(
  settlement: FileSystemAccessOperationSettlementAuthority,
  intent: ReceiveIntent,
  session: FileSystemAccessOutputSession | undefined,
  error: unknown,
): Promise<never> {
  let cleanupFailure: unknown
  try {
    await session?.close()
  } catch (caughtCleanupFailure) {
    // The output session emitted the cleanup fact at its native boundary.
    cleanupFailure = caughtCleanupFailure
  }
  try {
    await settlement.settleExecutionAdmissionFailure(
      intent,
      error,
      new AbortController().signal,
    )
  } catch (settlementError) {
    const consequences = [
      classificationForTransferFailure(settlementError, {
        stage: 'settlement',
        relation: 'consequence',
      }),
      ...(cleanupFailure === undefined
        ? []
        : [classificationForTransferFailure(cleanupFailure, {
            stage: 'cleanup',
            relation: 'consequence',
          })]),
    ].filter(candidate => candidate !== undefined)
    if (consequences.length !== 0) {
      throw new V2TransferFailureSettlementError(
        classificationForTransferFailure(error, {
          stage: 'output_reservation',
          relation: 'contributor',
        }),
        consequences,
      )
    }
  }
  if (cleanupFailure !== undefined) {
    throw new AggregateError(
      [error, cleanupFailure],
      'FSA reopen failed and output cleanup also failed',
      { cause: error },
    )
  }
  throw error
}

async function closeFSAContinuationAfterFailure(
  session: FileSystemAccessOutputSession,
  error: unknown,
): Promise<never> {
  let cleanupFailure: unknown
  try {
    await session.close()
  } catch (caughtCleanupFailure) {
    cleanupFailure = caughtCleanupFailure
  }
  if (cleanupFailure !== undefined) {
    throw new AggregateError(
      [error, cleanupFailure],
      'FSA continuation failed and output cleanup also failed',
      { cause: error },
    )
  }
  throw error
}

function observeFSAReservationFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.outputReservation, error)
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent('output_reservation', {
      backend: 'file_system_access',
      transition: 'failed',
    }))
}

async function createFSAAttempt(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  session: FileSystemAccessOutputSession,
  lifecycleEntry: 'start' | 'resume',
  admissionFallback?: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
  diagnostics?: OutputDiagnosticsPorts,
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
    diagnostics,
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
  diagnostics?: OutputDiagnosticsPorts,
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
    ...(diagnostics === undefined ? {} : { diagnostics }),
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
