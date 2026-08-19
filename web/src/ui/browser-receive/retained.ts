import { lifecycleFailureFact, type FailureFact } from '../../diagnostics/incident'
import { IndexedDbReceiveResumeSource } from '../../output/browser/indexeddb-resume-state'
import {
  createOutputFailureBinding,
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticBackend,
  type OutputDiagnosticsPorts,
  type OutputFailureBinding,
  type OutputFailureSinks,
  type OutputTraceSource,
} from '../../output/diagnostics'
import { openOriginPrivateRetainedArtifactBackend } from '../../output/origin-private/session'
import { TargetOwnershipUnknownError } from '../../output/persistent-tree/errors'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
  type ReceiveOperationResumeRef,
  type ReceiveOperationResumeSource,
} from '../../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
  ReopenedWorkspaceOperation,
} from '../../output/resume/reopen-authority'
import type { WorkspaceStageTraceListener } from '../../output/workspace/stages'
import { classificationForTransferFailure } from '../../transfer/job/failures'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import type {
  V2BoundReceiveOperation,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
} from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { FSAReceiveOperation } from './fsa'
import { WorkspaceReceiveOperation } from './workspace'
import { handoffRetainedWorkspacePackage } from './workspace-publication'

const RESUME_AUTHORITY_UNAVAILABLE =
  'Saved actions are unavailable because this browser has no persisted-operation authority.'
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
  })

type RetainedContinuationAction = Extract<V2RetainedReceiveAction, 'save' | 'redownload'>
type DirectTreeReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-tree-receive' }
>
type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' }
>
type ReceiveContinuation = DirectTreeReceiveContinuation | WorkspaceReceiveContinuation
type WorkspacePackageContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-package' }
>
type WorkspaceRetainedContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-retained' }
>

export interface BrowserRetainedContinuationExecutor {
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
  readonly onTrace?: WorkspaceStageTraceListener
}

