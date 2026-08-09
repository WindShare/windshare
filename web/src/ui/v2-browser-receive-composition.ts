import { encodeBase64Url } from '../crypto/bytes'
import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../output/browser/session-lease'
import { IndexedDbReceiveOperationRepository } from '../output/browser/indexeddb-repository'
import { IndexedDbReceiveResumeSource } from '../output/browser/indexeddb-resume-state'
import {
  probeBrowserEnvironment,
  startFSAParentPicker,
} from '../output/capability/acquisition'
import type {
  AcquiredFSAParentAuthority,
  BrowserCapabilityRuntime,
} from '../output/capability/contract'
import {
  createFileSystemAccessSettlementAuthority,
  type FileSystemAccessOperationSettlementAuthority,
} from '../output/file-system-access/settlement'
import {
  bindNewFileSystemAccessOutput,
  reopenFileSystemAccessOutput,
  type FileSystemAccessOutputSession,
} from '../output/file-system-access/session'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../output/origin-private/admission'
import {
  openOriginPrivateWorkspaceNamespace,
  removeOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../output/origin-private/namespace'
import {
  openOriginPrivateRetainedArtifactBackend,
  openOriginPrivateWorkspaceBackend,
  type OriginPrivateRetainedArtifactBackend,
  type OriginPrivateWorkspaceBackend,
} from '../output/origin-private/session'
import {
  OriginPrivatePackageWorkflow,
  type OriginPrivatePackageAttemptResult,
} from '../output/origin-private/workflow'
import {
  BrowserHandoffNotStartedError,
  createWindowBrowserHandoffPublisher,
} from '../output/portable/browser-download'
import {
  createPackagedArtifactHandoffPublisher,
  probeBrowserHandoffCapabilities,
  type BrowserHandoffCapabilityRuntime,
} from '../output/portable/packaged-handoff'
import {
  createPortableExecutionRoutes,
  type PortableAbortRecord,
  type PortableAdmissionRejectionRecord,
  type PortableDownloadStartedRecord,
  type PortableExecutionLifecycleAuthority,
} from '../output/portable/preparation'
import { artifactRequestedName } from '../output/planning'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
  type ReceiveOperationResumeRef,
  type ReceiveOperationResumeSource,
} from '../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
  ReopenedDirectTreeOperation,
  ReopenedWorkspaceOperation,
} from '../output/resume/reopen-authority'
import type {
  AcquiredMaterializationAuthority,
  ArtifactAction,
  BrowserHandoffTargetOffer,
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
} from '../output/planning'
import type {
  PersistentWorkspaceSettlementAuthority,
  WorkspaceMaterializationEvidence,
  PersistentMaterializationSettlementCut,
} from '../transfer/settlement/persistent-execution'
import {
  createPersistentDirectTreeExecution,
  createPersistentWorkspaceExecution,
} from '../transfer/settlement/persistent-execution'
import {
  createV2PlanExecutionAuthority,
  type V2PlanExecutionRouteRegistry,
  type V2UnopenedExecutionLifecycle,
} from '../transfer/settlement/v2-plan-authority'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  createOperationID,
  createOutputSessionID,
  createPortableBinding,
  createPortablePlanID,
  createTransferJobID,
  createWorkspaceBinding,
  createWorkspaceID,
  type DirectoryTreeArtifact,
  type ReceiveIntent,
} from '../transfer/intent'
import {
  TransferPauseRequestedError,
  outputSessionIdentity,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type ExecutionAdmissionResult,
  type PlanPauseRequest,
  type PlanSettlementRequest,
  type PortableExecution,
  type V2PlanExecutionAuthority,
  type WorkspaceExecution,
} from '../transfer/output-session'
import type { SuccessfulTransferWorkerSettlement } from '../transfer/outcome'
import {
  DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
  DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
  MINIMUM_OPFS_QUOTA_RESERVE,
} from '../output/workspace/budget'
import {
  reduceReceiveLifecycle,
  type LifecycleEvent,
} from '../output/workspace/lifecycle'
import type {
  WorkspaceCheckpointCleanupObservation,
  WorkspaceOwnedCleanupPort,
  WorkspaceOwnedObjectCleanupObservation,
} from '../output/workspace/cleanup'
import {
  decodeStoredReceiveLifecycleState,
} from '../output/workspace/state-codec'
import {
  initialReceiveLifecycleState,
  type ReceiveLifecycleState,
} from '../output/workspace/state'
import type { ReceiveOperationRepository } from '../output/workspace/repository'
import {
  WorkspaceOperationStages,
  type AdmittedWorkspaceContent,
  type WorkspaceCleanupRequest,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from '../output/workspace/stages'
import {
  sealWorkspaceZipPreparation,
  type SealedWorkspaceZipPreparationV1,
} from '../output/workspace/preparation'
import { IndexedDbZipCentralDirectorySpool } from '../output/streams/zip-spool'
import type {
  LifecycleUserAction,
  V2ActiveReceiveControl,
  WorkspaceUsage,
} from './v2-lifecycle-presentation'
import type {
  V2BoundReceiveOperation,
  V2LifecycleMutation,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
  V2StartedArtifactAuthority,
} from './v2-receive-runtime'

const WORKSPACE_ENVIRONMENT_OFFER_ID = 'browser-origin-private-workspace'
const PORTABLE_ENVIRONMENT_OFFER_ID = 'browser-portable-memory'
const AUTHORITY_REFERENCE_BYTES = 32
const PACKAGE_CLEANUP_RETRY_LIMIT = 3
type LifecycleEventPayload = LifecycleEvent extends infer Event
  ? Event extends LifecycleEvent
    ? Omit<Event, 'expectedGeneration' | 'leaseId'>
    : never
  : never
const ZERO_CONTENT_REQUESTS: WorkspaceContentRequestCounter = Object.freeze({ count: () => 0n })
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

interface OriginPrivateStorageManager extends StorageManager {
  getDirectory(): Promise<FileSystemDirectoryHandle>
}

interface BrowserReceiveNavigator extends Navigator {
  readonly storage: OriginPrivateStorageManager
  readonly locks: LockManager
}

export type BrowserReceiveWindow = Window & Readonly<{
  readonly navigator: BrowserReceiveNavigator
  readonly indexedDB: IDBFactory
  readonly URL: typeof URL
  readonly Blob: typeof Blob
  readonly File: typeof File
  readonly WritableStream: typeof WritableStream
  readonly showDirectoryPicker?: BrowserCapabilityRuntime['showDirectoryPicker']
}>

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
  ): Promise<V2BoundReceiveOperation>
  resumePackage(
    continuation: WorkspacePackageContinuation,
    signal: AbortSignal,
  ): Promise<void>
  continueRetained(
    continuation: WorkspaceRetainedContinuation,
    action: RetainedContinuationAction,
    signal: AbortSignal,
  ): Promise<void>
}

