import { describe, expect, it } from 'vitest'
import { decodeBase64Url, encodeBase64Url } from '../../../../src/crypto/bytes'
import { chainDirectZipEpochDigestV1 } from '../../../../src/output/direct-zip/format'
import {
  createDirectZipCheckpointProposalV1, createDirectZipCheckpointV1,
  validateDirectZipCheckpointProposalV1,
} from '../../../../src/output/direct-zip/journal/records'
import {
  assertDirectZipRollbackPredecessorV1, createDirectZipRollbackCandidateV1,
  validateDirectZipRollbackCandidateV1,
} from '../../../../src/output/direct-zip/journal/rollback'
import { assertCandidateCheckpointFence, assertCandidateFence } from '../../../../src/output/direct-zip/journal/indexeddb/authority'
import type { DirectZipCheckpointProposalV1, DirectZipCheckpointV1 } from '../../../../src/output/direct-zip/journal/model'
import { rollbackCheckpointFixture } from './rollback-fixture'

describe('Direct ZIP durable member rollback intent', () => {
  it('binds the shorter completed prefix without changing discovery, ownership, or page authority', async () => {
    const checkpoint = await rollbackCheckpointFixture()
    const proposal = await rollbackProposal(checkpoint)
    const candidate = await rollbackCandidate(checkpoint, proposal)
    expect(() => assertDirectZipRollbackPredecessorV1(checkpoint, candidate)).not.toThrow()
    expect(await validateDirectZipRollbackCandidateV1(candidate)).toEqual(candidate)
    expect(proposal.phase).toBe('between-members')
    expect(proposal.committedArchiveLength).toBe(100n)
    expect(proposal.committedSelectedPayloadBytes).toBe(0n)
    expect(proposal.centralPages).toEqual(checkpoint.centralPages)
    expect(proposal.layoutPages).toEqual(checkpoint.currentMember!.rollback.layoutPages)

    for (const changes of [
      { receiveIntentDigest: identity(32, 51) },
      { parentBindingDigest: identity(32, 52) },
      { fileBindingDigest: identity(32, 53) },
      { committedSelectedPayloadBytes: 1n },
      { discovery: { ...proposal.discovery, discoveryRootDigest: identity(32, 54) } },
      { layoutPages: { ...proposal.layoutPages, rootDigest: identity(32, 55) } },
    ]) {
      const altered = await createDirectZipCheckpointProposalV1({ ...proposal, ...changes })
      await expect((async () => assertDirectZipRollbackPredecessorV1(
        checkpoint, await rollbackCandidate(checkpoint, altered),
      ))()).rejects.toThrow()
    }
  })

  it('preserves a truncated terminal epoch proof and rejects a missing or forged prefix proof', async () => {
    const checkpoint = await rollbackCheckpointFixture()
    const rollback = checkpoint.currentMember!.rollback
    const epochStart = 80n
    const contentDigest = identity(32, 61)
    const epochRootDigest = encodeBase64Url(chainDirectZipEpochDigestV1({
      predecessorRoot: decodeBase64Url(rollback.predecessorEpochRootDigest)!,
      start: epochStart, end: rollback.archiveOffset, contentDigest: decodeBase64Url(contentDigest)!,
    }))
    const partial = await createDirectZipCheckpointV1({
      ...checkpoint,
      currentMember: {
        ...checkpoint.currentMember!,
        rollback: { ...rollback, epochStart, epochContentDigest: contentDigest, epochRootDigest },
      },
    })
    const proof = {
      start: epochStart, end: rollback.archiveOffset,
      predecessorRootDigest: rollback.predecessorEpochRootDigest, contentDigest, epochRootDigest,
    }
    const proposal = await rollbackProposal(partial, proof)
    const candidate = await rollbackCandidate(partial, proposal)
    expect(() => assertDirectZipRollbackPredecessorV1(partial, candidate)).not.toThrow()
    const withoutProof = await rollbackProposal(partial)
    const unproved = await rollbackCandidate(partial, withoutProof)
    expect(() => assertDirectZipRollbackPredecessorV1(partial, unproved)).toThrow('active-member boundary')
    await expect(createDirectZipCheckpointProposalV1({
      ...proposal, retainedEpochProof: { ...proof, start: epochStart + 1n },
    })).rejects.toThrow('retained epoch proof')
    await expect(validateDirectZipCheckpointProposalV1({
      ...proposal, retainedEpochProof: { ...proof, contentDigest: identity(32, 62) },
    })).rejects.toThrow('retained epoch proof')
  })

  it('keeps creation provenance immutable while the current checkpoint fence authorizes recovery', async () => {
    const checkpoint = await rollbackCheckpointFixture()
    const candidate = await rollbackCandidate(checkpoint, await rollbackProposal(checkpoint))
    const acquiredFence = {
      operationId: checkpoint.operationId, leaseId: identity(16, 71),
      checkpointGeneration: checkpoint.generation,
    }
    expect(() => assertCandidateCheckpointFence(candidate, acquiredFence)).not.toThrow()
    expect(() => assertCandidateFence(candidate, acquiredFence)).toThrow('admission lease')
    expect(() => assertCandidateCheckpointFence(candidate, {
      ...acquiredFence, checkpointGeneration: checkpoint.generation + 1n,
    })).toThrow('checkpoint fence')
    await expect(validateDirectZipRollbackCandidateV1({
      ...candidate, leaseId: acquiredFence.leaseId,
    })).rejects.toThrow('canonical')
  })
})

function rollbackProposal(
  checkpoint: DirectZipCheckpointV1,
  retainedEpochProof?: DirectZipCheckpointV1['retainedEpochProof'],
): Promise<DirectZipCheckpointProposalV1> {
  const rollback = checkpoint.currentMember!.rollback
  const { currentMember, ...base } = checkpoint
  if (currentMember === undefined) throw new TypeError('fixture requires an active member')
  return createDirectZipCheckpointProposalV1({
    ...base, generation: checkpoint.generation + 1n, predecessorCheckpointDigest: checkpoint.digest,
    phase: 'between-members', entryOrdinal: rollback.entryOrdinal,
    archiveOffset: rollback.archiveOffset, committedArchiveLength: rollback.archiveOffset,
    committedSelectedPayloadBytes: rollback.safeSelectedPayloadBytes,
    epochRootDigest: rollback.epochRootDigest,
    layoutPages: rollback.layoutPages, centralPages: rollback.centralPages, epochPages: rollback.epochPages,
    journalUsage: rollback.journalUsage,
    ...(rollback.accountingTailPageId === undefined ? {} : { accountingTailPageId: rollback.accountingTailPageId }),
    ...(retainedEpochProof === undefined ? {} : { retainedEpochProof }),
  })
}

function rollbackCandidate(checkpoint: DirectZipCheckpointV1, proposedCheckpoint: DirectZipCheckpointProposalV1) {
  return createDirectZipRollbackCandidateV1({
    operationId: checkpoint.operationId, candidateId: identity(16, 41), leaseId: identity(16, 42),
    predecessorCheckpointGeneration: checkpoint.generation, predecessorCheckpointDigest: checkpoint.digest,
    predecessorTargetObservation: checkpoint.targetObservation, proposedCheckpoint,
  })
}

function identity(width: number, fill: number): string { return encodeBase64Url(new Uint8Array(width).fill(fill)) }