export async function listBrowserRetainedOperations(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  signal: AbortSignal,
): Promise<V2RetainedReceiveInventory> {
  signal.throwIfAborted()
  if (typeof windowPort.indexedDB?.open !== 'function') return emptyRetainedInventory()

  const source = await (options.openResumeSource ?? (() => IndexedDbReceiveResumeSource.open()))()
  let inventoryClosed = false
  const closeSource = () => {
    if (inventoryClosed) return
    inventoryClosed = true
    source.close()
  }
  try {
    signal.throwIfAborted()
    const hasMutationAuthority = options.resumeMutations !== undefined
    const authority = new ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>({
      source,
      mutations: options.resumeMutations ?? READ_ONLY_RESUME_MUTATIONS,
      clock: { now: options.now ?? Date.now },
    })
    const inventory = await authority.listResumeState()
    const references = new WeakMap<V2RetainedReceiveOperation, ReceiveOperationResumeRef>()
    try {
      signal.throwIfAborted()
      const operations = Object.freeze(inventory.operations.map((reference) => {
        const { descriptor } = reference
        const presentation = retainedOperationAuthority(
          descriptor.continuation,
          hasMutationAuthority,
        )
        const operation: V2RetainedReceiveOperation = Object.freeze({
          operationId: descriptor.operationId,
          receiveIntentDigest: descriptor.receiveIntentDigest,
          lifecycleGeneration: descriptor.lifecycleGeneration,
          lifecycle: descriptor.lifecycle,
          continuation: descriptor.continuation,
          ...(descriptor.expiresAt === undefined ? {} : { expiresAt: descriptor.expiresAt }),
          actions: presentation.actions,
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

function retainedActions(
  ...actions: V2RetainedReceiveAction[]
): readonly V2RetainedReceiveAction[] {
  return Object.freeze(actions)
}

function retainedOperationAuthority(
  continuation: V2RetainedReceiveOperation['continuation'],
  hasMutationAuthority: boolean,
): Readonly<{
  actions: readonly V2RetainedReceiveAction[]
  unavailableReason?: string
}> {
  if (!hasMutationAuthority) {
    return Object.freeze({
      actions: retainedActions(),
      unavailableReason: RESUME_AUTHORITY_UNAVAILABLE,
    })
  }
  switch (continuation) {
    case 'resume-receive':
    case 'resume-package':
      return Object.freeze({ actions: retainedActions('continue', 'discard') })
    case 'save-artifact':
      return Object.freeze({ actions: retainedActions('save', 'discard') })
    case 'retry-download':
      return Object.freeze({ actions: retainedActions('redownload', 'delete') })
    case 'cleanup-expired':
    case 'retry-cleanup':
      return Object.freeze({ actions: retainedActions('delete') })
    case 'needs-attention':
      return Object.freeze({
        actions: retainedActions(),
        unavailableReason: 'Ownership needs attention; no automatic action is safe.',
      })
  }
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
  if (action === 'discard' || (action === 'delete' &&
      operation.continuation !== 'cleanup-expired')) {
    await authority.discard(reference, failures)
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  const result = await authority.resume(reference, failures)
  if (result.kind === 'retention-cleanup') {
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  const executor = options.continuationExecutor ??
    browserRetainedContinuationExecutor(windowPort, options.onTrace, options.outputTrace)
  const { continuation } = result
  switch (continuation.kind) {
    case 'direct-tree-receive':
    case 'workspace-receive':
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

async function continueRetainedReceive(
  executor: BrowserRetainedContinuationExecutor,
  continuation: ReceiveContinuation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<V2RetainedReceiveActionResult> {
  if (action !== 'continue') {
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
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
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

async function resumeWorkspaceReceive(
  windowPort: BrowserReceiveWindow,
  continuation: WorkspaceReceiveContinuation,
  signal: AbortSignal,
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: OutputTraceSource | undefined,
  binding: OutputFailureBinding,
): Promise<V2BoundReceiveOperation> {
  const fallback = continuation.operation.receiveAdmissionFallback
  if (fallback === undefined) {
    throw new TypeError('Workspace continuation omitted its admission fallback')
  }
  try {
    const backend = await continuation.operation.receiveContinuation.openBackend({
      ...(trace === undefined ? {} : { onTrace: trace }),
      ...diagnosticsOption('origin_private', outputTrace, binding.sinks),
    })
    try {
      signal.throwIfAborted()
      const runtime = await WorkspaceReceiveOperation.reopen({
        windowPort,
        operation: continuation.operation,
        backend,
        ...(trace === undefined ? {} : { trace }),
        ...diagnosticsOption('origin_private', outputTrace, binding.sinks),
      })
      return bindRuntimeOutputFailures(runtime, binding)
    } catch (error) {
      let cleanupFailed = false
      let cleanupFailure: unknown
      try {
        await backend.close()
      } catch (caughtCleanupFailure) {
        cleanupFailed = true
        cleanupFailure = caughtCleanupFailure
      }
      if (cleanupFailed) {
        throw new AggregateError(
          [error, cleanupFailure],
          'Workspace continuation adoption and output cleanup both failed',
          { cause: error },
        )
      }
      throw error
    }
  } catch (error) {
    try {
      await settleWorkspaceReceiveAdmissionFailure(continuation, fallback, error)
    } catch (settlementError) {
      const consequence = classificationForTransferFailure(settlementError, {
        stage: 'settlement',
        relation: 'consequence',
      })
      if (consequence === undefined) throw error
      throw new V2TransferFailureSettlementError(
        classificationForTransferFailure(error, {
          stage: 'reopen',
          relation: 'contributor',
        }),
        [consequence],
      )
    }
    throw error
  }
}

async function settleWorkspaceReceiveAdmissionFailure(
  continuation: WorkspaceReceiveContinuation,
  fallback: NonNullable<WorkspaceReceiveContinuation['operation']['receiveAdmissionFallback']>,
  error: unknown,
): Promise<void> {
  if (!(error instanceof TargetOwnershipUnknownError)) {
    await continuation.operation.stages.restoreReceiveContinuation(fallback)
    return
  }
  if (error.operationId !== null &&
      error.operationId !== continuation.operation.intent.operationId) {
    throw new TypeError('Workspace recovery evidence belongs to another operation', {
      cause: error,
    })
  }
  await continuation.operation.stages.recordTargetOwnershipUnknown(
    continuation.operation.intent.digest,
  )
}

async function continueRetainedWorkspaceOperation(
  windowPort: BrowserReceiveWindow,
  operation: ReopenedWorkspaceOperation,
  action: RetainedContinuationAction,
  signal: AbortSignal,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  signal.throwIfAborted()
  if ((action === 'save' && operation.lifecycle.kind !== 'waiting-to-save') ||
      (action === 'redownload' && (operation.lifecycle.kind !== 'download-started' ||
        operation.lifecycle.attemptKind !== 'workspace'))) {
    throw continuationMismatch()
  }
  const backend = await openOriginPrivateRetainedArtifactBackend({
    receiveIntent: operation.intent,
    operationRepository: operation.repository,
    namespace: operation.namespace,
    ...(diagnostics === undefined ? {} : { diagnostics }),
  }).catch((error: unknown) => {
    recordOutputException(diagnostics?.failures?.continuation, error)
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent('continuation', {
        backend: 'origin_private',
        transition: 'admission_failed',
      }))
    throw error
  })
  let handoffFailed = false
  let handoffFailure: unknown
  try {
    signal.throwIfAborted()
    await handoffRetainedWorkspacePackage(windowPort, operation, backend, diagnostics)
  } catch (error) {
    handoffFailed = true
    handoffFailure = error
    if (!signal.aborted) {
      recordOutputException(diagnostics?.failures?.publication, error)
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'unknown',
        }))
    }
  }

  let cleanupFailed = false
  let cleanupFailure: unknown
  try {
    await backend.close()
  } catch (error) {
    // The retained backend owns checkpoint-close classification and trace emission.
    cleanupFailed = true
    cleanupFailure = error
  }

  if (handoffFailed && cleanupFailed) {
    throw new AggregateError(
      [handoffFailure, cleanupFailure],
      'Retained handoff and output cleanup both failed',
    )
  }
  if (handoffFailed) throw handoffFailure
  if (cleanupFailed) throw cleanupFailure
}

function continuationMismatch(): DOMException {
  return new DOMException(
    'Retained continuation does not match its reopened authority',
    'InvalidStateError',
  )
}

function diagnosticsOption(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): { readonly diagnostics?: OutputDiagnosticsPorts } {
  const diagnostics = diagnosticsFor(backend, trace, failures)
  return diagnostics === undefined ? Object.freeze({}) : Object.freeze({ diagnostics })
}

function diagnosticsFor(
  backend: OutputDiagnosticBackend,
  trace: OutputTraceSource | undefined,
  failures?: OutputFailureSinks,
): OutputDiagnosticsPorts | undefined {
  if (trace === undefined && failures === undefined) return undefined
  return Object.freeze({
    backend,
    ...(failures === undefined ? {} : { failures }),
    ...(trace === undefined ? {} : { trace }),
  })
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
