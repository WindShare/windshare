import { compareRevisionMetadata, type RevisionSourceEvidence } from '../../content/revision-comparison'
import { newFileCheckpointV2, validateFileCheckpointTransition } from './checkpoint-codec'
import { FILE_CHECKPOINT_COMMIT_CANDIDATE } from './checkpoint-lifecycle'
import { fileCheckpointIsComplete, fileCheckpointIsFinalized, type FileCheckpointV2 } from './checkpoint-model'
import type { FileCheckpointJournal, SemanticFileCheckpointJournal } from './journal'
import { persistedSourceAdmissionError } from './source-admission'

/** The caller has already verified this checkpoint's local object and write authority. */
export async function admitCheckpointMetadataReuse(
  previous: FileCheckpointV2,
  current: RevisionSourceEvidence & { readonly fileId: string },
  journal: FileCheckpointJournal,
  semantic: SemanticFileCheckpointJournal | undefined,
): Promise<FileCheckpointV2> {
  if (previous.fileId !== current.fileId) throw new TypeError('Metadata reuse changed file context')
  const comparison = compareRevisionMetadata(previous, current)
  if (comparison.kind !== 'metadata-agreement') {
    throw new TypeError('Saved ranges lack current-source applicability')
  }
  if (previous.fileRevision === current.fileRevision) return previous
  // Semantic journals check final receipts atomically with the revision change. A
  // checkpoint-only journal cannot distinguish flushed bytes from a final delivery.
  if (semantic === undefined && previous.checkpointGeneration > 0n && fileCheckpointIsComplete(previous)) {
    throw persistedSourceAdmissionError({ kind: 'reuse-state-blocked' })
  }
  if (fileCheckpointIsFinalized(previous)) {
    throw new DOMException('metadata reuse cannot replace a historical delivery', 'InvalidStateError')
  }
  const next = newFileCheckpointV2({
    ...previous, fileRevision: current.fileRevision, reusedFrom: previous.fileRevision,
    stateGeneration: previous.stateGeneration + 1n,
    checkpointGeneration: previous.checkpointGeneration + 1n,
  })
  validateFileCheckpointTransition(previous, next)
  if (semantic !== undefined) {
    await semantic.commitDurableCut(previous, next)
  } else {
    const candidate = newFileCheckpointV2({ ...next, commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE })
    await journal.stageCheckpointUpdate(previous, candidate)
    await journal.commitCheckpointCandidate(candidate, next)
  }
  return next
}
