import {
  INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, openIndexedDbCheckpointDatabase, transactionCompletion,
} from '../../../src/output/browser/indexeddb-database'
import {
  createDirectZipCheckpointProposalV1, createDirectZipRollbackCandidateV1, createDirectZipTargetObservationV1,
  type DirectZipRollbackCandidateV1,
} from '../../../src/output/direct-zip/journal'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { createMemberRollbackFixture, readMemberRollbackState } from './member-rollback-fixture'
import { interruptMemberRollback } from './member-rollback-faults'
import { observeProductionDirectZipFileSystem } from './production-fsa-observation'

export type MemberRollbackCandidateTamper = 'earlier-completed-boundary' | 'target-binding' | 'target-observation'
type Fixture = Awaited<ReturnType<typeof createMemberRollbackFixture>>

export async function probeProductionMemberRollbackCandidateTamper(
  databaseName: string, mode: MemberRollbackCandidateTamper,
) {
  const fixture = await createMemberRollbackFixture(databaseName, 'changed-content')
  try {
    await interruptMemberRollback(fixture, 'before-truncate')
    const pending = await readMemberRollbackState(databaseName, fixture.intent.operationId)
    if (pending.candidate?.kind !== 'rollback') throw new Error('Rollback intent disappeared before corruption')
    const forged = await forgeCandidate(fixture, pending.candidate, mode)
    const database = await openIndexedDbCheckpointDatabase(databaseName)
    try {
      const transaction = database.transaction(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE, 'readwrite')
      transaction.objectStore(INDEXEDDB_DIRECT_ZIP_CANDIDATE_STORE).put(forged)
      await transactionCompletion(transaction)
    } finally { database.close() }
    // Canonical encoding alone cannot authorize truncation: even valid old proof
    // pages belong to a different member boundary than this pending rollback.
    const canonical = await readMemberRollbackState(databaseName, fixture.intent.operationId)
    const mutations = observeProductionDirectZipFileSystem()
    let rejected = false
    let openedExecution = false
    let mutationCounts
    try {
      const active = await fixture.resume()
      await active.plans.openDirectResumableZip(fixture.intent, new AbortController().signal)
      openedExecution = true
    } catch { rejected = true }
    finally {
      mutationCounts = mutations.snapshot()
      mutations.restore()
    }
    const retained = await readMemberRollbackState(databaseName, fixture.intent.operationId)
    const after = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
    return {
      rejected, openedExecution, mutationCounts, canonicalCandidateDigest: canonical.candidate?.digest,
      forgedCandidateDigest: forged.digest, retainedCandidateDigest: retained.candidate?.digest,
      checkpointDigest: retained.checkpoint.digest, expectedCheckpointDigest: fixture.paused.digest,
      expectedBytes: Array.from(fixture.original), after: Array.from(after),
      forgedOrdinal: forged.proposedCheckpoint.entryOrdinal.toString(),
      forgedOffset: Number(forged.proposedCheckpoint.archiveOffset), activeMemberOffset: fixture.rollbackOffset,
      resumedRanges: fixture.source.ranges.filter(range => range.phase === 'resumed'),
    }
  } finally { await fixture.close() }
}

async function forgeCandidate(
  fixture: Fixture, candidate: DirectZipRollbackCandidateV1, mode: MemberRollbackCandidateTamper,
) {
  if (mode === 'earlier-completed-boundary') {
    const proposedCheckpoint = await createDirectZipCheckpointProposalV1({
      ...fixture.initial, generation: fixture.paused.generation + 1n,
      predecessorCheckpointDigest: fixture.paused.digest, discovery: fixture.paused.discovery,
    })
    return createDirectZipRollbackCandidateV1({ ...candidate, proposedCheckpoint })
  }
  if (mode === 'target-binding') {
    const proposedCheckpoint = await createDirectZipCheckpointProposalV1({
      ...candidate.proposedCheckpoint, targetBindingDigest: encodeBase64Url(new Uint8Array(32).fill(99)),
    })
    return createDirectZipRollbackCandidateV1({ ...candidate, proposedCheckpoint })
  }
  const predecessorTargetObservation = await createDirectZipTargetObservationV1({
    ...candidate.predecessorTargetObservation,
    lastModifiedMilliseconds: candidate.predecessorTargetObservation.lastModifiedMilliseconds + 1,
  })
  return createDirectZipRollbackCandidateV1({ ...candidate, predecessorTargetObservation })
}
