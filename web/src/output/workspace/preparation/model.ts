import type { CanonicalModifiedTime } from '../../../transfer/directory-admission'
import type { SealedZipLayoutPlanV1 } from '../../zip-layout/layout'
import type { CanonicalBytes } from '../canonical'
import type { AuthenticatedGenerationReference } from '../manifest'
import type { ManifestPageRecord } from '../records'
import type { PreparationAdmissionReason } from '../state'

export const DEFAULT_PREPARATION_METADATA_LIMIT = 268_435_456n
export const DEFAULT_PORTABLE_PREPARATION_METADATA_LIMIT = 16_777_216n
export const PREPARATION_SCHEMA_VERSION = 1 as const
export const MAX_ARTIFACT_ENTRIES = 1_000_000

export type PreparationDirectoryRole =
  | 'result-root'
  | 'necessary-ancestor'
  | 'explicitly-selected-empty'

interface PreparationEntryBase {
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly modifiedTime?: CanonicalModifiedTime
}

export interface PreparationDirectoryEntry extends PreparationEntryBase {
  readonly kind: 'directory'
  readonly directoryId: string
  readonly generation: string
  readonly role: PreparationDirectoryRole
}

export interface PreparationFileEntry extends PreparationEntryBase {
  readonly kind: 'file'
  readonly fileId: string
  readonly containingDirectoryId: string
  readonly generation: string
  readonly exactSize: bigint
}

export type PreparationManifestEntry = PreparationDirectoryEntry | PreparationFileEntry

export interface PreparationManifestV1 {
  readonly schemaVersion: typeof PREPARATION_SCHEMA_VERSION
  readonly operationId: string
  readonly preparationId: string
  readonly receiveIntentDigest: string
  readonly artifactSpecDigest: string
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly PreparationManifestEntry[]
  readonly entryCount: bigint
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly selectedRawBytes: bigint
  readonly canonicalMetadataBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface SealedWorkspaceZipPreparationV1 {
  readonly manifest: PreparationManifestV1
  readonly pages: readonly ManifestPageRecord[]
  readonly zipLayout: SealedZipLayoutPlanV1
  readonly zipLayoutCanonicalBytes: CanonicalBytes
}

export class PreparationManifestError extends TypeError {
  readonly reason: PreparationAdmissionReason

  constructor(reason: PreparationAdmissionReason, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PreparationManifestError'
    this.reason = reason
  }
}
