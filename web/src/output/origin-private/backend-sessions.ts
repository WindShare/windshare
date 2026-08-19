import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type {
  FileCheckpointJournal,
  FinalFileCheckpointProof,
  PersistentHandleRepository,
} from '../persistence/journal'
import { finalFileCheckpointProof } from '../persistence/journal'
import type {
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
} from '../persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { FileCheckpointRecoveryRepository } from '../persistent-tree/recovery'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import type {
  FinalCheckpointReader,
  FinalCheckpointRecoveryEvidence,
  FinalCheckpointRecoveryReader,
  MaterializedManifestV1,
} from '../workspace/manifest'
import type { PackageTemporaryCleanupReceiptV1 } from '../workspace/receipts'
import type { OriginPrivateWorkspaceCleanupAuthority } from './cleanup-port'
import {
  OriginPrivatePackageStore,
  type PackagedArtifactReadPort,
} from './package-store'
import { OriginPrivateWorkspaceTree } from './workspace-tree'

export interface OriginPrivateCheckpointStore
extends FileCheckpointJournal, PersistentHandleRepository,
  Pick<FileCheckpointRecoveryRepository, 'resolveCandidate'> {
  close?(): void
}

export interface OriginPrivateDurableCheckpointStore extends OriginPrivateCheckpointStore {
  retireOperation(): Promise<void>
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

export class OriginPrivateMaterializationSession implements PersistentMaterializationPort {
  readonly #session: PersistentTreeOutputSession
  readonly #checkpoints: OriginPrivateCheckpointStore
  readonly #ownsStore: boolean
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #closed = false

  constructor(
    session: PersistentTreeOutputSession,
    checkpoints: OriginPrivateCheckpointStore,
    ownsStore: boolean,
    diagnostics?: OutputDiagnosticsPorts,
  ) {
    this.#session = session
    this.#checkpoints = checkpoints
    this.#ownsStore = ownsStore
    this.#diagnostics = diagnostics
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
    const failures: unknown[] = []
    try {
      await this.#session.close()
    } catch (error) {
      failures.push(error)
    }
    if (this.#ownsStore) {
      try {
        this.#checkpoints.close?.()
      } catch (error) {
        failures.push(error)
        recordCheckpointCleanupFailure(this.#diagnostics, error)
      }
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) {
      throw new AggregateError(failures, 'Origin-private materialization did not close cleanly')
    }
  }
}

export class OriginPrivateWorkspaceBackendSession implements OriginPrivateWorkspaceBackend {
  readonly materialization: PersistentMaterializationPort
  readonly packages: OriginPrivatePackageStore
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly finalCheckpoints: FinalCheckpointReader
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  readonly #checkpoints: OriginPrivateDurableCheckpointStore
  readonly #ownsStore: boolean
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #closed = false

  constructor(input: {
    readonly materialization: PersistentMaterializationPort
    readonly packages: OriginPrivatePackageStore
    readonly finalCheckpoints: FinalCheckpointReader
    readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
    readonly checkpoints: OriginPrivateDurableCheckpointStore
    readonly ownsStore: boolean
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.materialization = input.materialization
    this.packages = input.packages
    this.packagedArtifacts = input.packages
    this.finalCheckpoints = input.finalCheckpoints
    this.cleanup = input.cleanup
    this.#checkpoints = input.checkpoints
    this.#ownsStore = input.ownsStore
    this.#diagnostics = input.diagnostics
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    const failures: unknown[] = []
    try {
      await this.materialization.close()
    } catch (error) {
      failures.push(error)
    }
    if (this.#ownsStore) {
      try {
        this.#checkpoints.close?.()
      } catch (error) {
        failures.push(error)
        recordCheckpointCleanupFailure(this.#diagnostics, error)
      }
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) {
      throw new AggregateError(failures, 'Origin-private output resources did not close')
    }
  }
}

export class OriginPrivateRetainedArtifactBackendSession
implements OriginPrivateRetainedArtifactBackend {
  readonly packagedArtifacts: PackagedArtifactReadPort
  readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
  readonly close: () => Promise<void>

  constructor(input: {
    readonly packages: OriginPrivatePackageStore
    readonly cleanup: OriginPrivateWorkspaceCleanupAuthority
    readonly checkpoints: OriginPrivateDurableCheckpointStore
    readonly ownsStore: boolean
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.packagedArtifacts = input.packages
    this.cleanup = input.cleanup
    this.close = checkpointStoreCloser(
      input.checkpoints,
      input.ownsStore,
      input.diagnostics,
    )
  }
}

export class OriginPrivatePackageContinuationBackendSession
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
    readonly diagnostics?: OutputDiagnosticsPorts
  }) {
    this.#operationId = input.operationId
    this.#tree = input.tree
    this.packages = input.packages
    this.close = checkpointStoreCloser(
      input.checkpoints,
      input.ownsStore,
      input.diagnostics,
    )
    this.finalCheckpoints = Object.freeze({
      readFinalCheckpoint: (recordId: string, checkpointGeneration: bigint) =>
        readFinalCheckpoint(input.checkpoints, recordId, checkpointGeneration),
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

export function createOriginPrivateFinalCheckpointReader(
  checkpoints: OriginPrivateDurableCheckpointStore,
): FinalCheckpointReader {
  return Object.freeze({
    readFinalCheckpoint: (recordId: string, checkpointGeneration: bigint) =>
      readFinalCheckpoint(checkpoints, recordId, checkpointGeneration),
  })
}

export function closeOwnedCheckpointAfterFailure(
  checkpoints: OriginPrivateCheckpointStore,
  ownsStore: boolean,
  diagnostics: OutputDiagnosticsPorts | undefined,
): unknown | undefined {
  if (!ownsStore) return undefined
  try {
    checkpoints.close?.()
    return undefined
  } catch (error) {
    recordCheckpointCleanupFailure(diagnostics, error)
    return error
  }
}

function checkpointStoreCloser(
  checkpoints: OriginPrivateDurableCheckpointStore,
  ownsStore: boolean,
  diagnostics?: OutputDiagnosticsPorts,
): () => Promise<void> {
  let closed = false
  return async () => {
    if (closed) return
    closed = true
    if (!ownsStore) return
    try {
      checkpoints.close?.()
    } catch (error) {
      recordCheckpointCleanupFailure(diagnostics, error)
      throw error
    }
  }
}

async function readFinalCheckpoint(
  checkpoints: OriginPrivateDurableCheckpointStore,
  recordId: string,
  checkpointGeneration: bigint,
): Promise<FinalFileCheckpointProof | undefined> {
  try {
    return await checkpoints.finalCheckpointProof(recordId, checkpointGeneration)
  } catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return undefined
    throw error
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

function recordCheckpointCleanupFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.cleanup, error)
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent('cleanup', {
      backend: 'origin_private',
      transition: 'failed',
    }))
}
