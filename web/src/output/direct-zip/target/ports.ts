import type { DirectZipCanonicalBytes } from '../format/canonical'
import type {
  DirectZipEvidenceComparison,
  DirectZipFileBinding,
  DirectZipParentBinding,
  DirectZipPermissionState,
  DirectZipReservationCandidate,
  DirectZipReservationCandidateDraft,
  DirectZipReservationRetirementReason,
} from './model'

export interface DirectZipFileSnapshotPort {
  readonly size: bigint
  readonly lastModified: number
  read(start: bigint, end: bigint): Promise<Uint8Array>
}

export interface DirectZipWritablePort {
  write(position: bigint, bytes: Uint8Array): Promise<void>
  truncate(size: bigint): Promise<void>
  close(): Promise<void>
  abort(reason?: unknown): Promise<void>
}

export type DirectZipExactNameLookup<FileHandle> =
  | Readonly<{ readonly kind: 'absent' }>
  | Readonly<{ readonly kind: 'occupied-non-file' }>
  | Readonly<{ readonly kind: 'file'; readonly handle: FileHandle }>

/** The filesystem seam is intentionally weaker than a conditional filesystem API. */
export interface DirectZipFileSystemPort<ParentHandle, FileHandle> {
  queryPermission(parent: ParentHandle): Promise<DirectZipPermissionState>
  requestPermission(parent: ParentHandle): Promise<DirectZipPermissionState>
  lookupExactName(
    parent: ParentHandle,
    stableName: string,
  ): Promise<DirectZipExactNameLookup<FileHandle>>
  createFile(parent: ParentHandle, stableName: string): Promise<FileHandle>
  snapshot(file: FileHandle): Promise<DirectZipFileSnapshotPort>
  createWritable(file: FileHandle, keepExistingData: true | false): Promise<DirectZipWritablePort>
  removeExactName(parent: ParentHandle, stableName: string): Promise<void>
}

export interface DirectZipHandleBindingPort<ParentHandle, FileHandle> {
  compareParent(
    binding: DirectZipParentBinding<ParentHandle>,
    currentParent: ParentHandle,
  ): Promise<DirectZipEvidenceComparison>
  compareFile(
    binding: DirectZipFileBinding<FileHandle>,
    currentFile: FileHandle,
  ): Promise<DirectZipEvidenceComparison>
  compareCurrentFiles(
    left: FileHandle,
    right: FileHandle,
  ): Promise<DirectZipEvidenceComparison>
  bindFile(input: Readonly<{
    readonly operationId: DirectZipCanonicalBytes
    readonly targetRef: DirectZipCanonicalBytes
    readonly stableName: string
    readonly parentBinding: DirectZipParentBinding<ParentHandle>
    readonly file: FileHandle
  }>): Promise<DirectZipFileBinding<FileHandle>>
}

export interface DirectZipReservationCandidatePort<ParentHandle> {
  persistCandidate(
    draft: DirectZipReservationCandidateDraft<ParentHandle>,
    lease: DirectZipOperationLeaseEvidence,
  ): Promise<Readonly<{
    readonly targetRef: Uint8Array
    readonly bindingDigest: Uint8Array
  }>>
  retireCandidate(
    candidate: DirectZipReservationCandidate<ParentHandle>,
    reason: DirectZipReservationRetirementReason,
    lease: DirectZipOperationLeaseEvidence,
  ): Promise<void>
}

export interface DirectZipOperationLeaseEvidence {
  readonly leaseId: string
  readonly generation: bigint
}

export interface DirectZipOperationLease extends DirectZipOperationLeaseEvidence {
  release(): Promise<void>
}

export interface DirectZipParentLock {
  readonly name: string
  release(): Promise<void>
}

export interface DirectZipOperationLeasePort {
  acquire(operationId: DirectZipCanonicalBytes): Promise<DirectZipOperationLease>
}

export interface DirectZipParentLockPort<ParentHandle> {
  acquire(parent: ParentHandle): Promise<DirectZipParentLock>
}

export interface DirectZipTargetRandomPort {
  bytes(length: number): Uint8Array
}
