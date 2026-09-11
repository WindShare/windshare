import type { DirectZipRollbackCandidateV1 } from '../../../../output/direct-zip/journal'
import type { DirectZipWriterCheckpointV1 } from '../../../../output/direct-zip/writer'
import { requireCheckpointShape } from '../../../../output/direct-zip/writer/checkpoint-state'
import type { BrowserDirectZipPageAuthority } from './pages'

export interface BrowserDirectZipMemberRollback {
  readonly candidate: DirectZipRollbackCandidateV1
  readonly checkpoint: DirectZipWriterCheckpointV1
  readonly authority: BrowserDirectZipPageAuthority
}

/** The saved member boundary, rather than new source metadata, owns the retained ZIP prefix. */
export function memberRollbackCheckpoint(
  previous: DirectZipWriterCheckpointV1,
): DirectZipWriterCheckpointV1 {
  const rollback = previous.member?.rollback
  if (previous.phase !== 'inside-member' || rollback === undefined) {
    throw new TypeError('Direct ZIP rollback requires a committed active member')
  }
  const checkpoint: DirectZipWriterCheckpointV1 = Object.freeze({
    version: 1, operationId: previous.operationId, intentDigest: previous.intentDigest,
    generation: previous.generation + 1n, phase: 'between-members',
    nextEntryOrdinal: rollback.nextEntryOrdinal,
    archiveOffset: rollback.archiveOffset, committedLength: rollback.archiveOffset,
    safeResumeBytes: rollback.safeResumeBytes, epochRoot: rollback.epochRoot,
    targetObservationDigest: previous.targetObservationDigest, pages: rollback.pages,
  })
  requireCheckpointShape(checkpoint)
  return checkpoint
}
