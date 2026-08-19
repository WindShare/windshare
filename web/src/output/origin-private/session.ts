import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { IndexedDbFileCheckpointRepository } from '../browser/indexeddb-repository'
import { FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE } from '../persistence/checkpoint'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
  PersistentHandleRepository,
} from '../persistence/journal'
import { finalFileCheckpointProof } from '../persistence/journal'
import {
  durableCheckpointNamespaceIdentity,
  sameDurableCheckpointNamespace,
} from '../persistence/namespace'
import type {
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
  PersistentTreeTrace,
} from '../persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import type { FileCheckpointRecoveryRepository } from '../persistent-tree/recovery'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { ReceiveOperationRepository } from '../workspace/repository'
import type { WorkspaceContentGate } from '../workspace/stages'
import type {
  FinalCheckpointReader,
  FinalCheckpointRecoveryEvidence,
  FinalCheckpointRecoveryReader,
  MaterializedManifestV1,
} from '../workspace/manifest'
import type { PackageTemporaryCleanupReceiptV1 } from '../workspace/receipts'
import type { OriginPrivateWorkspaceBudgetClaim } from './admission'
import {
  OriginPrivateWorkspaceCleanupPort,
  type OriginPrivateWorkspaceCleanupAuthority,
} from './cleanup-port'
import type { OriginPrivateWorkspaceNamespace } from './namespace'
import {
  OriginPrivatePackageStore,
  type PackagedArtifactReadPort,
} from './package-store'
import { OriginPrivateWorkspaceRoot } from './workspace-root'
import { OriginPrivateWorkspaceTree } from './workspace-tree'

export interface OriginPrivateCheckpointStore
extends FileCheckpointJournal, PersistentHandleRepository,
  Pick<FileCheckpointRecoveryRepository, 'resolveCandidate'> {
  close?(): void
}

export interface OriginPrivateDurableCheckpointStore extends OriginPrivateCheckpointStore {
  retireOperation(): Promise<void>
}

export interface OriginPrivateMaterializationOptions {
  readonly receiveIntent: ReceiveIntent
  readonly operationRepository: ReceiveOperationRepository
  readonly workspaceRootHandleId: string
  readonly workspaceRootHandle: FileSystemDirectoryHandle
  readonly contentGate: WorkspaceContentGate
  readonly budgetClaim: OriginPrivateWorkspaceBudgetClaim
  readonly checkpointStore?: OriginPrivateCheckpointStore
  readonly checkpointDatabaseName?: string
  readonly onTrace?: PersistentTreeTrace
}

export interface OriginPrivateWorkspaceBackend {
  readonly materialization: PersistentMaterializationPort
  readonly packages: OriginPrivatePackageStore
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly finalCheckpoints: FinalCheckpointReader
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  close(): Promise<void>
}

export interface OriginPrivateRetainedArtifactBackend {
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  close(): Promise<void>
}

