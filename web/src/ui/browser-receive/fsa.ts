import {
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import {
  createFileSystemAccessSettlementAuthority,
  type FileSystemAccessOperationSettlementAuthority,
} from '../../output/file-system-access/settlement'
import {
  reopenFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../../output/file-system-access/session'
import type {
  LocalOutputOperationFailureDiagnosticsPort,
  OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import type { ReopenedDirectTreeOperation } from '../../output/resume/reopen-authority'
import type { CompatibleNameRepairProjectionSource } from '../../output/file-system-access/compatible-name/coordinator'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { classificationForTransferFailure } from '../../transfer/job/failures'
import { createPersistentDirectTreeExecution } from '../../transfer/settlement/persistent-execution'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import {
  createV2PlanExecutionAuthority,
} from '../../transfer/settlement/v2-plan-authority'
import {
  createOutputSessionID,
  createTransferJobID,
  type ReceiveIntent,
} from '../../transfer/intent'
import {
  TransferPauseRequestedError,
  TransferStopRequestedError,
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
} from '../v2-receive-runtime'
import {
  operationDigest,
  readLifecycle,
  transitionLifecycle,
  unavailableRoute,
} from './shared'
import { FSAResourceOwner } from './fsa-resource-owner'

interface FSAAttemptIdentitySource {
  readonly createOutputSessionId: () => string
  readonly createTransferJobId: () => string
}

const defaultAttemptIdentitySource: FSAAttemptIdentitySource = Object.freeze({
  createOutputSessionId: createOutputSessionID,
  createTransferJobId: createTransferJobID,
})

export class FSAReceiveOperation implements V2BoundReceiveOperation {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['pause', 'stop'] as const)
  readonly initialWorkspaceUsage = null
  readonly repairProjection?: CompatibleNameRepairProjectionSource
  readonly #repository: ReceiveOperationRepository
  readonly #lease: BrowserReceiveOperationLease
  readonly #resources: FSAResourceOwner
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined
  readonly #attemptIdentities: FSAAttemptIdentitySource
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
    resources: FSAResourceOwner
    plans: V2PlanExecutionAuthority
    transferJobId: string
    attemptIdentities: FSAAttemptIdentitySource
    diagnostics?: OutputDiagnosticsPorts
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort
  }) {
    this.intent = input.intent
    this.lifecycle = input.lifecycle
    this.#repository = input.repository
    this.#lease = input.lease
    this.#resources = input.resources
    const repairProjection = input.session.repairProjection
    if (repairProjection !== undefined) this.repairProjection = repairProjection
    this.#diagnostics = input.diagnostics
    this.#localOutputFailures = input.localOutputFailures
    this.#attemptIdentities = input.attemptIdentities
    this.#settlement = input.settlement
    this.#plans = input.plans
    this.#transferJobId = input.transferJobId
  }

  static async createCommitted(input: {
    intent: ReceiveIntent
    lifecycle: ReceiveLifecycleState
    repository: ReceiveOperationRepository
    lease: BrowserReceiveOperationLease
    session: FileSystemAccessOutputSession
    settlement: FileSystemAccessOperationSettlementAuthority
    transferJobId: string
    outputSessionId: string
    attemptIdentities: FSAAttemptIdentitySource
    resources: FSAResourceOwner
    diagnostics?: OutputDiagnosticsPorts
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort
  }): Promise<FSAReceiveOperation> {
    const plans = await createFSAPlanAuthority(
      input.intent,
      input.repository,
      input.lease,
      input.session,
      'start',
      input.settlement,
      input.outputSessionId,
    )
    return new FSAReceiveOperation({ ...input, plans })
  }

  static async reopen(
    operation: ReopenedDirectTreeOperation,
    diagnostics?: OutputDiagnosticsPorts,
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort,
    attemptIdentities: FSAAttemptIdentitySource = defaultAttemptIdentitySource,
  ): Promise<FSAReceiveOperation> {
    if (operation.lifecycle.kind !== 'receiving') {
      throw new TypeError('Direct-tree continuation requires active receive lifecycle state')
    }
    const admissionFallback = operation.receiveAdmissionFallback
    if (admissionFallback === undefined) {
      throw new TypeError('Direct-tree continuation omitted its admission fallback')
    }
    const transferJobId = attemptIdentities.createTransferJobId()
    const attemptAuthority = await createFSAAttemptSettlement(
      operation.intent,
      operation.repository,
      operation.lease,
      admissionFallback,
      diagnostics,
      transferJobId,
    )
    let session: FileSystemAccessOutputSession | undefined
    const outputSessionId = attemptIdentities.createOutputSessionId()
    try {
      session = await reopenFileSystemAccessOutput({
        intent: operation.intent,
        operationRepository: operation.repository,
        ...(diagnostics === undefined ? {} : { diagnostics }),
        ...stageDiagnosticsOption(
          localOutputFailures,
          diagnostics?.failures?.attempt,
          transferJobId,
          outputSessionId,
        ),
      })
      const plans = await createFSAPlanAuthority(
        operation.intent,
        operation.repository,
        operation.lease,
        session,
        'resume',
        attemptAuthority.settlement,
        outputSessionId,
      )
      const resources = new FSAResourceOwner({
        outputSession: session,
        closeOperationAuthority: () => operation.close(),
        ...(diagnostics === undefined ? {} : { diagnostics }),
      })
      return new FSAReceiveOperation({
        intent: operation.intent,
        lifecycle: operation.lifecycle,
        repository: operation.repository,
        lease: operation.lease,
        session,
        resources,
        settlement: attemptAuthority.settlement,
        plans,
        transferJobId: attemptAuthority.transferJobId,
        attemptIdentities,
        ...(diagnostics === undefined ? {} : { diagnostics }),
        ...(localOutputFailures === undefined ? {} : { localOutputFailures }),
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
    switch (control) {
      case 'pause':
        transfer.abort(new TransferPauseRequestedError())
        return
      case 'stop':
        transfer.abort(new TransferStopRequestedError())
        return
      default: throw unavailableRoute()
    }
  }

  subscribeRepairProjectionActivation(
    listener: (source: CompatibleNameRepairProjectionSource) => void,
  ): () => void {
    return this.#resources.subscribeRepairProjectionActivation(listener)
  }

  async startLifecycleAction(
    action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>,
    lifecycle: ReceiveLifecycleState,
  ): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    if (action !== 'continue' || lifecycle.kind !== 'resumable-receive') throw unavailableRoute()
    const transferJobId = this.#attemptIdentities.createTransferJobId()
    const outputSessionId = this.#attemptIdentities.createOutputSessionId()
    const session = await reopenFileSystemAccessOutput({
      intent: this.intent,
      operationRepository: this.#repository,
      ...(this.#diagnostics === undefined
        ? {}
        : { diagnostics: this.#diagnostics }),
      ...stageDiagnosticsOption(
        this.#localOutputFailures,
        this.#diagnostics?.failures?.attempt,
        transferJobId,
        outputSessionId,
      ),
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
        transferJobId,
        outputSessionId,
      )
      const resumed = await transitionLifecycle(
        this.#repository,
        this.intent,
        this.#lease.leaseId,
        { kind: 'resume-started' },
        lifecycle,
      )
      this.#resources.replaceOutputSession(session)
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
    await this.#resources.close()
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

async function createFSAAttempt(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  session: FileSystemAccessOutputSession,
  lifecycleEntry: 'start' | 'resume',
  admissionFallback: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }> | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
  transferJobId: string,
  outputSessionId: string,
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
    transferJobId,
  )
  const plans = await createFSAPlanAuthority(
    intent,
    repository,
    lease,
    session,
    lifecycleEntry,
    attemptAuthority.settlement,
    outputSessionId,
  )
  return Object.freeze({ ...attemptAuthority, plans })
}

async function createFSAAttemptSettlement(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  admissionFallback: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }> | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
  transferJobId: string,
): Promise<Readonly<{
  settlement: FileSystemAccessOperationSettlementAuthority
  transferJobId: string
}>> {
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

function stageDiagnosticsOption(
  failures: LocalOutputOperationFailureDiagnosticsPort | undefined,
  attempt: NonNullable<OutputDiagnosticsPorts['failures']>['attempt'],
  transferJobId: string,
  outputSessionId: string,
): Readonly<{
  stageDiagnostics?: ReturnType<LocalOutputOperationFailureDiagnosticsPort['forAttempt']>
}> {
  return failures === undefined || attempt === undefined
    ? Object.freeze({})
    : Object.freeze({
        stageDiagnostics: failures.forAttempt({ attempt, transferJobId, outputSessionId }),
      })
}

async function createFSAPlanAuthority(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  session: FileSystemAccessOutputSession,
  lifecycleEntry: 'start' | 'resume',
  settlement: FileSystemAccessOperationSettlementAuthority,
  outputSessionId: string,
): Promise<V2PlanExecutionAuthority> {
  const plans = await createV2PlanExecutionAuthority({
    intent,
    routes: {
      directTree: {
        open: async (boundIntent, signal) => {
          signal.throwIfAborted()
          // Binding settlement before prepareRoot ensures any ambiguous namespace
          // creation is treated as owned activation work, never as safely unopened.
          const materializationSettlement = settlement.bindMaterialization(session)
          await session.activate()
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
             namespaceClaims: session,
             repairSummary: () => session.repairSummary(),
             outputIdentity: outputSessionIdentity({
              backend: 'browser-fsa-tree',
              outputSessionId,
            }),
            settlement: materializationSettlement,
          })
        },
      },
      lifecycle: settlement,
    },
  })
  return plans
}
