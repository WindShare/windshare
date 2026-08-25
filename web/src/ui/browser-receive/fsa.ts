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
import {
  bindOutputPerformanceSummary,
  observePerformance,
  type LocalOutputOperationFailureDiagnosticsPort,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import type {
  TraceClock,
} from '../../diagnostics/trace/ports'
import { SYSTEM_TRACE_CLOCK } from '../../diagnostics/trace/ports'
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
  outputCheckpointCostBudget,
  outputExecutionProfile,
  outputSessionIdentity,
  type OutputExecutionProfileBoundedCheckpoint,
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
import {
  browserFSARecoverySpacePrompt,
  createFSAExecutionRecoveryPolicy,
  type FSARecoverySpacePrompt,
} from './fsa/recovery-policy'

interface FSAAttemptIdentitySource {
  readonly createOutputSessionId: () => string
  readonly createTransferJobId: () => string
  readonly clock?: TraceClock
}

const defaultAttemptIdentitySource: FSAAttemptIdentitySource = Object.freeze({
  createOutputSessionId: createOutputSessionID,
  createTransferJobId: createTransferJobID,
  clock: SYSTEM_TRACE_CLOCK,
})

const MEBIBYTE_BYTES = 1024n * 1024n

export const WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_FILE_PIPELINES = 15
export const WINDOWS_CHROMIUM_FSA_MAXIMUM_ACTIVE_NATIVE_WRITERS = 8
export const WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS = 3
export const WINDOWS_CHROMIUM_FSA_MAXIMUM_OUTSTANDING_WRITE_BYTES = 8n * MEBIBYTE_BYTES
export const WINDOWS_CHROMIUM_FSA_MAXIMUM_BUFFERED_BYTES = 8n * MEBIBYTE_BYTES
// Small transfers stay final-only; large transfers periodically bound restart loss
// without admitting unbounded native prefix copies or temporary disk usage.
export const FSA_DIRECT_TREE_AUTOMATIC_CHECKPOINT_TRIGGER:
  OutputExecutionProfileBoundedCheckpoint['trigger'] = Object.freeze({
  pendingBytes: 64n * MEBIBYTE_BYTES,
  pendingMilliseconds: 30_000,
})
export const FSA_DIRECT_TREE_CHECKPOINT_COST_BUDGET = outputCheckpointCostBudget({
  maximumPrefixCopyBytes: 256n * MEBIBYTE_BYTES,
  maximumCumulativeWriteAmplificationBytes: 512n * MEBIBYTE_BYTES,
  maximumPeakTemporaryBytes: 256n * MEBIBYTE_BYTES,
})
export const FSA_DIRECT_TREE_EXECUTION_PROFILE = outputExecutionProfile({
  maximumConcurrentFilePipelines: WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_FILE_PIPELINES,
  maximumOutstandingWriteBytes: WINDOWS_CHROMIUM_FSA_MAXIMUM_OUTSTANDING_WRITE_BYTES,
  maximumBufferedBytes: WINDOWS_CHROMIUM_FSA_MAXIMUM_BUFFERED_BYTES,
  automaticCheckpoint: {
    kind: 'bounded',
    trigger: FSA_DIRECT_TREE_AUTOMATIC_CHECKPOINT_TRIGGER,
    costBudget: FSA_DIRECT_TREE_CHECKPOINT_COST_BUDGET,
  },
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
  #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined
  readonly #attemptIdentities: FSAAttemptIdentitySource
  readonly #recoverySpacePrompt: FSARecoverySpacePrompt
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
    recoverySpacePrompt?: FSARecoverySpacePrompt
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
    this.#recoverySpacePrompt = input.recoverySpacePrompt ?? browserFSARecoverySpacePrompt
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
    recoverySpacePrompt?: FSARecoverySpacePrompt
  }): Promise<FSAReceiveOperation> {
    const plans = await createFSAPlanAuthority(
      input.intent,
      input.repository,
      input.lease,
      input.session,
      'start',
      input.settlement,
      input.outputSessionId,
      input.diagnostics,
      createFSAExecutionRecoveryPolicy({
        pausedFile: 'preserve',
        costBudget: FSA_DIRECT_TREE_CHECKPOINT_COST_BUDGET,
        prompt: input.recoverySpacePrompt ?? browserFSARecoverySpacePrompt,
      }),
    )
    return new FSAReceiveOperation({ ...input, plans })
  }

  static async reopen(
    operation: ReopenedDirectTreeOperation,
    diagnostics?: OutputDiagnosticsPorts,
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort,
    attemptIdentities: FSAAttemptIdentitySource = defaultAttemptIdentitySource,
    recoverySpacePrompt: FSARecoverySpacePrompt = browserFSARecoverySpacePrompt,
  ): Promise<FSAReceiveOperation> {
    if (operation.lifecycle.kind !== 'receiving') {
      throw new TypeError('Direct-tree continuation requires active receive lifecycle state')
    }
    const admissionFallback = operation.receiveAdmissionFallback
    if (admissionFallback === undefined) {
      throw new TypeError('Direct-tree continuation omitted its admission fallback')
    }
    const transferJobId = attemptIdentities.createTransferJobId()
    const outputSessionId = attemptIdentities.createOutputSessionId()
    const attemptDiagnostics = bindOutputPerformanceSummary(
      diagnostics,
      {
        receiveOperationId: operation.intent.operationId,
        transferJobId,
        outputSessionId,
      },
      attemptIdentities.clock ?? SYSTEM_TRACE_CLOCK,
    )
    let attemptAuthority: Awaited<ReturnType<typeof createFSAAttemptSettlement>> | undefined
    let session: FileSystemAccessOutputSession | undefined
    try {
      attemptAuthority = await createFSAAttemptSettlement(
        operation.intent,
        operation.repository,
        operation.lease,
        admissionFallback,
        attemptDiagnostics,
        transferJobId,
      )
      session = await reopenFileSystemAccessOutput({
        intent: operation.intent,
        operationRepository: operation.repository,
        maximumConcurrentInitialClaimInspections:
          WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS,
        ...(attemptDiagnostics === undefined ? {} : { diagnostics: attemptDiagnostics }),
        ...stageDiagnosticsOption(
          localOutputFailures,
          attemptDiagnostics?.failures?.attempt,
          transferJobId,
          outputSessionId,
        ),
      })
      observePerformance(attemptDiagnostics?.performance, summary =>
        summary.markMilestone('authority_acquired'))
      const plans = await createFSAPlanAuthority(
        operation.intent,
        operation.repository,
        operation.lease,
        session,
        'resume',
        attemptAuthority.settlement,
        outputSessionId,
        attemptDiagnostics,
        createFSAExecutionRecoveryPolicy({
          pausedFile: 'preserve',
          costBudget: FSA_DIRECT_TREE_CHECKPOINT_COST_BUDGET,
          prompt: recoverySpacePrompt,
        }),
      )
      const resources = new FSAResourceOwner({
        outputSession: session,
        closeOperationAuthority: () => operation.close(),
        ...(attemptDiagnostics === undefined ? {} : { diagnostics: attemptDiagnostics }),
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
        recoverySpacePrompt,
        ...(attemptDiagnostics === undefined ? {} : { diagnostics: attemptDiagnostics }),
        ...(localOutputFailures === undefined ? {} : { localOutputFailures }),
      })
    } catch (error) {
      observePerformance(attemptDiagnostics?.performance, summary => summary.complete())
      if (attemptAuthority === undefined) throw error
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
    if ((action !== 'continue' && action !== 'redownload') ||
        lifecycle.kind !== 'resumable-receive' ||
        lifecycle.payloadKind !== 'file-set') throw unavailableRoute()
    const recovery = createFSAExecutionRecoveryPolicy({
      pausedFile: action === 'redownload' ? 'restart-owned-file' : 'preserve',
      costBudget: FSA_DIRECT_TREE_CHECKPOINT_COST_BUDGET,
      prompt: this.#recoverySpacePrompt,
    })
    const transferJobId = this.#attemptIdentities.createTransferJobId()
    const outputSessionId = this.#attemptIdentities.createOutputSessionId()
    const attemptDiagnostics = bindOutputPerformanceSummary(
      this.#diagnostics,
      {
        receiveOperationId: this.intent.operationId,
        transferJobId,
        outputSessionId,
      },
      this.#attemptIdentities.clock ?? SYSTEM_TRACE_CLOCK,
    )
    let session: FileSystemAccessOutputSession | undefined
    try {
      session = await reopenFileSystemAccessOutput({
        intent: this.intent,
        operationRepository: this.#repository,
        maximumConcurrentInitialClaimInspections:
          WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS,
        ...(attemptDiagnostics === undefined ? {} : { diagnostics: attemptDiagnostics }),
        ...stageDiagnosticsOption(
          this.#localOutputFailures,
          attemptDiagnostics?.failures?.attempt,
          transferJobId,
          outputSessionId,
        ),
      })
      observePerformance(attemptDiagnostics?.performance, summary =>
        summary.markMilestone('authority_acquired'))
      // Acquire the attempt before leaving the stable state so setup failures retain
      // the exact checkpoint deadline and never require compensating durable writes.
      const attempt = await createFSAAttempt(
        this.intent,
        this.#repository,
        this.#lease,
        session,
        'resume',
        lifecycle,
        attemptDiagnostics,
        transferJobId,
        outputSessionId,
        recovery,
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
      this.#diagnostics = attemptDiagnostics
      return Object.freeze({
        lifecycle: resumed,
        activeControls: this.activeControls,
        resumeTransfer: true,
      })
    } catch (error) {
      observePerformance(attemptDiagnostics?.performance, summary => summary.complete())
      if (session === undefined) throw error
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
  admissionFallback: Extract<ReceiveLifecycleState, {
    kind: 'resumable-receive'
    payloadKind: 'file-set'
  }> | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
  transferJobId: string,
  outputSessionId: string,
  recovery: ReturnType<typeof createFSAExecutionRecoveryPolicy>,
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
    diagnostics,
    recovery,
  )
  return Object.freeze({ ...attemptAuthority, plans })
}

async function createFSAAttemptSettlement(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  admissionFallback: Extract<ReceiveLifecycleState, {
    kind: 'resumable-receive'
    payloadKind: 'file-set'
  }> | undefined,
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
  diagnostics: OutputDiagnosticsPorts | undefined,
  recovery: ReturnType<typeof createFSAExecutionRecoveryPolicy>,
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
            executionProfile: FSA_DIRECT_TREE_EXECUTION_PROFILE,
            recovery,
            outputIdentity: outputSessionIdentity({
              backend: 'browser-fsa-tree',
              outputSessionId,
            }),
            settlement: materializationSettlement,
            ...(diagnostics?.performance === undefined
              ? {}
              : { performance: diagnostics.performance }),
          })
        },
      },
      lifecycle: settlement,
    },
  })
  return plans
}