export interface BrowserReceiveCompositionOptions {
  readonly openResumeSource?: () => Promise<ReceiveOperationResumeSource & { close(): void }>
  readonly resumeMutations?:
    ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult>
  readonly continuationExecutor?: BrowserRetainedContinuationExecutor
  readonly now?: () => number
  readonly onTrace?: WorkspaceStageTraceListener
}

interface BrowserProviders {
  readonly fsa: boolean
  readonly workspace: boolean
  readonly portable: boolean
  readonly runtime: BrowserCapabilityRuntime
  readonly handoffTarget: BrowserHandoffTargetOffer | null
  readonly workspaceOffer: WorkspaceEnvironmentOffer | null
  readonly portableOffer: PortableEnvironmentOffer | null
}

/**
 * Product offers are derived from complete provider assemblies, not isolated browser APIs.
 * That prevents a picker or Blob probe from advertising a route whose durable owner is absent.
 */
export function createBrowserReceiveComposition(
  windowPort: BrowserReceiveWindow,
  options: BrowserReceiveCompositionOptions = {},
): V2ReceiveCompositionPort {
  const composition: V2ReceiveCompositionPort = {
    retained: Object.freeze({
      list: (signal: AbortSignal) => listBrowserRetainedOperations(windowPort, options, signal),
    }),
    environment: async (signal) => {
      const providers = await inspectBrowserProviders(windowPort, signal)
      return probeBrowserEnvironment(providers.runtime, {
        ...(providers.workspaceOffer === null ? {} : { workspace: providers.workspaceOffer }),
        ...(providers.portableOffer === null ? {} : { portable: providers.portableOffer }),
      }).offers
    },
    startArtifactAuthority: (action) => startProductionAuthority(windowPort, action, options.onTrace),
  }
  return Object.freeze(composition)
}

async function listBrowserRetainedOperations(
  windowPort: BrowserReceiveWindow,
  options: BrowserReceiveCompositionOptions,
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
      let open = true
      return Object.freeze({
        operations,
        act: async (
          operation: V2RetainedReceiveOperation,
          action: V2RetainedReceiveAction,
          actionSignal: AbortSignal,
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
    act: () => Promise.reject(new DOMException(
      'Retained operation authority is unavailable',
      'InvalidStateError',
    )),
    close: () => undefined,
  })
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
  options: BrowserReceiveCompositionOptions,
  authority: ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>,
  reference: ReceiveOperationResumeRef,
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
): Promise<V2RetainedReceiveActionResult> {
  if (action === 'discard' || (action === 'delete' &&
      operation.continuation !== 'cleanup-expired')) {
    await authority.discard(reference)
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  const result = await authority.resume(reference)
  if (result.kind === 'retention-cleanup') {
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  const executor = options.continuationExecutor ??
    browserRetainedContinuationExecutor(windowPort, options.onTrace)
  const { continuation } = result
  switch (continuation.kind) {
    case 'direct-tree-receive':
    case 'workspace-receive': {
      if (action !== 'continue') {
        await continuation.operation.close()
        throw continuationMismatch()
      }
      // The resumed runtime becomes the sole live owner. Its detach path closes the
      // output-owned operation after transfer and lifecycle controls are finished.
      const runtime = await executor.resumeReceive(continuation, signal).catch(async (error: unknown) => {
        await continuation.operation.close().catch(() => undefined)
        throw error
      })
      try {
        signal.throwIfAborted()
        return Object.freeze({ kind: 'receive-continuation', runtime })
      } catch (error) {
        await Promise.resolve(runtime.detach()).catch(() => undefined)
        throw error
      }
    }
    case 'workspace-package':
      try {
        if (action !== 'continue') throw continuationMismatch()
        signal.throwIfAborted()
        await executor.resumePackage(continuation, signal)
        signal.throwIfAborted()
        return Object.freeze({ kind: 'completed' })
      } finally {
        await continuation.operation.close()
      }
    case 'workspace-retained':
      try {
        if (action !== 'save' && action !== 'redownload') throw continuationMismatch()
        signal.throwIfAborted()
        await executor.continueRetained(continuation, action, signal)
        signal.throwIfAborted()
        return Object.freeze({ kind: 'completed' })
      } finally {
        await continuation.operation.close()
      }
  }
}

function browserRetainedContinuationExecutor(
  windowPort: BrowserReceiveWindow,
  trace: WorkspaceStageTraceListener | undefined,
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
    resumeReceive: async (
      continuation: ReceiveContinuation,
      signal: AbortSignal,
    ): Promise<V2BoundReceiveOperation> => {
      signal.throwIfAborted()
      switch (continuation.kind) {
        case 'direct-tree-receive':
          return FSAReceiveOperation.reopen(continuation.operation)
        case 'workspace-receive': {
          const backend = await continuation.operation.receiveContinuation.openBackend(
            trace === undefined ? {} : { onTrace: trace },
          )
          signal.throwIfAborted()
          return WorkspaceReceiveOperation.reopen({
            windowPort,
            operation: continuation.operation,
            backend,
            ...(trace === undefined ? {} : { trace }),
          })
        }
      }
    },
    resumePackage: async (
      continuation: WorkspacePackageContinuation,
      signal: AbortSignal,
    ) => {
      await continuation.operation.packageContinuation.execute(signal)
    },
    continueRetained: (
      continuation: WorkspaceRetainedContinuation,
      action: RetainedContinuationAction,
      signal: AbortSignal,
    ) =>
      continueRetainedWorkspaceOperation(windowPort, continuation.operation, action, signal),
  })
}

async function continueRetainedWorkspaceOperation(
  windowPort: BrowserReceiveWindow,
  operation: ReopenedWorkspaceOperation,
  action: RetainedContinuationAction,
  signal: AbortSignal,
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
  })
  try {
    signal.throwIfAborted()
    await handoffRetainedWorkspacePackage(windowPort, operation, backend)
  } finally {
    await backend.close()
  }
}

