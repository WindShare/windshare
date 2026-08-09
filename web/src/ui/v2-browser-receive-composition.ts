import { IndexedDbReceiveResumeSource } from '../output/browser/indexeddb-resume-state'
import {
  probeBrowserEnvironment,
  startFSAParentPicker,
} from '../output/capability/acquisition'
import type { BrowserCapabilityRuntime } from '../output/capability/contract'
import { openOriginPrivateRetainedArtifactBackend } from '../output/origin-private/session'
import {
  probeBrowserHandoffCapabilities,
  type BrowserHandoffCapabilityRuntime,
} from '../output/portable/packaged-handoff'
import type {
  ArtifactAction,
  BrowserHandoffTargetOffer,
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
} from '../output/planning'
import {
  ReceiveOperationResumeAuthority,
  type ReceiveOperationMutationPort,
  type ReceiveOperationResumeRef,
  type ReceiveOperationResumeSource,
} from '../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
  ReopenedWorkspaceOperation,
} from '../output/resume/reopen-authority'
import type { WorkspaceStageTraceListener } from '../output/workspace/stages'
import type {
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
  V2RetainedReceiveAction,
  V2RetainedReceiveActionResult,
  V2RetainedReceiveInventory,
  V2RetainedReceiveOperation,
  V2StartedArtifactAuthority,
} from './v2-receive-runtime'
import type { BrowserReceiveWindow } from './browser-receive/contracts'
import { FSAReceiveOperation, StartedFSAReceive } from './browser-receive/fsa'
import { StartedPortableReceive } from './browser-receive/portable'
import {
  portableEnvironmentOffer,
  quotaAvailability,
  unavailableRoute,
  workspaceEnvironmentOffer,
} from './browser-receive/shared'
import {
  StartedWorkspaceReceive,
  WorkspaceReceiveOperation,
} from './browser-receive/workspace'
import { handoffRetainedWorkspacePackage } from './browser-receive/workspace-publication'

export type { BrowserReceiveWindow } from './browser-receive/contracts'

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
