import type { FileRevisionDescriptor } from './records'
import { decodeBase64Url, encodeBase64Url, equalBytes } from '../crypto/bytes'

const SOURCE_METADATA_BYTES = 32

export interface RevisionSourceEvidence {
  readonly fileRevision: string
  readonly exactSize: bigint
  readonly sourceMetadata?: string
}

export type RevisionComparison =
  | Readonly<{ kind: 'metadata-agreement'; reason: 'same-revision' | 'metadata-agreement' }>
  | Readonly<{ kind: 'byte-agreement'; reason: 'saved-ranges-compared' }>
  | Readonly<{ kind: 'observed-change'; reason: 'context-changed' }>
  | Readonly<{ kind: 'observed-change'; reason: 'size-changed' | 'metadata-changed' }>
  | Readonly<{ kind: 'unavailable'; reason: 'metadata-unavailable' }>

/** Callers bind share/file/task independently before consulting source applicability. */
export function compareRevisionMetadata(
  previous: RevisionSourceEvidence, current: RevisionSourceEvidence,
): Exclude<RevisionComparison, { reason: 'context-changed' }> {
  if (previous.exactSize !== current.exactSize) return { kind: 'observed-change', reason: 'size-changed' }
  sourceMetadataFields(previous)
  sourceMetadataFields(current)
  if (previous.sourceMetadata !== undefined && current.sourceMetadata !== undefined &&
      previous.sourceMetadata !== current.sourceMetadata) {
    return { kind: 'observed-change', reason: 'metadata-changed' }
  }
  if (previous.fileRevision === current.fileRevision) return { kind: 'metadata-agreement', reason: 'same-revision' }
  if (previous.sourceMetadata === undefined || current.sourceMetadata === undefined) {
    return { kind: 'unavailable', reason: 'metadata-unavailable' }
  }
  return { kind: 'metadata-agreement', reason: 'metadata-agreement' }
}

/** An opaque provider token is comparison evidence, never a digest of file bytes. */
export function sourceMetadataFields(input: { readonly sourceMetadata?: string }): { readonly sourceMetadata?: string } {
  const value = input.sourceMetadata
  if (value === undefined) return {}
  const bytes = decodeBase64Url(value)
  if (bytes === undefined || bytes.length !== SOURCE_METADATA_BYTES || bytes.every(byte => byte === 0) ||
      encodeBase64Url(bytes) !== value) {
    throw new TypeError('Source metadata must be a canonical nonzero 32-byte token')
  }
  return { sourceMetadata: value }
}

export function compareRevisionDescriptors(previous: FileRevisionDescriptor, current: FileRevisionDescriptor): RevisionComparison {
  if (!equalBytes(previous.shareInstance, current.shareInstance) || !equalBytes(previous.fileId, current.fileId) ||
      previous.geometry.blockSize !== current.geometry.blockSize) {
    return { kind: 'observed-change', reason: 'context-changed' }
  }
  const before = previous.modifiedTime
  const after = current.modifiedTime
  // Newly available evidence is not evidence that the source itself changed.
  if (before !== undefined && after !== undefined &&
      (before.seconds !== after.seconds || before.nanoseconds !== after.nanoseconds || before.precision !== after.precision)) {
    return { kind: 'observed-change', reason: 'metadata-changed' }
  }
  return compareRevisionMetadata(
    { ...previous, fileRevision: previous.fileRevisionText }, { ...current, fileRevision: current.fileRevisionText },
  )
}
