import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import { FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE } from '../persistence/checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  sameDurableCheckpointNamespace,
} from '../persistence/namespace'
import type {
  PersistentMaterializationPort,
  PersistentTreeTrace,
} from '../persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import type { ReceiveOperationRepository } from '../workspace/repository'
import type { WorkspaceContentGate } from '../workspace/stages'
import type { OriginPrivateWorkspaceBudgetClaim } from './admission'
import { OriginPrivateWorkspaceCleanupPort } from './cleanup-port'
import type { OriginPrivateWorkspaceNamespace } from './namespace'
import { OriginPrivatePackageStore } from './package-store'
import { OriginPrivateWorkspaceRoot } from './workspace-root'
import { OriginPrivateWorkspaceTree } from './workspace-tree'
import {
  OriginPrivateMaterializationSession,
  OriginPrivatePackageContinuationBackendSession,
  OriginPrivateRetainedArtifactBackendSession,
  OriginPrivateWorkspaceBackendSession,
  closeOwnedCheckpointAfterFailure,
  createOriginPrivateFinalCheckpointReader,
  type OriginPrivateCheckpointStore,
  type OriginPrivateDurableCheckpointStore,
  type OriginPrivatePackageContinuationBackend,
  type OriginPrivateRetainedArtifactBackend,
  type OriginPrivateWorkspaceBackend,
} from './backend-sessions'

export type {
  OriginPrivateCheckpointStore,
  OriginPrivateDurableCheckpointStore,
  OriginPrivatePackageContinuationBackend,
  OriginPrivateRetainedArtifactBackend,
  OriginPrivateWorkspaceBackend,
} from './backend-sessions'

export interface OriginPrivateMaterializationOptions {
  readonly receiveIntent: ReceiveIntent
  readonly operationRepository: ReceiveOperationRepository
  readonly workspaceRootHandleId: string
  readonly workspaceRootHandle: FileSystemDirectoryHandle
  readonly contentGate: WorkspaceContentGate
  readonly budgetClaim: OriginPrivateWorkspaceBudgetClaim
  readonly checkpointStore?: OriginPrivateCheckpointStore
  readonly checkpointDatabaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly onTrace?: PersistentTreeTrace
}

function recordCheckpointFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.checkpoint, error)
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent('checkpoint', {
      backend: 'origin_private',
      transition: 'failed',
    }))
}

/**
 * Opens only after durable admission. The returned surface deliberately omits OPFS and
 * checkpoint repositories so callers cannot bypass the shared file transaction reducer.
 */
export async function openOriginPrivateWorkspaceMaterialization(
  options: OriginPrivateMaterializationOptions,
): Promise<PersistentMaterializationPort> {
  const intent = await validateReceiveIntent(options.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree') {
    throw new TypeError('origin-private materialization requires a workspace receive intent')
  }
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
  const ownsStore = options.checkpointStore === undefined
  const checkpoints = options.checkpointStore ?? await IndexedDbFileCheckpointRepository.open(
    binding,
    options.checkpointDatabaseName,
  ).catch((error: unknown) => {
    recordCheckpointFailure(options.diagnostics, error)
    throw error
  })
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    const error = new TypeError('origin-private checkpoint store escaped the receive operation')
    recordCheckpointFailure(options.diagnostics, error)
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Origin-private checkpoint binding failed and the store did not close',
      )
    }
    throw error
  }
  try {
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBindingDigest: intent.plan.workspace.digest,
      authorityRef: intent.plan.workspace.repositoryRef,
      workspaceRootHandleId: options.workspaceRootHandleId,
      workspaceRootHandle: options.workspaceRootHandle,
      repository: options.operationRepository,
      contentGate: options.contentGate,
      budgetClaim: options.budgetClaim,
    })
    const session = await PersistentTreeOutputSession.open({
      tree: new OriginPrivateWorkspaceTree({ root, handles: checkpoints }),
      checkpoints,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(options.onTrace === undefined ? {} : { trace: options.onTrace }),
    })
    return new OriginPrivateMaterializationSession(
      session,
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
  } catch (error) {
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Origin-private materialization failed and the checkpoint store did not close',
        { cause: error },
      )
    }
    throw error
  }
}

