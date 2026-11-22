import {
  compareRevisionMetadata,
  type RevisionMetadataComparison,
  type RevisionSourceEvidence,
} from '../../content/revision-comparison'
import {
  CheckpointLineageDecisionError,
  CheckpointSourceEvidenceUnavailableError,
  SourceRevisionChangedError,
} from '../persistent-tree/errors'

export interface CompletedShortCircuitInput {
  readonly requireRevisionOpen?: boolean
  readonly requestFileId: string | undefined
  readonly requestExpectedSize: bigint | undefined
  readonly retainedFileId: string
  readonly retainedExactSize: bigint
  readonly locallyComplete: boolean
  readonly hasBlockingState?: boolean
  readonly coordinateMatches: boolean
}

/**
 * Revisit locally complete files without reopening remote handles only while
 * the requested coordinate and catalog size still agree with retained authority.
 */
export function canShortCircuitCompletedFile(input: CompletedShortCircuitInput): boolean {
  if (input.requireRevisionOpen === true || input.requestFileId === undefined) return false
  if (!input.locallyComplete || input.hasBlockingState === true || !input.coordinateMatches) return false
  if (input.requestFileId !== input.retainedFileId) return false
  return input.requestExpectedSize === undefined || input.requestExpectedSize === input.retainedExactSize
}

export interface PersistedSourceAdmissionInput {
  readonly previous: RevisionSourceEvidence
  readonly current: RevisionSourceEvidence
  readonly allowsMetadataReuse: boolean
  readonly contextMatches?: boolean
}

export type PersistedSourceAdmission =
  | Readonly<{ kind: 'same-revision' }>
  | Readonly<{
      kind: 'metadata-reuse'
      comparison: Extract<RevisionMetadataComparison, { kind: 'metadata-agreement' }>
    }>
  | Readonly<{
      kind: 'source-changed'
      reason: 'context-changed' | 'size-changed' | 'metadata-changed'
      comparison?: Extract<RevisionMetadataComparison, { kind: 'observed-change' }>
    }>
  | Readonly<{
      kind: 'evidence-unavailable'
      comparison: Extract<RevisionMetadataComparison, { kind: 'unavailable' }>
    }>
  | Readonly<{ kind: 'reuse-state-blocked' }>
  | Readonly<{
      kind: 'invalid-same-revision'
      comparison: Extract<RevisionMetadataComparison, { kind: 'observed-change' }>
    }>

export type PersistedSourceAdmissionRejection = Exclude<
  PersistedSourceAdmission,
  { readonly kind: 'same-revision' | 'metadata-reuse' }
>

/**
 * Applies one canonical precedence order across PersistentTree and ProgressiveZip:
 * observed source changes win over state blocks, state blocks
 * win over missing comparison evidence, and same-revision divergence stays invalid.
 */
export function evaluatePersistedSourceAdmission(
  input: PersistedSourceAdmissionInput,
): PersistedSourceAdmission {
  if (input.contextMatches === false) {
    return Object.freeze({ kind: 'source-changed', reason: 'context-changed' })
  }
  const comparison = compareRevisionMetadata(input.previous, input.current)
  const sameRevision = input.previous.fileRevision === input.current.fileRevision
  if (comparison.kind === 'observed-change') {
    if (sameRevision) return Object.freeze({ kind: 'invalid-same-revision', comparison })
    return Object.freeze({ kind: 'source-changed', reason: comparison.reason, comparison })
  }
  if (!sameRevision && !input.allowsMetadataReuse) {
    return Object.freeze({ kind: 'reuse-state-blocked' })
  }
  if (comparison.kind === 'unavailable') {
    return Object.freeze({ kind: 'evidence-unavailable', comparison })
  }
  return sameRevision
    ? Object.freeze({ kind: 'same-revision' })
    : Object.freeze({ kind: 'metadata-reuse', comparison })
}

/** Maps persisted admission rejections onto the same FaultDomain as live transfers. */
export function persistedSourceAdmissionError(
  outcome: PersistedSourceAdmissionRejection,
  options?: ErrorOptions,
): Error {
  switch (outcome.kind) {
    case 'source-changed':
      return new SourceRevisionChangedError(options)
    case 'evidence-unavailable':
      return new CheckpointSourceEvidenceUnavailableError()
    case 'reuse-state-blocked':
      return new CheckpointLineageDecisionError('reuse-state-blocked')
    case 'invalid-same-revision':
      return new CheckpointLineageDecisionError('invalid')
  }
}
