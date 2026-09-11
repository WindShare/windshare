import {
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import { equalBytes } from '../../crypto/bytes'
import { canonicalReceiveLifecycleStateBytes } from '../../output/workspace/state-codec'
import { beginBrowserDeliveryLocalMutation, reconcileBrowserDeliveryLifecycle } from '../../output/browser-delivery/recovery/local-lifecycle'
import {
  createFileSystemAccessSettlementAuthority,
  type FileSystemAccessOperationSettlementAuthority,
} from '../../output/file-system-access/settlement'
import type { ReceiveAdmissionFallback } from '../../output/file-system-access/admission-fallback'
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
import { createAutomaticCheckpointAdmissionAuthority } from '../../output/persistent-tree/automatic-checkpoint-admission'
import { createPreservingWriterCapacityAuthority } from '../../output/persistent-tree/preserving-writer-capacity'
import { checkpointAuthorityObserver } from '../../output/file-system-access/session-diagnostics'
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
  outputExecutionProfile,
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
  readLifecycle,
  transitionLifecycle,
  unavailableRoute,
} from './shared'
import { FSAResourceOwner } from './fsa-resource-owner'
import { openFolderDeliveryAttempt, openFolderCleanupAttempt, readFolderRecoverySummary, type BrowserFolderDeliveryContext } from './fsa/folder-delivery'
import { observeBrowserFolderCheckpoint } from './fsa/delivery-trace'
import {
  createFSAExecutionRecoveryPolicy,
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
// Opportunistic cuts stop when prefix copying exceeds the admission budget.
// A successful pause still commits progress after automatic checkpointing stops.
export const FSA_DIRECT_TREE_EXECUTION_PROFILE = outputExecutionProfile({
  maximumConcurrentFilePipelines: WINDOWS_CHROMIUM_FSA_MAXIMUM_CONCURRENT_FILE_PIPELINES,
  maximumOutstandingWriteBytes: WINDOWS_CHROMIUM_FSA_MAXIMUM_OUTSTANDING_WRITE_BYTES,
  maximumBufferedBytes: WINDOWS_CHROMIUM_FSA_MAXIMUM_BUFFERED_BYTES,
})

export class FSAReceiveOperation implements V2BoundReceiveOperation {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['pause', 'stop'] as const)
  readonly initialWorkspaceUsage = null
  readonly repairProjection?: CompatibleNameRepairProjectionSource
  readonly outputProgress?: BrowserFolderDeliveryContext['progress']
  readonly #repository: ReceiveOperationRepository
  readonly #lease: BrowserReceiveOperationLease
  readonly #resources: FSAResourceOwner
  #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined
  readonly #attemptIdentities: FSAAttemptIdentitySource
  readonly #folderDelivery: BrowserFolderDeliveryContext | undefined
  #settlement: FileSystemAccessOperationSettlementAuthority
  #plans: V2PlanExecutionAuthority
  #closeCheckpointAuthorities: () => void | Promise<void>
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
    closeCheckpointAuthorities: () => void | Promise<void>
    transferJobId: string
    attemptIdentities: FSAAttemptIdentitySource
    diagnostics?: OutputDiagnosticsPorts
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort
    folderDelivery?: BrowserFolderDeliveryContext
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
    this.#folderDelivery = input.folderDelivery
    if (input.folderDelivery !== undefined) this.outputProgress = input.folderDelivery.progress
    this.#settlement = input.settlement
    this.#plans = input.plans
    this.#closeCheckpointAuthorities = input.closeCheckpointAuthorities
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
    folderDelivery?: BrowserFolderDeliveryContext
  }): Promise<FSAReceiveOperation> {
    const checkpointAttempt = await createFSAPlanAuthority(
      input.intent,
      input.repository,
      input.lease,
      input.session,
      'start',
      input.settlement,
      input.transferJobId,
      input.outputSessionId,
      input.diagnostics,
      createFSAExecutionRecoveryPolicy({
        pausedFile: 'preserve',
      }),
      input.folderDelivery,
    )
    return new FSAReceiveOperation({ ...input, ...checkpointAttempt })
  }

  static async reopen(
    operation: ReopenedDirectTreeOperation,
    diagnostics?: OutputDiagnosticsPorts,
    localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort,
    attemptIdentities: FSAAttemptIdentitySource = defaultAttemptIdentitySource,
    folderDelivery?: BrowserFolderDeliveryContext,
  ): Promise<FSAReceiveOperation> {
    if (operation.lifecycle.kind !== 'receiving') {
      throw new TypeError('Direct-tree continuation requires active receive lifecycle state')
    }
    const admissionFallback = operation.receiveAdmissionFallback
    if (admissionFallback === undefined) {
      throw new TypeError('Direct-tree continuation omitted its admission fallback')
    }
    const retainedFileRecovery = operation.retainedFileRecovery
    if (retainedFileRecovery === undefined) {
      throw new TypeError('Direct-tree continuation requires a retained-file recovery choice')
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
      const checkpointAttempt = await createFSAPlanAuthority(
        operation.intent,
        operation.repository,
        operation.lease,
        session,
        'resume',
        attemptAuthority.settlement,
        transferJobId,
        outputSessionId,
        attemptDiagnostics,
        createFSAExecutionRecoveryPolicy({
          pausedFile: retainedFileRecovery,
        }),
        folderDelivery,
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
        ...checkpointAttempt,
        transferJobId: attemptAuthority.transferJobId,
        attemptIdentities,
        ...(folderDelivery === undefined ? {} : { folderDelivery }),
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

  observeCheckpoint: NonNullable<import('../../transfer/job/contract').TransferJobOptions['onCheckpointObservation']> = event => {
    observeBrowserFolderCheckpoint(this.#diagnostics, event)
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
    if (action === 'save-staged-files' || action === 'cleanup-staging') {
      return this.#runLocalDelivery(action, lifecycle)
    }
    if ((action !== 'continue' && action !== 'redownload') ||
        lifecycle.kind !== 'resumable-receive' ||
        lifecycle.payloadKind !== 'file-set') throw unavailableRoute()
    const recovery = createFSAExecutionRecoveryPolicy({
      pausedFile: action === 'redownload' ? 'restart-owned-file' : 'preserve',
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
      await this.#closeCheckpointAuthorities()
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
        this.#folderDelivery,
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
      this.#closeCheckpointAuthorities = attempt.closeCheckpointAuthorities
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

  resolveWorkspaceUsage(): null {
    return null
  }

  async #runLocalDelivery(
    action: 'save-staged-files' | 'cleanup-staging',
    lifecycle: ReceiveLifecycleState,
  ): Promise<V2LifecycleMutation> {
    const context = this.#folderDelivery
    if (context === undefined || lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set' ||
        lifecycle.operationId !== this.intent.operationId || lifecycle.receiveIntentDigest !== this.intent.digest) {
      throw unavailableRoute()
    }
    await this.#closeCheckpointAuthorities()
    if (action === 'cleanup-staging') {
      const cleanup = await openFolderCleanupAttempt(context, {
        intent: this.intent, operationLease: this.#lease,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      this.#closeCheckpointAuthorities = cleanup.close
      try { await cleanup.delivery.cleanupStaging() } finally { await cleanup.close() }
      return Object.freeze({ lifecycle, workspaceUsage: null, actionOutcome: { kind: 'completed' } as const })
    }
    const localAuthority = { repository: this.#repository, lease: this.#lease }
    await beginBrowserDeliveryLocalMutation(localAuthority)
    let session: FileSystemAccessOutputSession | undefined
    let attempt: Awaited<ReturnType<typeof openFolderDeliveryAttempt>> | undefined
    const failures: unknown[] = []
    try {
      session = await reopenFileSystemAccessOutput({
        intent: this.intent, operationRepository: this.#repository,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      this.#resources.replaceOutputSession(session)
      await session.activate()
      attempt = await openFolderDeliveryAttempt(context, {
        target: session, intent: this.intent, operationLease: this.#lease,
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      this.#closeCheckpointAuthorities = attempt.close
      await attempt.delivery.saveStagedFiles()
    } catch (error) { failures.push(error) }
    for (const close of [() => attempt?.close(), () => session?.close()]) {
      try { await close() } catch (error) { failures.push(error) }
    }
    const reconciled = await reconcileBrowserDeliveryLifecycle(localAuthority).catch(error => {
      if (failures.length === 0) throw error
      throw new AggregateError([...failures, error], 'Local folder saving could not reconcile its recovery authority', { cause: failures[0] })
    })
    const retainedLifecycle = equalBytes(canonicalReceiveLifecycleStateBytes(reconciled), canonicalReceiveLifecycleStateBytes(lifecycle))
      ? lifecycle : reconciled
    let recoverySummary: Awaited<ReturnType<typeof readFolderRecoverySummary>> | undefined
    try { recoverySummary = await readFolderRecoverySummary(this.intent, retainedLifecycle) }
    catch (error) { failures.push(error) }
    const actionOutcome = localDeliveryOutcome(failures)
    // Local copying proves individual target files; remaining discovery still owns task completion.
    return Object.freeze({ lifecycle: retainedLifecycle, workspaceUsage: null, actionOutcome,
      ...(recoverySummary === undefined ? {} : { recoverySummary }) })
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
    try {
      await this.#closeCheckpointAuthorities()
    } finally {
      await this.#resources.close()
    }
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}

function localDeliveryOutcome(failures: readonly unknown[]): import('../v2-receive-runtime').V2LifecycleActionOutcome {
  if (failures.length === 0) return Object.freeze({ kind: 'completed' })
  const error = failures.length === 1 ? failures[0] : new AggregateError(failures,
    'Local folder saving could not release output authority', { cause: failures[0] })
  return Object.freeze({ kind: 'failed', error })
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
  session: Pick<FileSystemAccessOutputSession, 'close'>,
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
      'FSA output setup failed and cleanup also failed',
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
  admissionFallback: ReceiveAdmissionFallback | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
  transferJobId: string,
  outputSessionId: string,
  recovery: ReturnType<typeof createFSAExecutionRecoveryPolicy>,
  folderDelivery?: BrowserFolderDeliveryContext,
): Promise<Readonly<{
  settlement: FileSystemAccessOperationSettlementAuthority
  plans: V2PlanExecutionAuthority
  closeCheckpointAuthorities: () => void | Promise<void>
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
    transferJobId,
    outputSessionId,
    diagnostics,
    recovery,
    folderDelivery,
  )
  return Object.freeze({ ...attemptAuthority, ...plans })
}

async function createFSAAttemptSettlement(
  intent: ReceiveIntent,
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  admissionFallback: ReceiveAdmissionFallback | undefined,
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
  transferJobId: string,
  outputSessionId: string,
  diagnostics: OutputDiagnosticsPorts | undefined,
  recovery: ReturnType<typeof createFSAExecutionRecoveryPolicy>,
  folderDelivery?: BrowserFolderDeliveryContext,
): Promise<Readonly<{
  plans: V2PlanExecutionAuthority
  closeCheckpointAuthorities: () => void | Promise<void>
}>> {
  const checkpointIdentity = Object.freeze({
    receiveOperationId: intent.operationId,
    transferJobId,
    outputSessionId,
  })
  const observeCheckpointAuthority = checkpointAuthorityObserver(diagnostics)
  const automaticCheckpointAdmission = createAutomaticCheckpointAdmissionAuthority({
    identity: checkpointIdentity,
    ...(observeCheckpointAuthority === undefined ? {} : { observe: observeCheckpointAuthority }),
  })
  const preservingWriterCapacity = createPreservingWriterCapacityAuthority({
    identity: checkpointIdentity,
    ...(observeCheckpointAuthority === undefined ? {} : { observe: observeCheckpointAuthority }),
  })
  let deliveryAttempt: Awaited<ReturnType<typeof openFolderDeliveryAttempt>> | undefined
  const closeCheckpointAuthorities = async () => {
    automaticCheckpointAdmission.close('terminal-drain')
    preservingWriterCapacity.close('terminal-drain')
    await deliveryAttempt?.close()
  }
  try {
    deliveryAttempt = folderDelivery === undefined ? undefined : await openFolderDeliveryAttempt(folderDelivery, {
      target: session, intent, operationLease: lease,
      ...(diagnostics === undefined ? {} : { diagnostics }),
    })
  } catch (error) {
    return closeFSAContinuationAfterFailure({ close: closeCheckpointAuthorities }, error)
  }
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
            materialization: deliveryAttempt?.delivery ?? session,
            namespaceClaims: session,
            repairSummary: () => session.repairSummary(),
            executionProfile: FSA_DIRECT_TREE_EXECUTION_PROFILE,
            recovery,
            automaticCheckpointAdmission,
            preservingWriterCapacity,
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
  }).catch(error => closeFSAContinuationAfterFailure({ close: closeCheckpointAuthorities }, error))
  return Object.freeze({
    plans,
    closeCheckpointAuthorities,
  })
}