/** Production composition keeps checkpoint authority alive through seal, package, and cleanup. */
export async function openOriginPrivateWorkspaceBackend(options: {
  readonly receiveIntent: ReceiveIntent
  readonly operationRepository: ReceiveOperationRepository
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly contentGate: WorkspaceContentGate
  readonly budgetClaim: OriginPrivateWorkspaceBudgetClaim
  readonly checkpointStore?: OriginPrivateDurableCheckpointStore
  readonly checkpointDatabaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly onTrace?: PersistentTreeTrace
}): Promise<OriginPrivateWorkspaceBackend> {
  const intent = await validateReceiveIntent(options.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree' ||
      options.namespace.operationId !== intent.operationId ||
      options.namespace.authorityRef !== intent.plan.workspace.repositoryRef) {
    throw new TypeError('origin-private backend escaped its workspace receive intent')
  }
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
  const ownsStore = options.checkpointStore === undefined
  const checkpoints = options.checkpointStore ?? await IndexedDbFileCheckpointRepository.open(
    binding,
    options.checkpointDatabaseName,
  ).catch((error: unknown) => {
    recordCheckpointFailure(options.diagnostics, error)
    throw error
  })
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    const error = new TypeError('origin-private checkpoint store escaped the receive operation')
    recordCheckpointFailure(options.diagnostics, error)
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Origin-private checkpoint binding failed and the store did not close',
      )
    }
    throw error
  }
  try {
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBindingDigest: intent.plan.workspace.digest,
      authorityRef: intent.plan.workspace.repositoryRef,
      workspaceRootHandleId: options.namespace.rootHandleId,
      workspaceRootHandle: options.namespace.root,
      repository: options.operationRepository,
      contentGate: options.contentGate,
      budgetClaim: options.budgetClaim,
    })
    const treeSession = await PersistentTreeOutputSession.open({
      tree: new OriginPrivateWorkspaceTree({ root, handles: checkpoints }),
      checkpoints,
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(options.onTrace === undefined ? {} : { trace: options.onTrace }),
    })
    const materialization = new OriginPrivateMaterializationSession(
      treeSession,
      checkpoints,
      false,
      options.diagnostics,
    )
    const packages = new OriginPrivatePackageStore({
      root,
      operationRepository: options.operationRepository,
      checkpointHandles: checkpoints,
    })
    const cleanup = new OriginPrivateWorkspaceCleanupPort({
      root,
      namespace: options.namespace,
      operationRepository: options.operationRepository,
      checkpoints,
      packages,
    })
    const finalCheckpoints = createOriginPrivateFinalCheckpointReader(checkpoints)
    return new OriginPrivateWorkspaceBackendSession({
      materialization,
      packages,
      finalCheckpoints,
      cleanup,
      checkpoints,
      ownsStore,
      ...(options.diagnostics === undefined ? {} : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Origin-private output reservation failed and the checkpoint store did not close',
        { cause: error },
      )
    }
    throw error
  }
}

/** Reopens a sealed package without restoring any authority to mutate raw materialization. */
export async function openOriginPrivateRetainedArtifactBackend(options: {
  readonly receiveIntent: ReceiveIntent
  readonly operationRepository: ReceiveOperationRepository
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly checkpointStore?: OriginPrivateDurableCheckpointStore
  readonly checkpointDatabaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
}): Promise<OriginPrivateRetainedArtifactBackend> {
  const intent = await validateReceiveIntent(options.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree' ||
      options.namespace.operationId !== intent.operationId ||
      options.namespace.authorityRef !== intent.plan.workspace.repositoryRef) {
    throw new TypeError('retained origin-private artifact escaped its workspace receive intent')
  }
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
  const ownsStore = options.checkpointStore === undefined
  const checkpoints = options.checkpointStore ?? await IndexedDbFileCheckpointRepository.open(
    binding,
    options.checkpointDatabaseName,
  ).catch((error: unknown) => {
    recordCheckpointFailure(options.diagnostics, error)
    throw error
  })
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    const error = new TypeError('retained package checkpoint store escaped the receive operation')
    recordCheckpointFailure(options.diagnostics, error)
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Retained checkpoint binding failed and the store did not close',
      )
    }
    throw error
  }
  try {
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBindingDigest: intent.plan.workspace.digest,
      authorityRef: intent.plan.workspace.repositoryRef,
      workspaceRootHandleId: options.namespace.rootHandleId,
      workspaceRootHandle: options.namespace.root,
      repository: options.operationRepository,
    })
    await root.authorize()
    const packages = new OriginPrivatePackageStore({
      root,
      operationRepository: options.operationRepository,
      checkpointHandles: checkpoints,
    })
    const cleanup = new OriginPrivateWorkspaceCleanupPort({
      root,
      namespace: options.namespace,
      operationRepository: options.operationRepository,
      checkpoints,
      packages,
    })
    return new OriginPrivateRetainedArtifactBackendSession({
      packages,
      cleanup,
      checkpoints,
      ownsStore,
      ...(options.diagnostics === undefined ? {} : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Retained artifact reopen failed and the checkpoint store did not close',
        { cause: error },
      )
    }
    throw error
  }
}

