import { readRetainedBrowserDeliveries, retainedBrowserDeliveryActions, type BrowserDeliveryRepositoryFactory } from '../fsa/retained-delivery'
import { hasBrowserDeliveryStaging } from '../../../output/model/browser-delivery'
import type { runRetainedBrowserFolderAction } from '../fsa/local-delivery'
import { canForgetReceiveOperationHistory } from '../../../output/resume/operation-history'
import { artifactRequestedName } from '../../../output/model/artifact-name'
import { preparePartialExport, supportsPartialExport } from '../../../output/capability/partial-export'
import { dispatchRetainedAuthorityAction } from './action-dispatch'
import { lifecycleFailureFact, type FailureFact } from '../../../diagnostics/incident'
import { IndexedDbReceiveResumeSource } from '../../../output/browser/indexeddb-resume-state'
import { IndexedDbReceiveOperationRepository } from '../../../output/browser/indexeddb-repository'
import {
  createOutputFailureBinding,
  type LocalOutputOperationFailureDiagnosticsPort,
  type OutputFailureSinks,
  type OutputTraceSource,
} from '../../../output/diagnostics'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
  type ReceiveOperationResumeRef,
  type ReceiveOperationResumeSource,
} from '../../../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../../output/resume/reopen-authority'
import type { WorkspaceStageTraceListener } from '../../../output/workspace/stages'
import type { recoverWorkspaceActivationCandidates } from '../../../output/workspace/activation-recovery'
import type { WorkspaceActivationJournalRepository } from '../../../output/workspace/repository'
import type {
  BoundReceiveOperation,
  RetainedReceiveAction,
  RetainedReceiveActionResult,
  RetainedReceiveInventory,
  RetainedReceiveOperation,
} from '../../receive/contracts'
import {
  isBrowserStoragePolicyDenial,
  readBrowserIndexedDb,
  readBrowserLocks,
  type BrowserReceiveWindow,
} from '../../../output/browser/environment'
import { bindRuntimeOutputFailures, diagnosticsFor } from './diagnostics'
import {
  detachRuntimeAfterFailure,
  withFailedRetainedClose,
  withRetainedOperationClose,
} from './operation-lifetime'
import { retainedOperationAuthority } from './operation-authority'
import { observeBrowserReceiveOperationActivity } from '../../../output/browser/session-lease'
import { continuationMismatch } from './operation-lifetime'


const READ_ONLY_RESUME_MUTATIONS:
  ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
  Object.freeze({
    resume: () => Promise.reject(new DOMException(
      'Persisted receive reopen authority is not installed',
      'NotSupportedError',
    )),
    cleanup: () => Promise.reject(new DOMException(
      'Persisted receive expiry authority is not installed',
      'NotSupportedError',
    )),
    discard: () => Promise.reject(new DOMException(
      'Persisted receive cleanup authority is not installed',
      'NotSupportedError',
    )),
  })

type RetainedContinuationAction = Extract<RetainedReceiveAction, 'save' | 'redownload'>
type DirectTreeReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'direct-tree-receive' }
>
type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' | 'workspace-start' }
>
type ReceiveContinuation =
  | DirectTreeReceiveContinuation
  | WorkspaceReceiveContinuation
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
  ): Promise<BoundReceiveOperation>
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
  readonly prepareActivation?: (operation: RetainedReceiveOperation, signal: AbortSignal) => Promise<import('./activation').RetainedActivation | undefined>
  readonly openBrowserDeliveryRepository?: BrowserDeliveryRepositoryFactory
  readonly runBrowserFolderLocalAction?: typeof runRetainedBrowserFolderAction
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
}