function continuationMismatch(): DOMException {
  return new DOMException(
    'Retained continuation does not match its reopened authority',
    'InvalidStateError',
  )
}

async function inspectBrowserProviders(
  windowPort: BrowserReceiveWindow,
  signal: AbortSignal,
): Promise<BrowserProviders> {
  signal.throwIfAborted()
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceStorage =
    typeof windowPort.navigator.storage?.getDirectory === 'function' &&
    typeof windowPort.navigator.storage?.estimate === 'function'
  const handoffFacts = probeBrowserHandoffCapabilities(
    windowPort as unknown as BrowserHandoffCapabilityRuntime,
  )
  const workspace = hasRepository && hasLocks && hasWorkspaceStorage &&
    handoffFacts.supportsWorkspacePackage
  const portable = hasRepository && handoffFacts.supportsPortableArtifact
  const runtime: BrowserCapabilityRuntime = Object.freeze({
    ...(hasRepository && hasLocks && typeof windowPort.showDirectoryPicker === 'function'
      ? { showDirectoryPicker: windowPort.showDirectoryPicker.bind(windowPort) }
      : {}),
    browserHandoff: windowPort as unknown as BrowserHandoffCapabilityRuntime,
  })
  const initial = probeBrowserEnvironment(runtime)
  const workspaceOffer = workspace
    ? workspaceEnvironmentOffer(await quotaAvailability(windowPort.navigator.storage, signal))
    : null
  const portableOffer = portable ? portableEnvironmentOffer() : null
  return Object.freeze({
    fsa: initial.fsaParent !== null,
    workspace,
    portable,
    runtime,
    handoffTarget: initial.browserHandoff,
    workspaceOffer,
    portableOffer,
  })
}

function inspectBrowserProvidersSynchronously(windowPort: BrowserReceiveWindow): BrowserProviders {
  const hasRepository = typeof windowPort.indexedDB?.open === 'function'
  const hasLocks = typeof windowPort.navigator.locks?.request === 'function'
  const hasWorkspaceStorage =
    typeof windowPort.navigator.storage?.getDirectory === 'function' &&
    typeof windowPort.navigator.storage?.estimate === 'function'
  const handoffFacts = probeBrowserHandoffCapabilities(
    windowPort as unknown as BrowserHandoffCapabilityRuntime,
  )
  const runtime: BrowserCapabilityRuntime = Object.freeze({
    ...(hasRepository && hasLocks && typeof windowPort.showDirectoryPicker === 'function'
      ? { showDirectoryPicker: windowPort.showDirectoryPicker.bind(windowPort) }
      : {}),
    browserHandoff: windowPort as unknown as BrowserHandoffCapabilityRuntime,
  })
  const environment = probeBrowserEnvironment(runtime)
  return Object.freeze({
    fsa: environment.fsaParent !== null,
    workspace: hasRepository && hasLocks && hasWorkspaceStorage &&
      handoffFacts.supportsWorkspacePackage,
    portable: hasRepository && handoffFacts.supportsPortableArtifact,
    runtime,
    handoffTarget: environment.browserHandoff,
    workspaceOffer: hasRepository && hasLocks && hasWorkspaceStorage &&
        handoffFacts.supportsWorkspacePackage
      ? workspaceEnvironmentOffer(null)
      : null,
    portableOffer: hasRepository && handoffFacts.supportsPortableArtifact
      ? portableEnvironmentOffer()
      : null,
  })
}

function startProductionAuthority(
  windowPort: BrowserReceiveWindow,
  action: ArtifactAction,
  trace: WorkspaceStageTraceListener | undefined,
): V2StartedArtifactAuthority {
  const providers = inspectBrowserProvidersSynchronously(windowPort)
  switch (action.plan.kind) {
    case 'direct-tree': {
      if (!providers.fsa || action.plan.target.kind !== 'fsa-parent-directory') {
        throw unavailableRoute()
      }
      // startFSAParentPicker invokes showDirectoryPicker before returning this click-stack call.
      const picked = startFSAParentPicker(providers.runtime, action.plan.target)
      return new StartedFSAReceive(action, picked)
    }
    case 'workspace-then-publish':
      if (!providers.workspace || providers.workspaceOffer === null ||
          providers.handoffTarget?.supportsWorkspacePackage !== true) {
        throw unavailableRoute()
      }
      return new StartedWorkspaceReceive(windowPort, action, trace)
    case 'portable-handoff':
      if (!providers.portable || providers.portableOffer === null ||
          providers.handoffTarget?.supportsPortableArtifact !== true) {
        throw unavailableRoute()
      }
      return new StartedPortableReceive(windowPort, action)
    case 'direct-atomic':
      throw unavailableRoute()
  }
}