/** Reopens only the sealed read/package surface; no caller can mutate or re-receive raw content. */
export async function openOriginPrivatePackageContinuationBackend(options: {
  readonly receiveIntent: ReceiveIntent
  readonly operationRepository: ReceiveOperationRepository
  readonly namespace: OriginPrivateWorkspaceNamespace
  readonly contentGate: WorkspaceContentGate
  readonly budgetClaim: OriginPrivateWorkspaceBudgetClaim
  readonly checkpointStore?: OriginPrivateDurableCheckpointStore
  readonly checkpointDatabaseName?: string
  readonly diagnostics?: OutputDiagnosticsPorts
}): Promise<OriginPrivatePackageContinuationBackend> {
  const intent = await validateReceiveIntent(options.receiveIntent)
  if (intent.plan.kind !== 'workspace-then-publish' || intent.artifact.kind === 'directory-tree' ||
      options.namespace.operationId !== intent.operationId ||
      options.namespace.authorityRef !== intent.plan.workspace.repositoryRef) {
    throw new TypeError('package continuation escaped its workspace receive intent')
  }
  const binding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.workspace.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: intent.plan.workspace.repositoryRef,
  })
  const ownsStore = options.checkpointStore === undefined
  const checkpoints = options.checkpointStore ?? await IndexedDbFileCheckpointRepository.open(
    binding,
    options.checkpointDatabaseName,
  ).catch((error: unknown) => {
    recordCheckpointFailure(options.diagnostics, error)
    throw error
  })
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    const error = new TypeError('package continuation checkpoint store escaped the receive operation')
    recordCheckpointFailure(options.diagnostics, error)
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Package continuation checkpoint binding failed and the store did not close',
      )
    }
    throw error
  }
  try {
    const root = new OriginPrivateWorkspaceRoot({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      workspaceBindingDigest: intent.plan.workspace.digest,
      authorityRef: intent.plan.workspace.repositoryRef,
      workspaceRootHandleId: options.namespace.rootHandleId,
      workspaceRootHandle: options.namespace.root,
      repository: options.operationRepository,
      contentGate: options.contentGate,
      budgetClaim: options.budgetClaim,
    })
    await root.authorize()
    const tree = new OriginPrivateWorkspaceTree({ root, handles: checkpoints })
    const packages = new OriginPrivatePackageStore({
      root,
      operationRepository: options.operationRepository,
      checkpointHandles: checkpoints,
    })
    return new OriginPrivatePackageContinuationBackendSession({
      operationId: intent.operationId,
      tree,
      packages,
      checkpoints,
      ownsStore,
      ...(options.diagnostics === undefined ? {} : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    const cleanupFailure = closeOwnedCheckpointAfterFailure(
      checkpoints,
      ownsStore,
      options.diagnostics,
    )
    if (cleanupFailure !== undefined) {
      throw new AggregateError(
        [error, cleanupFailure],
        'Package continuation reopen failed and the checkpoint store did not close',
        { cause: error },
      )
    }
    throw error
  }
}

export type {
  PersistentFileRequest,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
} from '../persistent-tree/contracts'