export interface OriginPrivatePackageContinuationBackend {
  readonly packages: OriginPrivatePackageStore
  readonly finalCheckpoints: FinalCheckpointRecoveryReader
  verifyManifestOwnership(manifest: MaterializedManifestV1): Promise<void>
  verifyTemporaryCleanup(receipt: PackageTemporaryCleanupReceiptV1): Promise<void>
  close(): Promise<void>
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
  )
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    if (ownsStore) checkpoints.close?.()
    throw new TypeError('origin-private checkpoint store escaped the receive operation')
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
      ...(options.onTrace === undefined ? {} : { trace: options.onTrace }),
    })
    return new OriginPrivateMaterializationSession(session, checkpoints, ownsStore)
  } catch (error) {
    if (ownsStore) checkpoints.close?.()
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
  )
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    if (ownsStore) checkpoints.close?.()
    throw new TypeError('origin-private checkpoint store escaped the receive operation')
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
      ...(options.onTrace === undefined ? {} : { trace: options.onTrace }),
    })
    const materialization = new OriginPrivateMaterializationSession(
      treeSession,
      checkpoints,
      false,
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
    const finalCheckpoints: FinalCheckpointReader = Object.freeze({
      readFinalCheckpoint: async (recordId: string, checkpointGeneration: bigint) => {
        try {
          return await checkpoints.finalCheckpointProof(recordId, checkpointGeneration)
        } catch (error) {
          if (error instanceof DOMException && error.name === 'NotFoundError') return undefined
          throw error
        }
      },
    })
    return new OriginPrivateWorkspaceBackendSession({
      materialization,
      packages,
      finalCheckpoints,
      cleanup,
      checkpoints,
      ownsStore,
    })
  } catch (error) {
    if (ownsStore) checkpoints.close?.()
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
  )
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    if (ownsStore) checkpoints.close?.()
    throw new TypeError('retained package checkpoint store escaped the receive operation')
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
    })
  } catch (error) {
    if (ownsStore) checkpoints.close?.()
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
  )
  if (!sameDurableCheckpointNamespace(checkpoints.binding, binding)) {
    if (ownsStore) checkpoints.close?.()
    throw new TypeError('package continuation checkpoint store escaped the receive operation')
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
    })
  } catch (error) {
    if (ownsStore) checkpoints.close?.()
    throw error
  }
}

class OriginPrivateMaterializationSession implements PersistentMaterializationPort {
  readonly #session: PersistentTreeOutputSession
  readonly #checkpoints: OriginPrivateCheckpointStore
  readonly #ownsStore: boolean
  #closed = false

  constructor(
    session: PersistentTreeOutputSession,
    checkpoints: OriginPrivateCheckpointStore,
    ownsStore: boolean,
  ) {
    this.#session = session
    this.#checkpoints = checkpoints
    this.#ownsStore = ownsStore
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    return this.#session.beginFile(request)
  }

  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    return this.#session.ensureDirectory(path)
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    try {
      await this.#session.close()
    } finally {
      if (this.#ownsStore) this.#checkpoints.close?.()
    }
  }
}

class OriginPrivateWorkspaceBackendSession implements OriginPrivateWorkspaceBackend {
  readonly materialization: PersistentMaterializationPort
  readonly packages: OriginPrivatePackageStore
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly finalCheckpoints: FinalCheckpointReader
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  readonly #checkpoints: OriginPrivateDurableCheckpointStore
  readonly #ownsStore: boolean
  #closed = false

  constructor(input: {
    readonly materialization: PersistentMaterializationPort
    readonly packages: OriginPrivatePackageStore
    readonly finalCheckpoints: FinalCheckpointReader
    readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
    readonly checkpoints: OriginPrivateDurableCheckpointStore
    readonly ownsStore: boolean
  }) {
    this.materialization = input.materialization
    this.packages = input.packages
    this.packagedArtifacts = input.packages
    this.finalCheckpoints = input.finalCheckpoints
    this.cleanup = input.cleanup
    this.#checkpoints = input.checkpoints
    this.#ownsStore = input.ownsStore
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    try {
      await this.materialization.close()
    } finally {
      if (this.#ownsStore) this.#checkpoints.close?.()
    }
  }
}

class OriginPrivateRetainedArtifactBackendSession implements OriginPrivateRetainedArtifactBackend {
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  readonly close: () => Promise<void>