class StartedFSAReceive implements V2StartedArtifactAuthority {
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

class FSAReceiveOperation implements V2BoundReceiveOperation {
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
    const session = await reopenFileSystemAccessOutput({
      intent: operation.intent,
      operationRepository: operation.repository,
    })
    try {
      const attempt = await createFSAAttempt(
        operation.intent,
        operation.repository,
        operation.lease,
        session,
        'resume',
      )
      return new FSAReceiveOperation({
        intent: operation.intent,
        lifecycle: operation.lifecycle,
        repository: operation.repository,
        lease: operation.lease,
        session,
        closeAuthority: () => operation.close(),
        ...attempt,
      })
    } catch (error) {
      await session.close().catch(() => undefined)
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
    let resumed: ReceiveLifecycleState
    try {
      resumed = await transitionLifecycle(
        this.#repository,
        this.intent,
        this.#lease.leaseId,
        { kind: 'resume-started' },
      )
    } catch (error) {
      await session.close().catch(() => undefined)
      throw error
    }
    const attempt = await createFSAAttempt(this.intent, this.#repository, this.#lease, session, 'resume')
    this.#session = session
    this.#settlement = attempt.settlement
    this.#plans = attempt.plans
    this.#transferJobId = attempt.transferJobId
    return Object.freeze({
      lifecycle: resumed,
      activeControls: this.activeControls,
      resumeTransfer: true,
    })
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

  async abandon(reason: unknown): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const controller = new AbortController()
    const lifecycle = await this.#settlement.abortUnopened(this.intent, reason, controller.signal)
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
): Promise<Readonly<{
  settlement: FileSystemAccessOperationSettlementAuthority
  plans: V2PlanExecutionAuthority
  transferJobId: string
}>> {
  const transferJobId = createTransferJobID()
  const settlement = await createFileSystemAccessSettlementAuthority({
    intent,
    repository,
    lifecycleLeaseId: lease.leaseId,
    transferJobId,
  })
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
  return Object.freeze({ settlement, plans, transferJobId })
}

class StartedWorkspaceReceive implements V2StartedArtifactAuthority {
  readonly #window: BrowserReceiveWindow
  readonly #action: ArtifactAction
  readonly #trace: WorkspaceStageTraceListener | undefined
  #released = false
  #claimed = false

  constructor(
    windowPort: BrowserReceiveWindow,
    action: ArtifactAction,
    trace: WorkspaceStageTraceListener | undefined,
  ) {
    this.#window = windowPort
    this.#action = action
    this.#trace = trace
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    signal.throwIfAborted()
    const action = requireWorkspaceAction(this.#action)
    const operationId = createOperationID()
    const workspace = await createWorkspaceBinding({
      operationId,
      workspaceId: createWorkspaceID(),
      artifact: action.artifact,
      repositoryRef: randomAuthorityReference(),
    })
    const intent = await freezeIntent(Object.freeze({
      kind: 'workspace-binding',
      workspaceOfferId: action.plan.workspace.id,
      workspace,
    }))
    signal.throwIfAborted()
    this.#requireLive()
    const repository = await IndexedDbReceiveOperationRepository.open()
    let lease: BrowserReceiveOperationLease | undefined
    let namespace: OriginPrivateWorkspaceNamespace | undefined
    try {
      namespace = await openOriginPrivateWorkspaceNamespace({
        receiveIntent: intent,
        repository,
        storage: this.#window.navigator.storage,
      })
      lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId)
      const stages = await WorkspaceOperationStages.open({
        repository,
        receiveIntent: intent,
        leaseId: lease.leaseId,
        clock: Date.now,
        contentRequests: ZERO_CONTENT_REQUESTS,
        ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
      })
      const runtime = await WorkspaceReceiveOperation.create({
        windowPort: this.#window,
        intent,
        repository,
        namespace,
        lease,
        stages,
        ...(this.#trace === undefined ? {} : { trace: this.#trace }),
      })
      signal.throwIfAborted()
      return runtime
    } catch (error) {
      if (namespace !== undefined) {
        await removeOriginPrivateWorkspaceNamespace(namespace, repository).catch(() => undefined)
      }
      await lease?.release().catch(() => undefined)
      repository.close()
      throw error
    }
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) {
      throw new DOMException('Workspace artifact authority was already finalized', 'InvalidStateError')
    }
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }
}

interface WorkspaceSealBundle {
  readonly preparation?: SealedWorkspaceZipPreparationV1
  readonly sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>
}

class WorkspaceReceiveOperation implements V2BoundReceiveOperation, V2UnopenedExecutionLifecycle {
  readonly intent: ReceiveIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly activeControls = Object.freeze(['pause'] as const)
  readonly initialWorkspaceUsage: WorkspaceUsage
  readonly #window: BrowserReceiveWindow
  readonly #repository: ReceiveOperationRepository
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #lease: BrowserReceiveOperationLease
  readonly #stages: WorkspaceOperationStages
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #closeAuthority: (() => Promise<void>) | undefined
  #plans!: V2PlanExecutionAuthority
  #transferJobId: string
  #backend: OriginPrivateWorkspaceBackend | undefined
  #admitted: AdmittedWorkspaceContent | undefined
  #budgetClaim: OriginPrivateWorkspaceBudgetClaim | undefined
  #preparation: SealedWorkspaceZipPreparationV1 | undefined
  #sealBundle: WorkspaceSealBundle | undefined
  #packageExactBytes: bigint | undefined
  #detached = false

  private constructor(input: {
    windowPort: BrowserReceiveWindow
    intent: ReceiveIntent
    lifecycle?: ReceiveLifecycleState
    repository: ReceiveOperationRepository
    namespace: OriginPrivateWorkspaceNamespace
    lease: BrowserReceiveOperationLease
    stages: WorkspaceOperationStages
    trace?: WorkspaceStageTraceListener
    transferJobId: string
    backend?: OriginPrivateWorkspaceBackend
    admitted?: AdmittedWorkspaceContent
    preparation?: SealedWorkspaceZipPreparationV1
    closeAuthority?: () => Promise<void>
  }) {
    this.#window = input.windowPort
    this.intent = input.intent
    this.lifecycle = input.lifecycle ?? initialReceiveLifecycleState({
      operationId: input.intent.operationId,
      receiveIntentDigest: input.intent.digest,
    })
    this.#repository = input.repository
    this.#namespace = input.namespace
    this.#lease = input.lease
    this.#stages = input.stages
    this.#trace = input.trace
    this.#closeAuthority = input.closeAuthority
    this.#transferJobId = input.transferJobId
    this.#backend = input.backend
    this.#admitted = input.admitted
    this.#preparation = input.preparation
    this.initialWorkspaceUsage = Object.freeze({
      ownedBytes: 0n,
      maximumBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
    })
  }

  static async create(input: {
    windowPort: BrowserReceiveWindow
    intent: ReceiveIntent
    repository: IndexedDbReceiveOperationRepository
    namespace: OriginPrivateWorkspaceNamespace
    lease: BrowserReceiveOperationLease
    stages: WorkspaceOperationStages
    trace?: WorkspaceStageTraceListener
  }): Promise<WorkspaceReceiveOperation> {
    const owner = new WorkspaceReceiveOperation({
      windowPort: input.windowPort,
      intent: input.intent,
      repository: input.repository,
      namespace: input.namespace,
      lease: input.lease,
      stages: input.stages,
      ...(input.trace === undefined ? {} : { trace: input.trace }),
      transferJobId: createTransferJobID(),
    })
    owner.#plans = await workspacePlanAuthority(input.intent, owner)
    return owner
  }

  static async reopen(input: {
    windowPort: BrowserReceiveWindow
    operation: Extract<WorkspaceReceiveContinuation, { kind: 'workspace-receive' }>['operation']
    backend: OriginPrivateWorkspaceBackend
    trace?: WorkspaceStageTraceListener
  }): Promise<WorkspaceReceiveOperation> {
    const { operation } = input
    const owner = new WorkspaceReceiveOperation({
      windowPort: input.windowPort,
      intent: operation.intent,
      lifecycle: operation.lifecycle,
      repository: operation.repository,
      namespace: operation.namespace,
      lease: operation.lease,
      stages: operation.stages,
      ...(input.trace === undefined ? {} : { trace: input.trace }),
      transferJobId: createTransferJobID(),
      backend: input.backend,
      admitted: operation.admittedContent,
      ...(operation.receiveContinuation.preparation === undefined
        ? {}
        : { preparation: operation.receiveContinuation.preparation }),
      closeAuthority: () => operation.close(),
    })
    owner.#plans = await workspacePlanAuthority(operation.intent, owner)
    return owner
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
    switch (action) {
      case 'continue':
        return this.#continue(lifecycle)
      case 'save':
      case 'redownload':
        return this.#handoff(lifecycle)
      case 'discard':
      case 'delete':
        return this.#discard()
      case 'change-location':
        throw unavailableRoute()
    }
  }

  async observeExpiry(): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const result = await this.#stages.expireIfDue(this.#cleanupRequest())
    const state = result.kind === 'not-due' ? result.state : result.cleanup.state
    return Object.freeze({ lifecycle: state, workspaceUsage: this.resolveWorkspaceUsage(state) })
  }

  resolveWorkspaceUsage(lifecycle: ReceiveLifecycleState): WorkspaceUsage | null {
    if (lifecycle.kind === 'discarded' ||
        (lifecycle.kind === 'expired' && lifecycle.cleanupState === 'clean')) return null
    let ownedBytes = 0n
    if (lifecycle.kind === 'resumable-receive') ownedBytes = lifecycle.completedBytes
    else if (this.#packageExactBytes !== undefined &&
        (lifecycle.kind === 'waiting-to-save' ||
         (lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace'))) {
      ownedBytes = this.#packageExactBytes
    } else if (this.#sealBundle !== undefined) {
      ownedBytes = this.#sealBundle.sealed.manifest.rawBytes
    }
    return Object.freeze({ ownedBytes, maximumBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT })
  }

  async abandon(): Promise<V2LifecycleMutation> {
    this.#requireAttached()
    const current = await readLifecycle(this.#repository, this.intent.operationId)
    if (isWorkspaceTerminal(current)) {
      return Object.freeze({ lifecycle: current, workspaceUsage: this.resolveWorkspaceUsage(current) })
    }
    return this.#discard()
  }

  async abortUnopened(
    intent: ReceiveIntent,
    _reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState> {
    requireSameIntent(this.intent, intent)
    signal.throwIfAborted()
    const current = await readLifecycle(this.#repository, this.intent.operationId)
    if (isWorkspaceTerminal(current)) return current
    return (await this.#stages.discard(this.#cleanupRequest())).state
  }

  async recordSettlementUnknown(
    intent: ReceiveIntent,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>> {
    requireSameIntent(this.intent, intent)
    return this.#stages.recordTargetOwnershipUnknown(
      this.#sealBundle?.sealed.seal.digest ?? this.intent.digest,
    )
  }

  async detach(): Promise<void> {
    if (this.#detached) return
    this.#detached = true
    if (this.#closeAuthority !== undefined) {
      await this.#closeAuthority()
      return
    }
    const tasks: Promise<unknown>[] = [this.#lease.release()]
    if (this.#backend !== undefined) tasks.push(this.#backend.close())
    if (this.#budgetClaim !== undefined) tasks.push(this.#budgetClaim.release())
    const results = await Promise.allSettled(tasks)
    this.#repository.close()
    const failures = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    if (failures.length > 0) {
      throw new AggregateError(failures.map(result => result.reason), 'Workspace receive resources did not detach')
    }
  }

  async admitOriginal(
    intent: ReceiveIntent,
    evidence: ExactSingleFileEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      requireMatchingSingleFileAdmission(this.#admitted, evidence)
      return this.#reopenWorkspaceExecution(intent, {
        kind: 'single-file',
        evidence,
      }, signal)
    }
    const budget = await this.#budgetAuthority()
    const result = await this.#stages.admitSingleFile({
      fileId: evidence.fileId,
      containingDirectoryId: evidence.containingDirectoryId,
      generation: evidence.generation,
      catalogSize: evidence.catalogSize,
      authority: budget,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: this.#namespaceCleanupRequest(),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    return this.#openWorkspaceExecution(intent, {
      kind: 'single-file',
      evidence,
    }, result.content, signal)
  }

  async prepareZip(
    intent: ReceiveIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    requireSameIntent(this.intent, intent)
    if (this.#admitted !== undefined) {
      const existing = requirePreparation(this.#preparation)
      const verified = await sealWorkspaceZipPreparation({
        receiveIntent: intent,
        preparationId: existing.manifest.preparationId,
        generations: evidence.generations,
        entries: evidence.entries,
      })
      if (verified.manifest.digest !== existing.manifest.digest ||
          verified.zipLayout.digest !== existing.zipLayout.digest) {
        throw new TypeError('Workspace ZIP continuation changed its admitted preparation')
      }
      return this.#reopenWorkspaceExecution(intent, {
        kind: 'prepared',
        evidence,
      }, signal)
    }
    const preparationId = createOperationID()
    await this.#stages.beginReceive(preparationId)
    const preparation = await sealWorkspaceZipPreparation({
      receiveIntent: intent,
      preparationId,
      generations: evidence.generations,
      entries: evidence.entries,
    })
    const budget = await this.#budgetAuthority()
    const result = await this.#stages.admitPreparedZip({
      preparation,
      authority: budget,
      durableMetadataBytesExcludingAdmissionRecords: 0n,
      rejectionCleanup: this.#namespaceCleanupRequest(),
    })
    if (result.kind === 'rejected') return Object.freeze({ kind: 'rejected', state: result.state })
    this.#preparation = preparation
    return this.#openWorkspaceExecution(intent, {
      kind: 'prepared',
      evidence,
    }, result.content, signal)
  }

  async #reopenWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    const admitted = this.#admitted
    if (admitted === undefined) {
      throw new DOMException('Workspace continuation lost its admission authority', 'InvalidStateError')
    }
    if (this.#closeAuthority !== undefined) {
      const backend = this.#backend
      if (backend === undefined) {
        throw new DOMException('Workspace continuation lost its reopened backend', 'InvalidStateError')
      }
      return this.#createWorkspaceExecution(intent, admission, backend, signal)
    }
    const claim = this.#budgetClaim
    if (claim === undefined) {
      throw new DOMException('Workspace continuation lost its budget authority', 'InvalidStateError')
    }
    const reopened = await this.#stages.reopenAdmittedContent({
      budget: admitted.budget,
      claim,
    })
    return this.#openWorkspaceExecution(intent, admission, reopened, signal)
  }