export async function listBrowserRetainedOperations(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  signal: AbortSignal,
): Promise<RetainedReceiveInventory> {
  signal.throwIfAborted()
  if (typeof readBrowserIndexedDb(windowPort)?.open !== 'function') return emptyRetainedInventory()

  if (!await repairWorkspaceInventory(windowPort, options, signal)) return emptyRetainedInventory()

  let source: ReceiveOperationResumeSource & { close(): void }
  try {
    source = await (options.openResumeSource ?? (() => IndexedDbReceiveResumeSource.open()))()
  } catch (error) {
    signal.throwIfAborted()
    if (isBrowserStoragePolicyDenial(error)) return emptyRetainedInventory()
    throw error
  }
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
    })
    const inventory = await authority.listResumeState()
    try {
      const browserDeliveries = options.openResumeSource === undefined || options.openBrowserDeliveryRepository !== undefined
        ? await readRetainedBrowserDeliveries(inventory.operations.map(reference => reference.descriptor),
            signal, options.openBrowserDeliveryRepository)
        : new Map<string, import('../../../output/model/browser-delivery').BrowserDeliverySummary>()
      const locks = readBrowserLocks(windowPort)
      const activities = locks === undefined
        ? inventory.operations.map(() => 'inactive' as const)
        : await Promise.all(inventory.operations.map(reference => observeBrowserReceiveOperationActivity(
            reference.descriptor.operationId,
            locks,
          )))
      const references = new WeakMap<RetainedReceiveOperation, ReceiveOperationResumeRef>()
      signal.throwIfAborted()
      const operations = Object.freeze(inventory.operations.filter((_, index) =>
        activities[index] === 'inactive').map((reference) => {
        const { descriptor } = reference
        const browserDelivery = browserDeliveries.get(descriptor.operationId) ?? descriptor.browserDelivery
        const presentation = retainedOperationAuthority(
          descriptor.continuation,
          hasMutationAuthority,
          reference.recoverySummary !== undefined,
        )
        const routeActions = options.resumeMutations?.forget !== undefined &&
          canForgetReceiveOperationHistory(descriptor.lifecycle)
          ? Object.freeze([...presentation.actions, 'forget' as const]) : presentation.actions
        const availableActions = retainedBrowserDeliveryActions(descriptor.lifecycle, browserDelivery, routeActions)
        const unavailableReason = descriptor.recoveryUnavailable === 'native-checkpoint-unavailable'
          ? 'zipUnavailable'
          : presentation.unavailableReason
        const operation: RetainedReceiveOperation = Object.freeze({
          operationId: descriptor.operationId,
          ...(descriptor.display === undefined ? {} : { display: descriptor.display }),
          ...(descriptor.shareInstance === undefined ? {} : { shareInstance: descriptor.shareInstance }),
          ...(descriptor.selectionDigest === undefined ? {} : { selectionDigest: descriptor.selectionDigest }),
          receiveIntentDigest: descriptor.receiveIntentDigest,
          lifecycleGeneration: descriptor.lifecycleGeneration,
          lifecycle: descriptor.lifecycle,
          continuation: descriptor.continuation,
          ...(browserDelivery === undefined ? {} : { browserDelivery }),
          actions: hasMutationAuthority && descriptor.recoveryUnavailable === undefined && supportsPartialExport(windowPort) &&
            descriptor.lifecycle.kind === 'resumable-receive' &&
            descriptor.lifecycle.payloadKind === 'workspace-archive' &&
            descriptor.lifecycle.completedFileCount > 0n
            ? Object.freeze([...availableActions, 'save-partial' as const])
            : availableActions,
          ...(descriptor.sourceRevisionFailures === undefined ? {} : { sourceRevisionFailures: descriptor.sourceRevisionFailures }),
          ...(reference.recoverySummary === undefined
            ? {}
            : { recoverySummary: reference.recoverySummary }),
          ...(unavailableReason === undefined ? {} : { unavailableReason }),
        })
        references.set(operation, reference)
        return operation
      }))
      const activations = new Map<RetainedReceiveOperation, import('./activation').RetainedActivation>()
      if (options.prepareActivation !== undefined) {
        for (const operation of operations) {
          const activation = await options.prepareActivation(operation, signal)
          signal.throwIfAborted()
          if (activation !== undefined) activations.set(operation, activation)
        }
      }
      const presentationFailures = retainedInventoryPresentationFailures(operations)
      let open = true
      return Object.freeze({
        operations,
        presentationFailures,
        act: async (
          operation: RetainedReceiveOperation,
          action: RetainedReceiveAction,
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
          const activate = activations.get(operation)
          if (activate !== undefined) await activate(action)
          actionSignal.throwIfAborted()
          const result = await performRetainedAction(
            windowPort,
            options,
            authority,
            reference,
            operation,
            action,
            actionSignal,
            failures,
          )
          return result.kind === 'receive-continuation' && operation.display !== undefined
            ? Object.freeze({ ...result, runtime: bindRuntimeOutputFailures(result.runtime, undefined, operation.display) })
            : result
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

function emptyRetainedInventory(): RetainedReceiveInventory {
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
  operations: readonly RetainedReceiveOperation[],
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
  operation: RetainedReceiveOperation,
  action: RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<RetainedReceiveActionResult> {
  if (isBrowserFolderStorageAction(operation, action)) {
    const run = options.runBrowserFolderLocalAction ?? (await import('../fsa/local-delivery')).runRetainedBrowserFolderAction
    signal.throwIfAborted()
    await run(
      windowPort, operation, browserFolderStorageAction(action), signal,
      diagnosticsFor('file_system_access', options.outputTrace, failures),
    )
    if (action !== 'discard' && action !== 'delete') return Object.freeze({ kind: 'completed' })
  }
  return performOrdinaryRetainedAction(windowPort, options, authority, reference, operation, action, signal, failures)
}

async function performOrdinaryRetainedAction(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  authority: ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>,
  reference: ReceiveOperationResumeRef,
  operation: RetainedReceiveOperation,
  action: RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<RetainedReceiveActionResult> {
  const partialDestination = action === 'save-partial'
    ? await preparePartialExport(windowPort)
    : undefined
  const dispatch = await dispatchRetainedAuthorityAction(
    authority,
    reference,
    operation,
    action,
    signal,
    failures,
  )
  if (dispatch.kind === 'completed') return dispatch
  const { continuation } = dispatch
  const executor = options.continuationExecutor ??
    (await import('./continuation')).browserRetainedContinuationExecutor(
      windowPort,
      options.onTrace,
      options.outputTrace,
      options.localOutputFailures,
    )
  switch (continuation.kind) {
    case 'workspace-progressive-zip-partial':
      if (action === 'save-partial' && partialDestination !== undefined) {
        return withRetainedOperationClose(continuation.operation, async () => {
          const { saveProgressivePartial } = await import('../../../output/portable/partial-export')
          signal.throwIfAborted()
          const destination = partialDestination.kind === 'browser-download'
            ? Object.freeze({
                ...partialDestination,
                suggestedName: artifactRequestedName(continuation.operation.intent.artifact),
              })
            : partialDestination
          await saveProgressivePartial(
            continuation.operation.partialContinuation,
            destination,
            signal,
            diagnosticsFor('origin_private', options.outputTrace, failures),
          )
          return Object.freeze({ kind: 'completed' as const })
        })
      }
      return withFailedRetainedClose(continuation.operation, continuationMismatch())
    case 'workspace-progressive-zip': {
      if (action !== 'continue') return withFailedRetainedClose(continuation.operation, continuationMismatch())
      const binding = createOutputFailureBinding(failures)
      const { continueProgressiveZip } = await import('./progressive')
      signal.throwIfAborted()
      const result = await continueProgressiveZip(windowPort, continuation.operation, signal,
        diagnosticsFor('origin_private', options.outputTrace, binding.sinks))
      return result.kind === 'receive-continuation'
        ? Object.freeze({ ...result, runtime: bindRuntimeOutputFailures(result.runtime, binding, operation.display) }) : result
    }
    case 'direct-tree-receive':
    case 'workspace-start':
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

function isBrowserFolderStorageAction(operation: RetainedReceiveOperation, action: RetainedReceiveAction): boolean {
  if (action === 'save-staged-files' || action === 'cleanup-staging' || action === 'discard-incomplete-staging') return true
  return (action === 'discard' || action === 'delete') && operation.browserDelivery !== undefined &&
    hasBrowserDeliveryStaging(operation.browserDelivery)
}

function browserFolderStorageAction(action: RetainedReceiveAction) {
  if (action === 'save-staged-files' || action === 'cleanup-staging' || action === 'discard-incomplete-staging') return action
  return 'discard-staging' as const
}

async function continueRetainedReceive(
  executor: BrowserRetainedContinuationExecutor,
  continuation: ReceiveContinuation,
  action: RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<RetainedReceiveActionResult> {
  const directTreeRecovery = continuation.kind === 'direct-tree-receive' &&
    (action === 'continue' || action === 'redownload')
  if (action !== 'continue' && !directTreeRecovery) {
    return withRetainedOperationClose(continuation.operation, async () => {
      throw continuationMismatch()
    })
  }
  // The resumed runtime becomes the sole live owner. Its detach path closes the
  // output-owned operation after transfer and lifecycle controls are finished.
  let runtime: BoundReceiveOperation
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

async function repairWorkspaceInventory(
  windowPort: BrowserReceiveWindow,
  options: BrowserRetainedCompositionOptions,
  signal: AbortSignal,
): Promise<boolean> {
  if (options.openResumeSource === undefined || options.openActivationRepository !== undefined ||
      options.recoverWorkspaceActivations !== undefined) {
    let activationRepository: WorkspaceActivationJournalRepository
    try {
      activationRepository = await (
        options.openActivationRepository ?? (() => IndexedDbReceiveOperationRepository.open())
      )()
    } catch (error) {
      signal.throwIfAborted()
      if (isBrowserStoragePolicyDenial(error)) return false
      throw error
    }
    try {
      // Opening an empty inventory must not load the workspace repair engine.
      const needsRepair = options.recoverWorkspaceActivations !== undefined ||
        (await activationRepository.listWorkspaceActivationCandidates()).length > 0 ||
        (await activationRepository.listInitialWorkspaceActivationOperationIds()).length > 0
      if (needsRepair) await (options.recoverWorkspaceActivations ?? (await import('../../../output/workspace/activation-recovery')).recoverWorkspaceActivationCandidates)({
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
  return true
}