  constructor(input: {
    readonly packages: OriginPrivatePackageStore
    readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
    readonly checkpoints: OriginPrivateDurableCheckpointStore
    readonly ownsStore: boolean
  }) {
    this.packagedArtifacts = input.packages
    this.cleanup = input.cleanup
    this.close = checkpointStoreCloser(input.checkpoints, input.ownsStore)
  }
}

class OriginPrivatePackageContinuationBackendSession
implements OriginPrivatePackageContinuationBackend {
  readonly packages: OriginPrivatePackageStore
  readonly finalCheckpoints: FinalCheckpointRecoveryReader
  readonly #operationId: string
  readonly #tree: OriginPrivateWorkspaceTree
  readonly close: () => Promise<void>

  constructor(input: {
    readonly operationId: string
    readonly tree: OriginPrivateWorkspaceTree
    readonly packages: OriginPrivatePackageStore
    readonly checkpoints: OriginPrivateDurableCheckpointStore
    readonly ownsStore: boolean
  }) {
    this.#operationId = input.operationId
    this.#tree = input.tree
    this.packages = input.packages
    this.close = checkpointStoreCloser(input.checkpoints, input.ownsStore)
    this.finalCheckpoints = Object.freeze({
      readFinalCheckpoint: async (recordId: string, checkpointGeneration: bigint) => {
        try {
          return await input.checkpoints.finalCheckpointProof(recordId, checkpointGeneration)
        } catch (error) {
          if (error instanceof DOMException && error.name === 'NotFoundError') return undefined
          throw error
        }
      },
      recoverFinalCheckpoint: (evidence: FinalCheckpointRecoveryEvidence) =>
        recoverFinalCheckpoint(input.checkpoints, evidence),
    })
  }

  async verifyManifestOwnership(manifest: MaterializedManifestV1): Promise<void> {
    if (manifest.operationId !== this.#operationId) {
      throw new TypeError('materialized manifest escaped the package continuation')
    }
    for (const entry of manifest.entries) {
      if (entry.kind === 'directory') {
        if (!await this.#tree.validateDirectory(entry.artifactPath, entry.ownedObjectId)) {
          throw new TargetOwnershipUnknownError('commit', this.#operationId)
        }
        continue
      }
      const file = await this.packages.readOwnedFile(entry.ownedObjectId)
      if (BigInt(file.size) !== entry.exactSize) {
        throw new TargetOwnershipUnknownError('commit', this.#operationId)
      }
    }
  }

  async verifyTemporaryCleanup(receipt: PackageTemporaryCleanupReceiptV1): Promise<void> {
    if (receipt.operationId !== this.#operationId) {
      throw new TypeError('temporary package cleanup escaped the package continuation')
    }
    const current = await this.packages.cleanupPackage(receipt.packageOwnedObjectId)
    if (current.operationId !== receipt.operationId ||
        current.packageOwnedObjectId !== receipt.packageOwnedObjectId ||
        current.packageHandleId !== receipt.packageHandleId || current.result !== 'already-absent') {
      throw new TargetOwnershipUnknownError('cleanup', this.#operationId)
    }
  }

}

function checkpointStoreCloser(
  checkpoints: OriginPrivateDurableCheckpointStore,
  ownsStore: boolean,
): () => Promise<void> {
  let closed = false
  return async () => {
    if (closed) return
    closed = true
    if (ownsStore) checkpoints.close?.()
  }
}

async function recoverFinalCheckpoint(
  checkpoints: OriginPrivateDurableCheckpointStore,
  evidence: FinalCheckpointRecoveryEvidence,
): Promise<FinalFileCheckpointProof | undefined> {
  const matches: FinalFileCheckpointProof[] = []
  let cursor: string | undefined
  do {
    const page = await checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: evidence.fileId,
      ...(cursor === undefined ? {} : { cursor }),
    })
    for (const record of page.records) {
      let proof: FinalFileCheckpointProof
      try {
        proof = finalFileCheckpointProof(record)
      } catch {
        continue
      }
      if (proof.fileRevision === evidence.fileRevision &&
          proof.exactSize === evidence.exactSize && proof.ownedObjectId === evidence.ownedObjectId &&
          proof.recordDigest === evidence.recordDigest &&
          proof.checkpointGeneration === evidence.checkpointGeneration &&
          samePath(proof.canonicalPath, evidence.artifactPath)) {
        matches.push(proof)
      }
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)
  if (matches.length > 1) throw new TypeError('final checkpoint authority is ambiguous')
  return matches[0]
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

export type {
  PersistentFileRequest,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
} from '../persistent-tree/contracts'
