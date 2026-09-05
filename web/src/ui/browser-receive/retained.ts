import { lifecycleFailureFact, type FailureFact } from '../../diagnostics/incident'
import { IndexedDbReceiveResumeSource } from '../../output/browser/indexeddb-resume-state'
import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import { IndexedDbCompatibleNameLedger } from '../../output/browser/indexeddb-compatible-name-ledger'
import type { CompatibleNameLedger } from '../../output/file-system-access/compatible-name/ledger'
import type { CompatibleNameRepairSummary } from '../../output/file-system-access/compatible-name/model'
import {
  createOutputFailureBinding,
  recordOutputException,
  type LocalOutputOperationFailureDiagnosticsPort,
  type OutputFailureBinding,
  type OutputFailureSinks,
  type OutputTraceSource,
} from '../../output/diagnostics'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
  type ReceiveOperationResumeRef,
  type ReceiveOperationResumeSource,
} from '../../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../output/resume/reopen-authority'
import type { WorkspaceStageTraceListener } from '../../output/workspace/stages'
import { recoverWorkspaceActivationCandidates } from '../../output/workspace/activation-recovery'
import type { WorkspaceActivationJournalRepository } from '../../output/workspace/repository'
import { catchUpFileSystemAccessCompatibleNames } from '../../output/file-system-access/settlement'
import type {
  V2BoundReceiveOperation,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { FSAReceiveOperation } from './fsa'
import { unavailableRoute } from './shared'
import type { BrowserDirectZipCompositionPort } from './direct-zip'
import { diagnosticsFor } from './retained-diagnostics'
import { retainedOperationAuthority } from './retained-operation-authority'
import { observeBrowserReceiveOperationActivity } from '../../output/browser/session-lease'
import {
  continueRetainedWorkspaceOperation,
  continuationMismatch,
  resumeWorkspaceReceive,
} from './retained-workspace'


const READ_ONLY_RESUME_MUTATIONS:
  ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
  Object.freeze({
    resume: () => Promise.reject(new DOMException(
      'Persisted receive reopen authority is not installed',
      'NotSupportedError',
    )),
    expire: () => Promise.reject(new DOMException(
      'Persisted receive expiry authority is not installed',
      'NotSupportedError',
    )),
    discard: () => Promise.reject(new DOMException(
      'Persisted receive cleanup authority is not installed',
      'NotSupportedError',
    )),
    catchUp: () => Promise.reject(new DOMException(
      'Persisted terminal catch-up authority is not installed',
      'NotSupportedError',
    )),
  })

type RetainedContinuationAction = Extract<V2RetainedReceiveAction, 'save' | 'redownload'>
type DirectTreeCatchUpContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-tree-catch-up' }
>
type DirectTreeReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-tree-receive' }
>
type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' }
>
type DirectZipReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-zip' }
>
type DirectZipRetainedCleanupContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-zip-retained-cleanup' }
>
type ReceiveContinuation =
  | DirectTreeReceiveContinuation
  | WorkspaceReceiveContinuation
  | DirectZipReceiveContinuation
type WorkspacePackageContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-package' }
>
type WorkspaceRetainedContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-retained' }
>
type RetainedAuthorityDispatch =
  | Readonly<{ kind: 'completed' }>
  | Readonly<{
      kind: 'continuation'
      continuation: AuthorityOwnedReceiveOperationContinuation
      directZipAction: boolean
    }>

export interface BrowserRetainedContinuationExecutor {
  catchUpCompatibleNames?(
    continuation: DirectTreeCatchUpContinuation,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<void>
  resumeReceive(
    continuation: ReceiveContinuation,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<V2BoundReceiveOperation>
  resumePackage(
    continuation: WorkspacePackageContinuation,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<void>
  continueRetained(
    continuation: WorkspaceRetainedContinuation,
    action: RetainedContinuationAction,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<void>
}

export interface BrowserRetainedCompositionOptions {
  readonly openResumeSource?: () => Promise<ReceiveOperationResumeSource & { close(): void }>
  readonly resumeMutations?:
    ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult>
  readonly continuationExecutor?: BrowserRetainedContinuationExecutor
  readonly now?: () => number
  readonly outputTrace?: OutputTraceSource
  readonly localOutputFailures?: LocalOutputOperationFailureDiagnosticsPort
  readonly onTrace?: WorkspaceStageTraceListener
  readonly openActivationRepository?: () => Promise<WorkspaceActivationJournalRepository>
  readonly recoverWorkspaceActivations?: typeof recoverWorkspaceActivationCandidates
  readonly openCompatibleNameLedger?: () => Promise<Pick<
    CompatibleNameLedger,
    'readRepairSummary' | 'close'
  >>
  readonly directZip?: BrowserDirectZipCompositionPort
}

export async function readBrowserCompatibleNameRepairSummary(
  options: BrowserRetainedCompositionOptions,
  operationId: string,
  signal: AbortSignal,
): Promise<CompatibleNameRepairSummary | undefined> {
  signal.throwIfAborted()
  const ledger = await (options.openCompatibleNameLedger ??
    (() => IndexedDbCompatibleNameLedger.open()))()
  try {
    const summary = await ledger.readRepairSummary(operationId)
    signal.throwIfAborted()
    return summary
  } finally {
    ledger.close()
  }
}

export async function listBrowserRetainedOperations(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  signal: AbortSignal,
): Promise<V2RetainedReceiveInventory> {
  signal.throwIfAborted()
  if (typeof windowPort.indexedDB?.open !== 'function') return emptyRetainedInventory()

  if (options.openResumeSource === undefined || options.openActivationRepository !== undefined ||
      options.recoverWorkspaceActivations !== undefined) {
    const activationRepository = await (
      options.openActivationRepository ?? (() => IndexedDbReceiveOperationRepository.open())
    )()
    try {
      await (options.recoverWorkspaceActivations ?? recoverWorkspaceActivationCandidates)({
        repository: activationRepository,
        storage: windowPort.navigator.storage as StorageManager & {
          getDirectory(): Promise<FileSystemDirectoryHandle>
        },
        locks: windowPort.navigator.locks,
      })
    } finally {
      activationRepository.close()
    }
  }
  signal.throwIfAborted()

  const source = await (options.openResumeSource ?? (() => IndexedDbReceiveResumeSource.open()))()
  let inventoryClosed = false
  const closeSource = () => {
    if (inventoryClosed) return
    inventoryClosed = true
    source.close()
  }
  try {
    signal.throwIfAborted()
    await dispatchDirectZipBootstrapCandidates(source, options.directZip, signal)
    const hasMutationAuthority = options.resumeMutations !== undefined
    const authority = new ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>({
      source: sourceWithoutBootstrapCandidates(source),
      mutations: options.resumeMutations ?? READ_ONLY_RESUME_MUTATIONS,
      clock: { now: options.now ?? Date.now },
    })
    const inventory = await authority.listResumeState()
    const locks = windowPort.navigator?.locks
    const activities = locks === undefined
      ? inventory.operations.map(() => 'inactive' as const)
      : await Promise.all(inventory.operations.map(reference => observeBrowserReceiveOperationActivity(
          reference.descriptor.operationId,
          locks,
        )))
    const references = new WeakMap<V2RetainedReceiveOperation, ReceiveOperationResumeRef>()
    try {
      signal.throwIfAborted()
      const operations = Object.freeze(inventory.operations.filter((_, index) =>
        activities[index] === 'inactive').map((reference) => {
        const { descriptor } = reference
        const presentation = retainedOperationAuthority(
          descriptor.continuation,
          hasMutationAuthority,
          options.directZip !== undefined,
          reference.recoverySummary !== undefined,
        )
        const operation: V2RetainedReceiveOperation = Object.freeze({
          operationId: descriptor.operationId,
          receiveIntentDigest: descriptor.receiveIntentDigest,
          lifecycleGeneration: descriptor.lifecycleGeneration,
          lifecycle: descriptor.lifecycle,
          continuation: descriptor.continuation,
          ...(descriptor.expiresAt === undefined ? {} : { expiresAt: descriptor.expiresAt }),
          actions: presentation.actions,
          ...(reference.recoverySummary === undefined
            ? {}
            : { recoverySummary: reference.recoverySummary }),
          ...(presentation.unavailableReason === undefined
            ? {}
            : { unavailableReason: presentation.unavailableReason }),
        })
        references.set(operation, reference)
        return operation
      }))
      const presentationFailures = retainedInventoryPresentationFailures(operations)
      let open = true
      return Object.freeze({
        operations,
        presentationFailures,
        act: async (
          operation: V2RetainedReceiveOperation,
          action: V2RetainedReceiveAction,
          actionSignal: AbortSignal,
          failures?: OutputFailureSinks,
        ) => {
          actionSignal.throwIfAborted()
          if (!open) throw new DOMException('Retained inventory is closed', 'InvalidStateError')
          const reference = references.get(operation)
          if (reference === undefined || !operation.actions.includes(action)) {
            throw new DOMException('Retained action escaped its inventory authority', 'InvalidStateError')
          }
          references.delete(operation)
          return performRetainedAction(
            windowPort,
            options,
            authority,
            reference,
            operation,
            action,
            actionSignal,
            failures,
          )
        },
        close: () => {
          if (!open) return
          open = false
          inventory.close()
          closeSource()
        },
      })
    } catch (error) {
      inventory.close()
      throw error
    }
  } catch (error) {
    closeSource()
    throw error
  }
}

function emptyRetainedInventory(): V2RetainedReceiveInventory {
  return Object.freeze({
    operations: Object.freeze([]),
    presentationFailures: Object.freeze([]),
    act: () => Promise.reject(new DOMException(
      'Retained operation authority is unavailable',
      'InvalidStateError',
    )),
    close: () => undefined,
  })
}

function retainedInventoryPresentationFailures(
  operations: readonly V2RetainedReceiveOperation[],
): readonly FailureFact[] {
  return Object.freeze(operations.flatMap((operation) =>
    operation.lifecycle.kind === 'needs-attention'
      ? [lifecycleFailureFact({
          stage: 'retained_inventory',
          recoveryDisposition: 'needs_attention',
          kind: operation.lifecycle.kind,
          reason: operation.lifecycle.reason,
        })]
      : []))
}

async function performRetainedAction(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  authority: ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>,
  reference: ReceiveOperationResumeRef,
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<V2RetainedReceiveActionResult> {
  const dispatch = await dispatchRetainedAuthorityAction(
    authority,
    reference,
    operation,
    action,
    signal,
    failures,
  )
  if (dispatch.kind === 'completed') return dispatch
  const { continuation, directZipAction } = dispatch
  const executor = options.continuationExecutor ??
    browserRetainedContinuationExecutor(
      windowPort,
      options.onTrace,
      options.outputTrace,
      options.localOutputFailures,
      options.directZip,
    )
  if (continuation.kind === 'direct-zip-retained-cleanup') {
    return deleteRetainedDirectZip(options.directZip, continuation, signal, failures)
  }
  if (directZipAction && action === 'delete') {
    if (continuation.kind !== 'direct-zip') {
      return withFailedRetainedClose(continuation.operation, continuationMismatch())
    }
    return deleteRetainedDirectZip(options.directZip, continuation, signal, failures)
  }
  switch (continuation.kind) {
    case 'direct-tree-catch-up':
      return withRetainedOperationClose(continuation.operation, async () => {
        if (action !== 'catch-up') throw continuationMismatch()
        const catchUp = executor.catchUpCompatibleNames
        if (catchUp === undefined) throw unavailableRoute()
        signal.throwIfAborted()
        await (failures === undefined
          ? catchUp(continuation, signal)
          : catchUp(continuation, signal, failures))
        return Object.freeze({ kind: 'completed' as const })
      })
    case 'direct-tree-receive':
    case 'workspace-receive':
    case 'direct-zip':
      return continueRetainedReceive(executor, continuation, action, signal, failures)
    case 'workspace-package':
      return withRetainedOperationClose(continuation.operation, async () => {
        if (action !== 'continue') throw continuationMismatch()
        signal.throwIfAborted()
        await (failures === undefined
          ? executor.resumePackage(continuation, signal)
          : executor.resumePackage(continuation, signal, failures))
        signal.throwIfAborted()
        return Object.freeze({ kind: 'completed' as const })
      })
    case 'workspace-retained':
      return withRetainedOperationClose(continuation.operation, async () => {
        if (action !== 'save' && action !== 'redownload') throw continuationMismatch()
        signal.throwIfAborted()
        await (failures === undefined
          ? executor.continueRetained(continuation, action, signal)
          : executor.continueRetained(continuation, action, signal, failures))
        signal.throwIfAborted()
        return Object.freeze({ kind: 'completed' as const })
      })
  }
}

async function dispatchRetainedAuthorityAction(
  authority: ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>,
  reference: ReceiveOperationResumeRef,
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<RetainedAuthorityDispatch> {
  const directZipAction = isDirectZipContinuation(operation.continuation)
  if (!directZipAction && (action === 'discard' || (action === 'delete' &&
      operation.continuation !== 'cleanup-expired' &&
      operation.continuation !== 'retry-cleanup'))) {
    await authority.discard(reference, failures)
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  let result: AuthorityOwnedReceiveOperationMutationResult
  if (action === 'delete' &&
      (operation.continuation === 'cleanup-expired' ||
       operation.continuation === 'retry-cleanup')) {
    result = await authority.cleanup(reference, failures)
  } else if (action === 'catch-up') {
    result = await authority.catchUp(reference, failures)
  } else {
    const retainedFileRecovery = retainedFileRecoveryFor(operation, action)
    result = await authority.resume(reference, {
      ...(retainedFileRecovery === undefined ? {} : { retainedFileRecovery }),
      ...(failures === undefined ? {} : { failures }),
    })
  }
  if (result.kind === 'retention-cleanup') {
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  return Object.freeze({ kind: 'continuation', continuation: result.continuation, directZipAction })
}

function retainedFileRecoveryFor(
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
): 'preserve' | 'restart-owned-file' | undefined {
  if (operation.recoverySummary === undefined) return undefined
  return action === 'redownload' ? 'restart-owned-file' : 'preserve'
}

function deleteRetainedDirectZip(
  directZip: BrowserDirectZipCompositionPort | undefined,
  continuation: DirectZipReceiveContinuation | DirectZipRetainedCleanupContinuation,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<V2RetainedReceiveActionResult> {
  return withRetainedOperationClose(continuation.operation, async () => {
    if (directZip === undefined) throw unavailableRoute()
    signal.throwIfAborted()
    await (failures === undefined
      ? directZip.runtime.deleteRetained(continuation.operation, signal)
      : directZip.runtime.deleteRetained(continuation.operation, signal, failures))
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' as const })
  })
}

async function continueRetainedReceive(
  executor: BrowserRetainedContinuationExecutor,
  continuation: ReceiveContinuation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<V2RetainedReceiveActionResult> {
  const directTreeRecovery = continuation.kind === 'direct-tree-receive' &&
    (action === 'continue' || action === 'redownload')
  if (action !== 'continue' && !directTreeRecovery) {
    return withRetainedOperationClose(continuation.operation, async () => {
      throw continuationMismatch()
    })
  }
  // The resumed runtime becomes the sole live owner. Its detach path closes the
  // output-owned operation after transfer and lifecycle controls are finished.
  let runtime: V2BoundReceiveOperation
  try {
    runtime = await (failures === undefined
      ? executor.resumeReceive(continuation, signal)
      : executor.resumeReceive(continuation, signal, failures))
  } catch (error) {
    return withFailedRetainedClose(continuation.operation, error)
  }
  try {
    signal.throwIfAborted()
    return Object.freeze({ kind: 'receive-continuation', runtime })
  } catch (error) {
    return detachRuntimeAfterFailure(runtime, error)
  }
}

async function withFailedRetainedClose(
  operation: { close(): Promise<void> },
  error: unknown,
): Promise<never> {
  return withRetainedOperationClose(operation, async () => {
    throw error
  })
}

async function detachRuntimeAfterFailure(
  runtime: V2BoundReceiveOperation,
  error: unknown,
): Promise<never> {
  let cleanupFailure: unknown
  try {
    await Promise.resolve(runtime.detach())
  } catch (caughtCleanupFailure) {
    cleanupFailure = caughtCleanupFailure
  }
  if (cleanupFailure !== undefined) {
    throw new AggregateError(
      [error, cleanupFailure],
      'Receive continuation adoption failed and runtime cleanup also failed',
      { cause: error },
    )
  }
  throw error
}

async function withRetainedOperationClose<Result>(
  operation: { close(): Promise<void> },
  execute: () => Promise<Result>,
): Promise<Result> {
  let failed = false
  let failure: unknown
  let result: Result | undefined
  try {
    result = await execute()
  } catch (error) {
    failed = true
    failure = error
  }
  let cleanupFailed = false
  let cleanupFailure: unknown
  try {
    await operation.close()
  } catch (error) {
    cleanupFailed = true
    cleanupFailure = error
  }
  if (failed && cleanupFailed) {
    throw new AggregateError(
      [failure, cleanupFailure],
      'Retained operation and output cleanup both failed',
      { cause: failure },
    )
  }
  if (failed) throw failure
  if (cleanupFailed) throw cleanupFailure
  return result!
}

function browserRetainedContinuationExecutor(
  windowPort: BrowserReceiveWindow,
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: OutputTraceSource | undefined,
  localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined,
  directZip?: BrowserDirectZipCompositionPort,
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
    catchUpCompatibleNames: async (
      continuation: DirectTreeCatchUpContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => {
      try {
        await catchUpFileSystemAccessCompatibleNames({
          operation: continuation.operation,
          signal,
        })
      } catch (error) {
        if (!signal.aborted) recordOutputException(failures?.settlement, error)
        throw error
      }
    },
    resumeReceive: async (
      continuation: ReceiveContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ): Promise<V2BoundReceiveOperation> => {
      signal.throwIfAborted()
      const binding = createOutputFailureBinding(failures)
      switch (continuation.kind) {
        case 'direct-tree-receive':
          return bindRuntimeOutputFailures(
            await FSAReceiveOperation.reopen(
              continuation.operation,
              diagnosticsFor('file_system_access', outputTrace, binding.sinks),
              localOutputFailures,
            ),
            binding,
          )
        case 'workspace-receive':
          return resumeWorkspaceReceive(
            windowPort,
            continuation,
            signal,
            trace,
            outputTrace,
            binding,
          )
        case 'direct-zip': {
          if (directZip === undefined) throw unavailableRoute()
          const runtime = await (failures === undefined
            ? directZip.runtime.resume(continuation.operation, signal)
            : directZip.runtime.resume(continuation.operation, signal, failures))
          return bindRuntimeOutputFailures(runtime, binding)
        }
      }
    },
    resumePackage: async (
      continuation: WorkspacePackageContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => {
      try {
        await continuation.operation.packageContinuation.execute(signal)
      } catch (error) {
        if (!signal.aborted) recordOutputException(failures?.continuation, error)
        throw error
      }
    },
    continueRetained: (
      continuation: WorkspaceRetainedContinuation,
      action: RetainedContinuationAction,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => continueRetainedWorkspaceOperation(
      windowPort,
      continuation.operation,
      action,
      signal,
      diagnosticsFor('origin_private', outputTrace, failures),
    ),
  })
}

async function dispatchDirectZipBootstrapCandidates(
  source: ReceiveOperationResumeSource,
  directZip: BrowserDirectZipCompositionPort | undefined,
  signal: AbortSignal,
): Promise<void> {
  const candidates = source.listDirectZipBootstrapCandidates === undefined
    ? []
    : await source.listDirectZipBootstrapCandidates()
  if (candidates.length !== 0 && directZip === undefined) {
    throw new DOMException(
      'Retained Direct ZIP bootstrap effects require their exact recovery authority',
      'NotSupportedError',
    )
  }
  for (const candidate of candidates) {
    signal.throwIfAborted()
    await directZip!.runtime.dispatchBootstrapCandidate(candidate, signal)
  }
}

function sourceWithoutBootstrapCandidates(
  source: ReceiveOperationResumeSource,
): ReceiveOperationResumeSource {
  return Object.freeze({
    listDirectZipBootstrapCandidates: () => Promise.resolve(Object.freeze([])),
    listLifecycleStates: () => source.listLifecycleStates(),
    ...(source.isCleanupOnly === undefined ? {} : {
      isCleanupOnly: (operationId: string) => source.isCleanupOnly!(operationId),
    }),
    ...(source.readRecoverySummary === undefined
      ? {}
      : { readRecoverySummary: (lifecycle: Parameters<NonNullable<
          ReceiveOperationResumeSource['readRecoverySummary']
        >>[0]) => source.readRecoverySummary!(lifecycle) }),
  })
}

function isDirectZipContinuation(
  continuation: V2RetainedReceiveOperation['continuation'],
): boolean {
  return continuation === 'resume-direct-zip' || continuation === 'reauthorize-direct-zip' ||
    continuation === 'verify-direct-zip-target' || continuation === 'retry-direct-zip-space'
}



export function bindRuntimeOutputFailures(
  runtime: V2BoundReceiveOperation,
  binding: OutputFailureBinding,
): V2BoundReceiveOperation {
  const bound: V2BoundReceiveOperation = {
    intent: runtime.intent,
    get plans() {
      return runtime.plans
    },
    get transferJobId() {
      return runtime.transferJobId
    },
    lifecycle: runtime.lifecycle,
    activeControls: runtime.activeControls,
    ...(runtime.outputProgress === undefined ? {} : { outputProgress: runtime.outputProgress }),
    ...(runtime.repairProjection === undefined
      ? {}
      : { repairProjection: runtime.repairProjection }),
    ...(runtime.subscribeRepairProjectionActivation === undefined
      ? {}
      : {
          subscribeRepairProjectionActivation: (
            listener: Parameters<NonNullable<
              V2BoundReceiveOperation['subscribeRepairProjectionActivation']
            >>[0],
          ) => runtime.subscribeRepairProjectionActivation!(listener),
        }),
    ...(runtime.initialWorkspaceUsage === undefined
      ? {}
      : { initialWorkspaceUsage: runtime.initialWorkspaceUsage }),
    bindOutputFailures: failures => binding.bind(failures),
    interrupt: (control, transfer) => runtime.interrupt(control, transfer),
    startLifecycleAction: (action, lifecycle) =>
      runtime.startLifecycleAction(action, lifecycle),
    observeExpiry: lifecycle => runtime.observeExpiry(lifecycle),
    resolveWorkspaceUsage: lifecycle => runtime.resolveWorkspaceUsage(lifecycle),
    settleTransferAdmissionFailure: reason => runtime.settleTransferAdmissionFailure(reason),
    detach: () => runtime.detach(),
  }
  return Object.freeze(bound)
}