  async #openWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    content: AdmittedWorkspaceContent,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    this.#admitted = content
    this.#budgetClaim = content.claim as OriginPrivateWorkspaceBudgetClaim
    this.#backend = await openOriginPrivateWorkspaceBackend({
      receiveIntent: intent,
      operationRepository: this.#repository,
      namespace: this.#namespace,
      contentGate: content.gate,
      budgetClaim: this.#budgetClaim,
      ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
    })
    return this.#createWorkspaceExecution(intent, admission, this.#backend, signal)
  }

  async #createWorkspaceExecution(
    intent: ReceiveIntent,
    admission:
      | Readonly<{ kind: 'single-file'; evidence: ExactSingleFileEvidence }>
      | Readonly<{ kind: 'prepared'; evidence: ExactPreparationEvidence }>,
    backend: OriginPrivateWorkspaceBackend,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>> {
    const settlement = this.#workspaceSettlement(backend)
    const execution = await createPersistentWorkspaceExecution({
      intent: intent as Parameters<typeof createPersistentWorkspaceExecution>[0]['intent'],
      admission: admission as Parameters<typeof createPersistentWorkspaceExecution>[0]['admission'],
      materialization: backend.materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'browser-origin-private-workspace',
        outputSessionId: createOutputSessionID(),
      }),
      settlement,
      signal,
    } as Parameters<typeof createPersistentWorkspaceExecution>[0])
    return Object.freeze({ kind: 'accepted', execution })
  }

  #workspaceSettlement(
    backend: OriginPrivateWorkspaceBackend,
  ): PersistentWorkspaceSettlementAuthority {
    return Object.freeze({
      pause: async (
        _request: PlanPauseRequest,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
      ) => {
        await cut.closeMaterialization()
        const files = cut.evidence.entries.filter(entry => entry.kind === 'file')
        const completedBytes = files.reduce((total, entry) => total + entry.exactSize, 0n)
        return this.#stages.pauseReceive({
          checkpointSetDigest: await checkpointSetDigest(this.intent, cut.evidence),
          completedFileCount: BigInt(files.length),
          completedBytes,
        })
      },
      settle: async (
        request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>,
        cut: PersistentMaterializationSettlementCut<WorkspaceMaterializationEvidence>,
        signal: AbortSignal,
      ) => {
        if (request.transferJobId !== this.#transferJobId) {
          throw new TypeError('Workspace settlement escaped its active transfer attempt')
        }
        await cut.closeMaterialization()
        const sealed = await this.#stages.sealMaterialization({
          transferJobId: request.transferJobId,
          generations: cut.evidence.generations,
          entries: cut.evidence.entries,
          checkpoints: backend.finalCheckpoints,
          ...(this.#preparation === undefined ? {} : { preparation: this.#preparation }),
        })
        this.#sealBundle = Object.freeze({
          sealed,
          ...(this.#preparation === undefined ? {} : { preparation: this.#preparation }),
        })
        const result = await this.#package(sealed, signal)
        return result.state
      },
    })
  }

  async #package(
    sealed: Awaited<ReturnType<WorkspaceOperationStages['sealMaterialization']>>,
    signal: AbortSignal,
    retry = false,
  ): Promise<Exclude<OriginPrivatePackageAttemptResult, { kind: 'cleanup-pending' }>> {
    const backend = this.#backend
    if (backend === undefined) throw new DOMException('Workspace backend is unavailable', 'InvalidStateError')
    const workflow = new OriginPrivatePackageWorkflow({ stages: this.#stages, store: backend.packages })
    let result: OriginPrivatePackageAttemptResult = this.intent.artifact.kind === 'zip-archive'
      ? await workflow.buildZip({
          receiveIntentDigest: this.intent.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          layout: requirePreparation(this.#preparation).zipLayout,
          signal,
          retry,
        })
      : await workflow.buildOriginalFile({
          receiveIntentDigest: this.intent.digest,
          artifactSpecDigest: this.intent.artifact.digest,
          sealedMaterialization: sealed.seal,
          materializedManifest: sealed.manifest,
          signal,
          retry,
        })
    for (let attempt = 0; result.kind === 'cleanup-pending' &&
        attempt < PACKAGE_CLEANUP_RETRY_LIMIT; attempt += 1) {
      result = await result.retryCleanup()
    }
    if (result.kind === 'cleanup-pending') {
      throw new DOMException('Workspace package cleanup remains pending', 'OperationError')
    }
    if (result.kind === 'sealed') this.#packageExactBytes = result.package.exactBytes
    return result
  }

  async #continue(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    if (lifecycle.kind === 'resumable-package') {
      const seal = this.#sealBundle?.sealed
      if (seal === undefined) throw new DOMException('Package continuation proof is unavailable', 'InvalidStateError')
      const result = await this.#package(seal, new AbortController().signal, true)
      return Object.freeze({
        lifecycle: result.state,
        workspaceUsage: this.resolveWorkspaceUsage(result.state),
      })
    }
    if (lifecycle.kind !== 'resumable-receive' || this.#admitted === undefined ||
        (this.#closeAuthority === undefined && this.#budgetClaim === undefined)) throw unavailableRoute()
    if (this.#closeAuthority === undefined) {
      await this.#backend?.close()
      this.#backend = undefined
    }
    await this.#stages.resumeReceive()
    this.#transferJobId = createTransferJobID()
    this.#plans = await workspacePlanAuthority(this.intent, this)
    const current = await readLifecycle(this.#repository, this.intent.operationId)
    return Object.freeze({
      lifecycle: current,
      activeControls: this.activeControls,
      workspaceUsage: this.resolveWorkspaceUsage(current),
      resumeTransfer: true,
    })
  }

  async #handoff(lifecycle: ReceiveLifecycleState): Promise<V2LifecycleMutation> {
    if (lifecycle.kind !== 'waiting-to-save' &&
        !(lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace')) {
      throw unavailableRoute()
    }
    const backend = this.#backend
    if (backend === undefined) throw new DOMException('Retained package backend is unavailable', 'InvalidStateError')
    const state = await handoffRetainedWorkspacePackage(
      this.#window,
      Object.freeze({ intent: this.intent, lifecycle, stages: this.#stages }),
      backend,
    )
    return Object.freeze({ lifecycle: state, workspaceUsage: this.resolveWorkspaceUsage(state) })
  }

  async #discard(): Promise<V2LifecycleMutation> {
    const result = await this.#stages.discard(this.#cleanupRequest())
    return Object.freeze({ lifecycle: result.state, workspaceUsage: null })
  }

  async #budgetAuthority(): Promise<OriginPrivateWorkspaceBudgetAuthority> {
    return OriginPrivateWorkspaceBudgetAuthority.open(this.intent.operationId, {
      estimate: () => this.#window.navigator.storage.estimate(),
    })
  }

  #cleanupRequest(): WorkspaceCleanupRequest {
    return this.#backend === undefined
      ? this.#namespaceCleanupRequest()
      : Object.freeze({ targets: Object.freeze([]), port: this.#backend.cleanup })
  }

  #namespaceCleanupRequest(): WorkspaceCleanupRequest {
    return Object.freeze({
      targets: Object.freeze([]),
      port: new NamespaceOnlyCleanupPort(this.#namespace, this.#repository, this.intent),
    })
  }

  #requireAttached(): void {
    if (this.#detached) throw new DOMException('Receive operation is detached', 'InvalidStateError')
  }
}

type RetainedWorkspaceHandoffOperation =
Pick<ReopenedWorkspaceOperation, 'intent' | 'lifecycle' | 'stages'>

async function handoffRetainedWorkspacePackage(
  windowPort: BrowserReceiveWindow,
  operation: RetainedWorkspaceHandoffOperation,
  backend: OriginPrivateRetainedArtifactBackend,
): Promise<ReceiveLifecycleState> {
  const { lifecycle } = operation
  if (lifecycle.kind !== 'waiting-to-save' &&
      !(lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace')) {
    throw unavailableRoute()
  }
  const artifact = await operation.stages.readRetainedPackage()
  const attempt = await operation.stages.startHandoff({
    package: artifact,
    publicationAttemptId: createOperationID(),
    suggestedName: artifactRequestedName(operation.intent.artifact),
    packagedFileSupported: true,
  })
  const retryableUntil = lifecycle.kind === 'waiting-to-save'
    ? lifecycle.expiresAt
    : lifecycle.retryableUntil
  try {
    const publisher = createPackagedArtifactHandoffPublisher({
      packages: backend.packagedArtifacts,
      browser: createWindowBrowserHandoffPublisher(windowPort),
      File: windowPort.File,
    })
    const started = await publisher.handoff({ artifact, attempt, retryableUntil })
    return (await operation.stages.recordHandoffStarted({
      package: artifact,
      attempt,
      urlLeaseStartedAt: started.urlLeaseStartedAt,
      urlLeaseEndsAt: started.urlLeaseEndsAt,
    })).state
  } catch (error) {
    return error instanceof BrowserHandoffNotStartedError
      ? await operation.stages.recordHandoffNotStarted({
          package: artifact,
          attempt,
          reason: error.externalAttemptReason,
        })
      : await operation.stages.recordHandoffUnknown({
          package: artifact,
          attempt,
          lastVerifiedRecordDigest: artifact.digest,
        })
  }
}

async function workspacePlanAuthority(
  intent: ReceiveIntent,
  owner: WorkspaceReceiveOperation,
): Promise<V2PlanExecutionAuthority> {
  const routes: V2PlanExecutionRouteRegistry = {
    ...(intent.artifact.kind === 'original-file'
      ? {
          workspaceOriginal: {
            admit: (boundIntent, evidence, signal) =>
              owner.admitOriginal(boundIntent, evidence, signal),
          },
        }
      : {}),
    ...(intent.artifact.kind === 'zip-archive'
      ? {
          workspaceZip: {
            prepare: (boundIntent, evidence, signal) =>
              owner.prepareZip(boundIntent, evidence, signal),
          },
        }
      : {}),
    lifecycle: owner,
  }
  return createV2PlanExecutionAuthority({ intent, routes })
}

class NamespaceOnlyCleanupPort implements WorkspaceOwnedCleanupPort {
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #repository: ReceiveOperationRepository
  readonly #intent: ReceiveIntent

  constructor(
    namespace: OriginPrivateWorkspaceNamespace,
    repository: ReceiveOperationRepository,
    intent: ReceiveIntent,
  ) {
    this.#namespace = namespace
    this.#repository = repository
    this.#intent = intent
  }

  removeOwnedObject(): Promise<WorkspaceOwnedObjectCleanupObservation> {
    return Promise.resolve(Object.freeze({ kind: 'ownership-unknown' }))
  }

  async removeFileCheckpoints(input: {
    readonly operationId: string
    readonly receiveIntentDigest: string
  }): Promise<WorkspaceCheckpointCleanupObservation> {
    if (input.operationId !== this.#intent.operationId ||
        input.receiveIntentDigest !== this.#intent.digest) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    try {
      await removeOriginPrivateWorkspaceNamespace(this.#namespace, this.#repository)
      return Object.freeze({ kind: 'clean', removedRecordDigests: Object.freeze([]) })
    } catch {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
  }
}

class StartedPortableReceive implements V2StartedArtifactAuthority {
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

async function transitionLifecycle(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
  payload: LifecycleEventPayload,
  expected?: ReceiveLifecycleState,
): Promise<ReceiveLifecycleState> {
  const current = await readLifecycle(repository, intent.operationId)
  if (expected !== undefined && current.generation !== expected.generation) {
    throw new DOMException('Receive lifecycle changed before the requested transition', 'InvalidStateError')
  }
  const reduction = reduceReceiveLifecycle(current, {
    ...payload,
    expectedGeneration: current.generation,
    leaseId,
  } as LifecycleEvent, {
    planKind: intent.plan.kind,
    preparationRequired: intent.plan.preparation !== 'none',
    activeLeaseId: leaseId,
    nowMilliseconds: Date.now(),
  })
  if (reduction.status !== 'applied') {
    throw new DOMException('Receive lifecycle transition was stale', 'InvalidStateError')
  }
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

async function readLifecycle(
  repository: ReceiveOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('Receive operation has no lifecycle authority')
  return decodeStoredReceiveLifecycleState(record)
}

async function checkpointSetDigest(
  intent: ReceiveIntent,
  evidence: WorkspaceMaterializationEvidence,
): Promise<string> {
  const entries = evidence.entries
    .filter(entry => entry.kind === 'file')
    .map(entry => `${entry.fileId}:${entry.checkpoint.recordDigest}:${entry.exactSize.toString()}`)
    .sort()
  return digestText(`windshare/workspace-checkpoint-set/v1\n${intent.digest}\n${entries.join('\n')}`)
}

async function operationDigest(intent: ReceiveIntent, purpose: string): Promise<string> {
  return digestText(`windshare/ui-operation-receipt/v1\n${intent.digest}\n${purpose}`)
}

async function digestText(value: string): Promise<string> {
  const bytes = new TextEncoder().encode(value)
  return encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)))
}

async function quotaAvailability(
  storage: OriginPrivateStorageManager,
  signal: AbortSignal,
): Promise<bigint | null> {
  const estimate = await storage.estimate()
  signal.throwIfAborted()
  if (!Number.isSafeInteger(estimate.quota) || !Number.isSafeInteger(estimate.usage) ||
      estimate.quota === undefined || estimate.usage === undefined) return null
  return BigInt(Math.max(0, estimate.quota - estimate.usage))
}

function workspaceEnvironmentOffer(
  quotaAvailabilityEstimateBytes: bigint | null,
): WorkspaceEnvironmentOffer {
  return Object.freeze({
    id: WORKSPACE_ENVIRONMENT_OFFER_ID,
    kind: 'origin-private-workspace',
    persistence: 'durable-owned-repository',
    jobHardLimitBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
    processHardLimitBytes: DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
    minimumQuotaReserveBytes: MINIMUM_OPFS_QUOTA_RESERVE,
    quotaAvailabilityEstimateBytes,
  })
}

function portableEnvironmentOffer(): PortableEnvironmentOffer {
  return Object.freeze({
    id: PORTABLE_ENVIRONMENT_OFFER_ID,
    kind: 'portable-memory',
    persistence: 'none',
    maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
    maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  })
}

function requireDirectoryArtifact(action: ArtifactAction): DirectoryTreeArtifact {
  if (action.artifact?.kind !== 'directory-tree') {
    throw new TypeError('FSA DirectTree action lacks a directory artifact')
  }
  return action.artifact
}

function requireWorkspaceAction(action: ArtifactAction): ArtifactAction & Readonly<{
  artifact: NonNullable<ArtifactAction['artifact']>
  plan: Extract<ArtifactAction['plan'], { kind: 'workspace-then-publish' }>
}> {
  if (action.plan.kind !== 'workspace-then-publish' || action.artifact === null ||
      action.artifact.kind === 'directory-tree') throw unavailableRoute()
  return action as ArtifactAction & Readonly<{
    artifact: NonNullable<ArtifactAction['artifact']>
    plan: Extract<ArtifactAction['plan'], { kind: 'workspace-then-publish' }>
  }>
}

function requirePortableAction(action: ArtifactAction): ArtifactAction & Readonly<{
  artifact: NonNullable<ArtifactAction['artifact']>
  plan: Extract<ArtifactAction['plan'], { kind: 'portable-handoff' }>
}> {
  if (action.plan.kind !== 'portable-handoff' || action.artifact === null ||
      action.artifact.kind === 'directory-tree') throw unavailableRoute()
  return action as ArtifactAction & Readonly<{
    artifact: NonNullable<ArtifactAction['artifact']>
    plan: Extract<ArtifactAction['plan'], { kind: 'portable-handoff' }>
  }>
}

function requireMatchingSingleFileAdmission(
  admitted: AdmittedWorkspaceContent,
  evidence: ExactSingleFileEvidence,
): void {
  const frozen = admitted.budget.evidence
  if (frozen.kind !== 'single-file' || frozen.fileId !== evidence.fileId ||
      frozen.containingDirectoryId !== evidence.containingDirectoryId ||
      frozen.generation !== evidence.generation ||
      frozen.catalogSize !== evidence.catalogSize) {
    throw new TypeError('Workspace continuation changed its admitted file evidence')
  }
}

function requirePreparation(
  preparation: SealedWorkspaceZipPreparationV1 | undefined,
): SealedWorkspaceZipPreparationV1 {
  if (preparation === undefined) throw new TypeError('Workspace ZIP lost sealed preparation')
  return preparation
}

function requireSameIntent(expected: ReceiveIntent, supplied: ReceiveIntent): void {
  if (expected.operationId !== supplied.operationId || expected.digest !== supplied.digest) {
    throw new TypeError('Receive authority escaped its frozen intent')
  }
}

function randomAuthorityReference(): string {
  const bytes = new Uint8Array(AUTHORITY_REFERENCE_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

function unavailableRoute(): DOMException {
  return new DOMException('No installed browser authority matches this action', 'NotSupportedError')
}

function isWorkspaceTerminal(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'expired' || state.kind === 'needs-attention' ||
    (state.kind === 'download-started' && state.attemptKind === 'portable')
}
