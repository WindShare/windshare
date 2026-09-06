import { acquireArtifactReader, type ArtifactReaderLease } from './export-readers'
import type { PersistentHandleRepository } from '../persistence/journal'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  sealPackagedArtifact,
  type PackagedArtifactV1,
} from '../workspace/aggregate'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalText,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
import type { MaterializedManifestV1 } from '../workspace/manifest'
import {
  type ReceiveOperationHandleRecord,
} from '../workspace/records'
import {
  createOriginalFileArtifactVerificationReceipt,
  type OriginalFileArtifactVerificationReceiptV1,
} from '../workspace/receipts'
import type { ReceiveOperationRepository } from '../workspace/repository'
import { WORKSPACE_HANDLE_PACKAGE_OBJECT } from '../workspace/stages'
import {
  ORIGIN_PRIVATE_PACKAGE_CONTAINER,
  ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
  type OriginPrivateWorkspaceRoot,
} from './workspace-root'
import { originPrivateRawFileHandleId } from './workspace-tree'

const PACKAGE_HANDLE_DOMAIN = 'windshare/origin-private/package-handle/v1'

type PackageIdentityStage = 'writer-open' | 'commit' | 'cleanup'

export interface OriginPrivatePackageCleanupProofV1 {
  readonly schemaVersion: 1
  readonly operationId: string
  readonly packageOwnedObjectId: string
  readonly packageHandleId: string
  readonly result: 'removed' | 'already-absent'
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface PackagedArtifactReadPort {
  readPackagedArtifact(artifact: PackagedArtifactV1): Promise<Blob>
  acquireReader?(artifact: PackagedArtifactV1): Promise<ArtifactReaderLease>
}

/** Sealed artifact readers preserve the sole object's physical ownership. */
export class OriginPrivatePackageStore
implements PackagedArtifactReadPort {
  readonly #root: OriginPrivateWorkspaceRoot
  readonly #operationRepository: ReceiveOperationRepository
  readonly #checkpointHandles: PersistentHandleRepository

  constructor(input: {
    readonly root: OriginPrivateWorkspaceRoot
    readonly operationRepository: ReceiveOperationRepository
    readonly checkpointHandles: PersistentHandleRepository
  }) {
    this.#root = input.root
    this.#operationRepository = input.operationRepository
    this.#checkpointHandles = input.checkpointHandles
  }

  async readOwnedFile(ownedObjectId: string): Promise<Blob> {
    const objectId = snapshotIdentity(ownedObjectId, 32, 'raw owned object ID')
    let persisted
    try {
      persisted = await this.#checkpointHandles.readHandle(
        originPrivateRawFileHandleId(this.#root.operationId, objectId),
      )
    } catch (cause) {
      throw new TargetOwnershipUnknownError('commit', this.#root.operationId, { cause })
    }
    const persistedHandle = requireFileHandle(persisted?.handle, this.#root.operationId, 'commit')
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_RAW_FILE_CONTAINER,
      objectId,
      'commit',
    )
    if (persisted === undefined || current === undefined ||
        persisted.operationId !== this.#root.operationId ||
        persisted.authorityRef !== this.#root.authorityRef ||
        persisted.ownedObjectId !== objectId ||
        !await this.#root.sameObject(current, persistedHandle, 'commit')) {
      throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
    }
    return persistedHandle.getFile()
  }

  async promoteOriginalFile(input: {
    readonly receiveIntentDigest: string
    readonly sealedMaterializationDigest: string
    readonly artifactSpecDigest: string
    readonly manifest: MaterializedManifestV1
    readonly signal: AbortSignal
  }): Promise<OriginalFileArtifactVerificationReceiptV1> {
    const files = input.manifest.entries.filter((entry) => entry.kind === 'file')
    if (files.length !== 1 || input.manifest.entries.length !== 1) {
      throw new TypeError('original-file promotion requires exactly one materialized file')
    }
    const file = files[0]!
    const source = await this.readOwnedFile(file.ownedObjectId)
    if (BigInt(source.size) !== file.exactSize) {
      throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
    }
    input.signal.throwIfAborted()
    // The final checkpoint already sealed this object; promotion changes authority,
    // never storage identity or payload bytes. The export name belongs to the attempt.
    const actualBytes = BigInt(source.size)
    return createOriginalFileArtifactVerificationReceipt({
      operationId: this.#root.operationId,
      receiveIntentDigest: input.receiveIntentDigest,
      sealedMaterializationDigest: input.sealedMaterializationDigest,
      artifactSpecDigest: input.artifactSpecDigest,
      packageOwnedObjectId: file.ownedObjectId,
      exactBytes: actualBytes,
      finalCheckpointDigest: file.checkpoint.recordDigest,
      finalCheckpointGeneration: file.checkpoint.checkpointGeneration,
      promotionVerified: true,
    })
  }

  acquireReader(artifact: PackagedArtifactV1): Promise<ArtifactReaderLease> {
    if (artifact.operationId !== this.#root.operationId) {
      throw new TypeError('export reader escaped its operation')
    }
    return acquireArtifactReader(this.#root.operationId)
  }

  async readPackagedArtifact(artifact: PackagedArtifactV1): Promise<Blob> {
    const validated = await sealPackagedArtifact(artifact)
    if (validated.operationId !== this.#root.operationId ||
        validated.digest !== artifact.digest ||
        validated.packageOwnedObjectId !== artifact.packageOwnedObjectId) {
      throw new TypeError('packaged artifact escaped its origin-private operation')
    }
    if (validated.layoutDigest === validated.artifactSpecDigest) {
      const original = await this.readOwnedFile(validated.packageOwnedObjectId)
      if (BigInt(original.size) !== validated.exactBytes) {
        throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
      }
      return original
    }
    const handle = await this.#verifyPackageHandle(
      validated.packageOwnedObjectId,
      originPrivatePackageHandleId(this.#root.operationId, validated.packageOwnedObjectId),
      'commit',
    )
    const file = await handle.getFile()
    if (BigInt(file.size) !== validated.exactBytes) {
      throw new TargetOwnershipUnknownError('commit', this.#root.operationId)
    }
    return file
  }

  async cleanupPackage(
    ownedObjectId: string,
  ): Promise<OriginPrivatePackageCleanupProofV1> {
    const objectId = snapshotIdentity(ownedObjectId, 32, 'package owned object ID')
    const handleId = originPrivatePackageHandleId(this.#root.operationId, objectId)
    const persisted = await this.#readPackageHandle(handleId, 'cleanup')
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_PACKAGE_CONTAINER,
      objectId,
      'cleanup',
    )
    if (persisted === undefined) {
      if (current !== undefined) {
        throw new TargetOwnershipUnknownError('cleanup', this.#root.operationId)
      }
      return this.#cleanupProof(objectId, handleId, 'already-absent')
    }
    const handle = requireFileHandle(persisted.handle, this.#root.operationId, 'cleanup')
    this.#assertPackageHandleRecord(persisted, objectId, 'cleanup')
    if (current === undefined) return this.#cleanupProof(objectId, handleId, 'already-absent')
    if (!await this.#root.sameObject(current, handle, 'cleanup')) {
      throw new TargetOwnershipUnknownError('cleanup', this.#root.operationId)
    }
    const result = await this.#root.removeObject(ORIGIN_PRIVATE_PACKAGE_CONTAINER, objectId, handle)
    return this.#cleanupProof(objectId, handleId, result)
  }

  async #cleanupProof(
    objectId: string,
    handleId: string,
    result: OriginPrivatePackageCleanupProofV1['result'],
  ): Promise<OriginPrivatePackageCleanupProofV1> {
    const canonicalBytes = canonicalRecord('windshare/package-cleanup-proof/v1', 1, [
      canonicalFrame(canonicalIdentity(this.#root.operationId, 16, 'operation ID')),
      canonicalFrame(canonicalIdentity(objectId, 32, 'package owned object ID')),
      canonicalFrame(canonicalText(handleId)),
      canonicalText(result),
    ])
    return Object.freeze({
      schemaVersion: 1,
      operationId: this.#root.operationId,
      packageOwnedObjectId: objectId,
      packageHandleId: handleId,
      result,
      canonicalBytes,
      digest: await canonicalDigest(canonicalBytes),
    })
  }

  async #verifyPackageHandle(
    ownedObjectId: string,
    handleId: string,
    stage: PackageIdentityStage,
  ): Promise<FileSystemFileHandle> {
    const persisted = await this.#readPackageHandle(handleId, stage)
    const persistedHandle = requireFileHandle(persisted?.handle, this.#root.operationId, stage)
    const current = await this.#root.readObject(
      ORIGIN_PRIVATE_PACKAGE_CONTAINER,
      ownedObjectId,
      stage,
    )
    if (persisted === undefined || current === undefined) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId)
    }
    this.#assertPackageHandleRecord(persisted, ownedObjectId, stage)
    if (!await this.#root.sameObject(current, persistedHandle, stage)) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId)
    }
    return persistedHandle
  }

  async #readPackageHandle(
    handleId: string,
    stage: PackageIdentityStage,
  ): Promise<ReceiveOperationHandleRecord<FileSystemFileHandle> | undefined> {
    try {
      return await this.#operationRepository.readHandle<FileSystemFileHandle>(handleId)
    } catch (cause) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId, { cause })
    }
  }

  #assertPackageHandleRecord(
    persisted: ReceiveOperationHandleRecord,
    ownedObjectId: string,
    stage: PackageIdentityStage,
  ): void {
    if (persisted.kind !== WORKSPACE_HANDLE_PACKAGE_OBJECT ||
        persisted.operationId !== this.#root.operationId ||
        persisted.authorityRef !== this.#root.authorityRef ||
        persisted.ownedObjectId !== ownedObjectId) {
      throw new TargetOwnershipUnknownError(stage, this.#root.operationId)
    }
  }
}

export function originPrivatePackageHandleId(
  operationId: string,
  ownedObjectId: string,
): string {
  return `${PACKAGE_HANDLE_DOMAIN}/${operationId}/${ownedObjectId}`
}

function requireFileHandle(
  value: unknown,
  operationId: string,
  stage: PackageIdentityStage,
): FileSystemFileHandle {
  if (typeof value !== 'object' || value === null ||
      !('kind' in value) || value.kind !== 'file' ||
      !('isSameEntry' in value) || typeof value.isSameEntry !== 'function') {
    throw new TargetOwnershipUnknownError(stage, operationId)
  }
  return value as FileSystemFileHandle
}
