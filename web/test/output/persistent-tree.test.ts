import { describe, expect, it } from 'vitest'

import type {
  OutputDiagnosticsPorts,
  OutputFailureObservation,
  OutputTraceEvent,
} from '../../src/output/diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  classifyCheckpointLineage,
  deriveCheckpointLineageID,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  sameCheckpointLineageSpec,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  FILE_CHECKPOINT_BATCH_REQUEST_LIMIT,
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type FileCheckpointJournal,
  type InitialCheckpointCASResult,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type PersistentHandleRecord,
} from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import type {
  OpenedFileRevision,
  PersistentTreeTraceEvent,
  SemanticPersistentOutputJournal,
} from '../../src/output/persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import type { FileCheckpointCandidateObservation } from '../../src/output/persistent-tree/recovery'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { identity } from './planning/fixture'
import {
  MemoryTree,
  beginInitialClaim,
  deferred,
  seedOccupiedClaim,
} from './persistent-tree-file-fixture'

const FILE_ID = identity(21)
const FILE_REVISION = identity(22)
const NEXT_REVISION = identity(23)

describe('persistent DirectoryTree materialization port', () => {
  it('opens the authenticated revision before creating a visible file', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => {
        fixture.events.push('revision-opened')
        return revision(4n)
      },
    })

    expect(fixture.events).toEqual([
      'authorize',
      'prepare-root',
      'revision-opened',
      'create:report.bin',
    ])
    expect(fixture.tree.visible(['report.bin'])).toEqual(new Uint8Array())
    expect(transaction.verifiedRanges).toEqual([])
  })

  it('keeps prefix writes visible while checkpoint truth advances only after flush', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(6n),
    })
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))

    expect(fixture.tree.visible(['report.bin'])).toEqual(Uint8Array.of(1, 2, 3))
    expect(transaction.verifiedRanges).toEqual([])
    await expect(transaction.checkpoint()).resolves.toEqual([{ start: 0n, end: 3n }])
    expect(transaction.verifiedRanges).toEqual([{ start: 0n, end: 3n }])
  })

  it('keeps a natively accepted write pending when cancellation arrives before its return', async () => {
    const fixture = await materializationFixture()
    const write = fixture.tree.deferFileWrite(['cancelled.bin'])
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['cancelled.bin'],
      openRevision: async () => revision(2n),
    })
    const controller = new AbortController()

    const writing = transaction.writeRange(0n, Uint8Array.of(1, 2), controller.signal)
    await write.accepted
    controller.abort(new DOMException('cancel after native acceptance', 'AbortError'))
    write.release()

    const writeOutcome = await writing.then(() => 'accepted' as const, () => 'rejected' as const)
    const durable = await transaction.pause()
    expect({ writeOutcome, durable }).toEqual({
      writeOutcome: 'accepted',
      durable: [{ start: 0n, end: 2n }],
    })
  })

  it('reopens the same owned file after restart and completes from its persisted range', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(6n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2, 3))
    await first.pause()
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
    })
    const second = await reopened.beginFile({
      materializationRelativePath: ['report.bin'],
      recovery: preservingRecoveryPolicy(),
      openRevision: async () => revision(6n),
    })
    expect(second.ownedObjectId).toBe(first.ownedObjectId)
    expect(second.verifiedRanges).toEqual([{ start: 0n, end: 3n }])
    expect(fixture.events.filter((event) => event === 'create:report.bin')).toHaveLength(1)

    await second.writeRange(3n, Uint8Array.of(4, 5, 6))
    const proof = await second.commit()
    expect(proof.complete).toBe(true)
    await expect(second.commit()).resolves.toEqual(proof)
    await expect(second.writeRange(0n, Uint8Array.of(9))).rejects.toMatchObject({
      kind: 'output-state',
    })
    await expect(second.checkpoint()).rejects.toMatchObject({ kind: 'output-state' })
    expect(fixture.tree.visible(['report.bin'])).toEqual(Uint8Array.of(1, 2, 3, 4, 5, 6))
  })

  it('publishes a genuine zero-byte revision through the ordinary file transaction', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['empty.bin'],
      openRevision: async () => revision(0n),
    })

    const proof = await transaction.commit()
    expect(proof.exactSize).toBe(0n)
    expect(proof.complete).toBe(true)
    expect(transaction.verifiedRanges).toEqual([])
    expect(fixture.tree.visible(['empty.bin'])).toEqual(new Uint8Array())
  })

  it('uses one native final cut and one atomic final transaction for a small file', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['small.bin'],
      shareInstance: identity(24),
      outputSession: { backend: 'direct-tree-test', outputSessionId: identity(1) },
      openRevision: async () => revision(3n),
    })
    const file = fixture.tree.file(['small.bin'])
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await transaction.commit()

    expect(file.writerModes).toEqual(['truncate'])
    expect(file.flushCount).toBe(1)
    expect(file.sizeCount).toBe(1)
    expect(file.verificationCount('commit')).toBe(1)
    expect(fixture.checkpoints.commitCreatedFileCount).toBe(1)
    expect(fixture.checkpoints.commitFinalFileCount).toBe(1)
    expect(fixture.checkpoints.readCommittedCount).toBe(0)
    expect(fixture.checkpoints.finalCheckpointProofCount).toBe(0)
  })

  it('finalizes a zero-byte file without opening a writer', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['zero.bin'],
      openRevision: async () => revision(0n),
    })
    const file = fixture.tree.file(['zero.bin'])
    await transaction.commit()

    expect(file.writerOpenCount).toBe(0)
    expect(file.flushCount).toBe(0)
    expect(file.sizeCount).toBe(1)
    expect(file.verificationCount('commit')).toBe(1)
    expect(fixture.checkpoints.commitFinalFileCount).toBe(1)
  })

  it('advances one queued automatic checkpoint and reopens preserving the durable prefix', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['periodic.bin'],
      recovery: preservingRecoveryPolicy(),
      openRevision: async () => revision(6n),
    })
    const file = fixture.tree.file(['periodic.bin'])
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await expect(transaction.automaticCheckpoint(
      'pending-bytes',
      preservingRecoveryPolicy().costBudget,
    )).resolves.toMatchObject({ kind: 'advanced' })
    await transaction.writeRange(3n, Uint8Array.of(4, 5, 6))
    await transaction.commit()

    expect(file.writerModes).toEqual(['truncate', 'preserve'])
    expect(file.preflightCount).toBe(1)
    expect(file.flushCount).toBe(2)
  })

  it('declines an over-budget automatic checkpoint without closing its writer', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['declined.bin'],
      openRevision: async () => revision(4n),
    })
    const file = fixture.tree.file(['declined.bin'])
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await expect(transaction.automaticCheckpoint('pending-bytes', {
      maximumPrefixCopyBytes: 1n,
      maximumCumulativeWriteAmplificationBytes: 1n,
      maximumPeakTemporaryBytes: 1n,
    })).resolves.toMatchObject({ kind: 'declined', reason: 'prefix-copy-budget' })
    expect(file.flushCount).toBe(0)
    expect(file.writerOpenCount).toBe(1)
    await transaction.writeRange(2n, Uint8Array.of(3, 4))
    expect(file.writerOpenCount).toBe(1)
  })

  it('persists PAUSED, resumes ACTIVE, and restarts only after exact user authority', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['restart.bin'],
      openRevision: async () => revision(4n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2))
    await first.pause()
    expect(fixture.checkpoints.committed(FILE_ID).phase).toBe(FILE_CHECKPOINT_PHASE_PAUSED)
    expect(fixture.tree.visible(['restart.bin'])).toEqual(Uint8Array.of(1, 2))

    const restarted = await fixture.session.beginFile({
      materializationRelativePath: ['restart.bin'],
      recovery: {
        kind: 'restart-owned-file',
        expectedOwnedObjectId: first.ownedObjectId,
      },
      openRevision: async () => revision(4n),
    })
    expect(restarted.initialDurableRanges).toEqual([])
    expect(fixture.checkpoints.committed(FILE_ID).phase).toBe(FILE_CHECKPOINT_PHASE_ACTIVE)
    // The metadata CAS cannot erase bytes; truncate occurs only when the authorized writer opens.
    expect(fixture.tree.visible(['restart.bin'])).toEqual(Uint8Array.of(1, 2))
    await restarted.writeRange(0n, Uint8Array.of(5, 6, 7, 8))
    expect(fixture.tree.file(['restart.bin']).writerModes.at(-1)).toBe('truncate')
  })

  it('requires typed temporary-space confirmation before non-empty preserving recovery', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['preserve.bin'],
      openRevision: async () => revision(4n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2))
    await first.pause()
    const file = fixture.tree.file(['preserve.bin'])
    file.requiresSpaceConfirmation = true

    const unconfirmed = await fixture.session.beginFile({
      materializationRelativePath: ['preserve.bin'],
      recovery: preservingRecoveryPolicy(),
      openRevision: async () => revision(4n),
    })
    await expect(unconfirmed.writeRange(2n, Uint8Array.of(3, 4))).rejects.toMatchObject({
      name: 'PersistentRecoveryPreflightError',
      reason: 'space-confirmation-required',
    })
    await unconfirmed.retire()
    const confirmations: bigint[] = []
    const confirmed = await fixture.session.beginFile({
      materializationRelativePath: ['preserve.bin'],
      recovery: {
        ...preservingRecoveryPolicy(),
        confirmTemporarySpace: (preflight) => {
          confirmations.push(preflight.cost.peakTemporaryBytes)
          return true
        },
      },
      openRevision: async () => revision(4n),
    })
    await confirmed.writeRange(2n, Uint8Array.of(3, 4))
    expect(confirmations).toEqual([2n])
    expect(file.writerModes.at(-1)).toBe('preserve')
  })

  it('replays the final transaction after a pre-commit crash and redownloads from zero', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['pre-final.bin'],
      openRevision: async () => revision(3n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2, 3))
    fixture.checkpoints.failNextFinalCommit('before')
    await expect(first.commit()).rejects.toThrow('pre-final-transaction')
    await first.retire()
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
    })
    const retry = await reopened.beginFile({
      materializationRelativePath: ['pre-final.bin'],
      openRevision: async () => revision(3n),
    })
    expect(retry.initialDurableRanges).toEqual([])
    await retry.writeRange(0n, Uint8Array.of(4, 5, 6))
    expect(fixture.tree.file(['pre-final.bin']).writerModes).toEqual(['truncate', 'truncate'])
  })

  it('recovers an ambiguously committed final without reopening a writer or redownloading', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['post-final.bin'],
      openRevision: async () => revision(3n),
    })
    const file = fixture.tree.file(['post-final.bin'])
    await first.writeRange(0n, Uint8Array.of(1, 2, 3))
    fixture.checkpoints.failNextFinalCommit('after')
    await expect(first.commit()).rejects.toThrow('ambiguous final')
    await first.retire()
    await fixture.session.close()
    const writerOpenCount = file.writerOpenCount

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
    })
    const retry = await reopened.beginFile({
      materializationRelativePath: ['post-final.bin'],
      openRevision: async () => revision(3n),
    })
    expect(retry.initialDurableRanges).toEqual([{ start: 0n, end: 3n }])
    await retry.commit()
    expect(file.writerOpenCount).toBe(writerOpenCount)
    expect(fixture.checkpoints.commitFinalFileCount).toBe(1)
  })

  it('keeps writer-close failure non-durable and aborts before a zero-range retry', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['close-crash.bin'],
      openRevision: async () => revision(2n),
    })
    const file = fixture.tree.file(['close-crash.bin'])
    await first.writeRange(0n, Uint8Array.of(1, 2))
    file.failNextFlush()
    await expect(first.commit()).rejects.toThrow('writer close failure')
    await first.retire()
    expect(file.abortCount).toBe(1)
    expect(fixture.checkpoints.committed(FILE_ID).verifiedRanges).toEqual([])
  })

  it('bounds ready lineage claims without delaying the first file on complete discovery', async () => {
    const fixture = await materializationFixture()
    const transactions = await Promise.all(Array.from({ length: 100 }, (_, index) =>
      fixture.session.beginFile({
        materializationRelativePath: [`batched-${index}.bin`],
        openRevision: async () => ({ ...revision(0n), fileId: identity(80 + index) }),
      })))
    expect(Math.max(...fixture.checkpoints.lineageBatchSizes)).toBeLessThanOrEqual(64)
    expect(fixture.checkpoints.lineageBatchSizes.length).toBeLessThan(100)
    await Promise.all(transactions.map(transaction => transaction.retire()))
  })

})

describe('persistent initial claim inspection coordination', () => {

  it('starts the first discovered file and bounds different-parent claim inspections at two', async () => {
    const fixture = await materializationFixture(undefined, 2)
    const gate = fixture.tree.deferFileInspection(['gate', 'first.bin'])
    const first = beginInitialClaim(fixture.session, ['gate', 'first.bin'],
      { ...revision(0n), fileId: identity(110) })
    await gate.started
    const paths = [['a', '1.bin'], ['b', '2.bin'], ['c', '3.bin']] as const
    const inspections = paths.map(path => fixture.tree.deferFileInspection(path))
    const later = paths.map((path, index) => beginInitialClaim(fixture.session, path,
      { ...revision(0n), fileId: identity(111 + index) }))
    await Promise.resolve()
    gate.resolve()
    const firstTransaction = await first
    await Promise.all([inspections[0]!.started, inspections[1]!.started])
    let thirdStarted = false
    inspections[2]!.started.then(() => { thirdStarted = true }).catch(() => undefined)
    await Promise.resolve()
    expect({ thirdStarted, active: fixture.tree.activeInspections }).toEqual({
      thirdStarted: false,
      active: 2,
    })
    inspections[0]!.resolve()
    await inspections[2]!.started
    expect(fixture.tree.peakActiveInspections).toBe(2)
    inspections[1]!.resolve()
    inspections[2]!.resolve()
    const transactions = await Promise.all(later)
    expect(fixture.checkpoints.installedClaimBatches.at(-1)).toEqual(paths.map(path => path.join('/')))
    await Promise.all([firstTransaction, ...transactions].map(transaction => transaction.retire()))
  })

  it('keeps same-parent claim inspections serialized inside the bounded group', async () => {
    const fixture = await materializationFixture(undefined, 2)
    const gate = fixture.tree.deferFileInspection(['gate', 'same-parent.bin'])
    const first = beginInitialClaim(fixture.session, ['gate', 'same-parent.bin'],
      { ...revision(0n), fileId: identity(120) })
    await gate.started
    const left = fixture.tree.deferFileInspection(['same', 'left.bin'])
    const right = fixture.tree.deferFileInspection(['same', 'right.bin'])
    const claims = [left, right].map((_inspection, index) => beginInitialClaim(fixture.session,
      ['same', index === 0 ? 'left.bin' : 'right.bin'],
      { ...revision(0n), fileId: identity(121 + index) }))
    await Promise.resolve()
    gate.resolve()
    await left.started
    let rightStarted = false
    right.started.then(() => { rightStarted = true }).catch(() => undefined)
    await Promise.resolve()
    expect({ rightStarted, active: fixture.tree.activeInspections }).toEqual({
      rightStarted: false,
      active: 1,
    })
    left.resolve()
    await right.started
    right.resolve()
    const transactions = await Promise.all([first, ...claims])
    expect(fixture.tree.inspectionStarts.slice(-2)).toEqual(['same/left.bin', 'same/right.bin'])
    await Promise.all(transactions.map(transaction => transaction.retire()))
  })

  it('drains active inspection siblings before rejecting a failed batch without installing it', async () => {
    const fixture = await materializationFixture(undefined, 2)
    const gate = fixture.tree.deferFileInspection(['gate', 'failure.bin'])
    const first = beginInitialClaim(fixture.session, ['gate', 'failure.bin'],
      { ...revision(0n), fileId: identity(130) })
    await gate.started
    const left = fixture.tree.deferFileInspection(['left', 'failed.bin'])
    const right = fixture.tree.deferFileInspection(['right', 'drained.bin'])
    const claims = [left, right].map((_inspection, index) => beginInitialClaim(fixture.session,
      [index === 0 ? 'left' : 'right', index === 0 ? 'failed.bin' : 'drained.bin'],
      { ...revision(0n), fileId: identity(131 + index) }))
    await Promise.resolve()
    gate.resolve()
    const firstTransaction = await first
    const installedBeforeFailure = fixture.checkpoints.installedClaimBatches.length
    await Promise.all([left.started, right.started])
    const raw = new Error('injected inspection failure')
    left.reject(raw)
    let batchSettled = false
    const settled = Promise.allSettled(claims).then((results) => {
      batchSettled = true
      return results
    })
    await Promise.resolve()
    expect(batchSettled).toBe(false)
    right.resolve()
    const results = await settled
    expect(results.map(result => result.status === 'rejected' ? result.reason : undefined))
      .toEqual([raw, raw])
    expect(fixture.checkpoints.installedClaimBatches).toHaveLength(installedBeforeFailure)
    await firstTransaction.retire()
  })

  it('batch-reclassifies occupied destinations and installs absent claims in original order', async () => {
    const fixture = await materializationFixture(undefined, 2)
    const gate = fixture.tree.deferFileInspection(['gate', 'reclassify.bin'])
    const first = beginInitialClaim(fixture.session, ['gate', 'reclassify.bin'],
      { ...revision(0n), fileId: identity(140) })
    await gate.started
    const paths = [['p0', '0.bin'], ['p1', '1.bin'], ['p2', '2.bin'], ['p3', '3.bin']] as const
    const revisions = paths.map((_path, index) => ({ ...revision(0n), fileId: identity(141 + index) }))
    const inspections = paths.map(path => fixture.tree.deferFileInspection(path))
    const claims = paths.map((path, index) =>
      beginInitialClaim(fixture.session, path, revisions[index]!))
    await Promise.resolve()
    gate.resolve()
    const firstTransaction = await first
    await Promise.all([inspections[0]!.started, inspections[1]!.started])
    seedOccupiedClaim(fixture, paths[0], revisions[0]!)
    inspections[0]!.resolve('occupied')
    await inspections[2]!.started
    inspections[1]!.resolve()
    await inspections[3]!.started
    seedOccupiedClaim(fixture, paths[2], revisions[2]!)
    inspections[2]!.resolve('occupied')
    inspections[3]!.resolve()
    const transactions = await Promise.all(claims)
    expect(fixture.checkpoints.lineageBatches.slice(-2)).toEqual([
      paths.map(path => path.join('/')),
      ['p0/0.bin', 'p2/2.bin'],
    ])
    expect(fixture.checkpoints.installedClaimBatches.at(-1)).toEqual(['p1/1.bin', 'p3/3.bin'])
    await Promise.all([firstTransaction, ...transactions].map(transaction => transaction.retire()))
  })

  it.each([0, 1.5, FILE_CHECKPOINT_BATCH_REQUEST_LIMIT + 1])(
    'rejects invalid initial claim inspection width %s',
    async (maximumConcurrentInitialClaimInspections) => {
      await expect(materializationFixture(undefined, maximumConcurrentInitialClaimInspections))
        .rejects.toThrow('maximum concurrent initial claim inspections is invalid')
    },
  )

})

describe('persistent DirectoryTree recovery authority', () => {

  it('does not create a replacement when the opened revision changes', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(1n),
    })
    await first.writeRange(0n, Uint8Array.of(9))
    await first.commit()
    await first.close()

    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => ({
        fileId: FILE_ID,
        fileRevision: NEXT_REVISION,
        exactSize: 1n,
      }),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'revision-conflict',
    })
    expect(fixture.events.filter((event) => event === 'create:report.bin')).toHaveLength(1)
  })

  it('blocks an invalid same-revision size without creating another object', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(1n),
    })
    await first.writeRange(0n, Uint8Array.of(1))
    await first.commit()
    await first.close()

    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'invalid',
    })
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('blocks multiple persisted objects for one lineage without moving ranges', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    await first.writeRange(0n, Uint8Array.of(1))
    await first.checkpoint()
    await first.close()
    const original = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records[0]!
    const conflictingCandidate = newFileCheckpointV2({
      ...original,
      ownedObjectId: identity(99, 32),
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    fixture.checkpoints.seedCommittedForTest(newFileCheckpointV2({
      ...conflictingCandidate,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    }))

    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toMatchObject({
      name: 'CheckpointLineageDecisionError',
      decision: 'ownership-conflict',
    })
    const records = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records
    expect(records).toHaveLength(2)
    expect(records.find(record => record.recordId === original.recordId)?.verifiedRanges)
      .toEqual([{ start: 0n, end: 1n }])
    expect(records.find(record => record.recordId !== original.recordId)?.verifiedRanges)
      .toEqual([{ start: 0n, end: 1n }])
  })

  it('classifies an occupied absent-lineage destination as a collision before claiming', async () => {
    const fixture = await materializationFixture()
    fixture.tree.occupy(['report.bin'], identity(88, 32))

    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(1n),
    })).rejects.toMatchObject({ name: 'DestinationCollisionError', kind: 'collision' })
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([])
  })

  it('recreates only the selected authority after a candidate-before-object restart', async () => {
    const fixture = await materializationFixture()
    fixture.tree.failNextCreation()
    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toThrow('simulated pre-object crash')
    const selected = fixture.tree.proposedOwnedObjectIds[0]!
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([expect.objectContaining({ ownedObjectId: selected })])
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
    })
    const resumed = await reopened.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    expect(resumed.ownedObjectId).toBe(selected)
    expect(fixture.tree.proposedOwnedObjectIds).toEqual([selected])
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('rejects unresolved post-object recovery as operation ownership attention', async () => {
    const fixture = await materializationFixture()
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    await transaction.writeRange(0n, Uint8Array.of(7))
    await transaction.checkpoint()
    await transaction.close()
    const committed = (await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records[0]!
    fixture.checkpoints.seedCandidateForTest(newFileCheckpointV2({
      ...committed,
      stateGeneration: committed.stateGeneration + 1n,
      checkpointGeneration: committed.checkpointGeneration + 1n,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    }))
    fixture.tree.occupy(['report.bin'], identity(88, 32))
    await fixture.session.close()
    const trace: PersistentTreeTraceEvent[] = []

    await expect(PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
      trace: event => trace.push(event),
    })).rejects.toMatchObject({
      name: 'InvalidStateError',
      reason: 'target-ownership-unknown',
      stage: 'checkpoint',
    })

    expect(trace).toEqual([expect.objectContaining({
      name: 'receive.operation.needs_attention',
      operation_id: fixture.binding.operationId,
      needs_attention_reason: 'target-ownership-unknown',
    })])
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([expect.objectContaining({ recordId: committed.recordId })])
  })

  it('recovers the same object when restart occurs before atomic handle promotion', async () => {
    const fixture = await materializationFixture()
    fixture.checkpoints.failNextCreatedFileCommit()
    await expect(fixture.session.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })).rejects.toThrow('simulated atomic created-file commit failure')
    const selected = fixture.tree.proposedOwnedObjectIds[0]!
    expect(fixture.tree.visible(['report.bin'])).toEqual(new Uint8Array())
    await fixture.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: fixture.tree,
      checkpoints: fixture.checkpoints, semantic: fixture.checkpoints,
    })
    const resumed = await reopened.beginFile({
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    })
    expect((await fixture.checkpoints.scanCandidates({ direction: 'ascending' })).records)
      .toEqual([])
    expect(resumed.ownedObjectId).toBe(selected)
    expect(fixture.tree.proposedOwnedObjectIds).toEqual([selected])
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
  })

  it('concurrent callers converge on the repository-selected object identity', async () => {
    const fixture = await materializationFixture()
    const request = {
      materializationRelativePath: ['report.bin'],
      openRevision: async () => revision(2n),
    } as const

    const [left, right] = await Promise.all([
      fixture.session.beginFile(request),
      fixture.session.beginFile(request),
    ])
    expect(left.ownedObjectId).toBe(right.ownedObjectId)
    expect(fixture.events.filter(event => event === 'create:report.bin')).toHaveLength(1)
    expect((await fixture.checkpoints.scanCommitted({
      direction: 'ascending',
      fileId: FILE_ID,
    })).records).toHaveLength(1)
  })

})

describe('persistent tree diagnostics and mutation admission', () => {
  it('rechecks object identity before writer acquisition and after checkpoint commit', async () => {
    const fixture = await materializationFixture()
    const beforeWriter = await fixture.session.beginFile({
      materializationRelativePath: ['writer.bin'],
      openRevision: async () => revision(1n),
    })
    fixture.tree.failVerification(['writer.bin'], 'writer-open', 1)
    await expect(beforeWriter.writeRange(0n, Uint8Array.of(1))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(beforeWriter.verifiedRanges).toEqual([])

    const afterCommit = await fixture.session.beginFile({
      materializationRelativePath: ['commit.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(31) }),
    })
    await afterCommit.writeRange(0n, Uint8Array.of(7))
    fixture.tree.failVerification(['commit.bin'], 'commit', 1)
    await expect(afterCommit.commit()).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
  })

  it('keeps range success off trace while retaining checkpoint decisions and transaction failure', async () => {
    const traceEvents: OutputTraceEvent[] = []
    const writeFailures: OutputFailureObservation<'output_write'>[] = []
    const diagnostics: OutputDiagnosticsPorts = {
      backend: 'file_system_access',
      failures: {
        outputWrite: {
          stage: 'output_write',
          record: (observation) => {
            writeFailures.push(observation)
            return undefined
          },
        },
      },
      trace: {
        current: (event) => traceEvents.push(event),
      },
    }
    const fixture = await materializationFixture(diagnostics)
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: ['bounded.bin'],
      recovery: preservingRecoveryPolicy(),
      openRevision: async () => revision(2n),
    })
    await transaction.writeRange(0n, Uint8Array.of(1))
    await transaction.checkpoint()
    expect(traceEvents).toEqual([{
      eventName: 'checkpoint',
      payload: { backend: 'file_system_access', transition: 'persisted', decision: 'installed' }, }])
    fixture.tree.failVerification(['bounded.bin'], 'writer-open', 2)
    await expect(transaction.writeRange(1n, Uint8Array.of(2)))
      .rejects.toBeInstanceOf(TargetOwnershipUnknownError)

    expect(writeFailures).toEqual([
      expect.objectContaining({
        nativeClass: 'invalid_state',
        code: 'ownership',
      }),
    ])
    expect(traceEvents.at(-1)).toEqual({
      eventName: 'output_write',
      payload: {
        backend: 'file_system_access',
        transition: 'transaction_failed',
      },
    })
  })

  it('admits directory mutations synchronously and drains them without serializing workers', async () => {
    const fixture = await materializationFixture()
    const firstBarrier = fixture.tree.deferDirectory(['first'])
    const secondBarrier = fixture.tree.deferDirectory(['second'])

    const first = fixture.session.ensureDirectory(['first'])
    const second = fixture.session.ensureDirectory(['second'])
    await Promise.all([firstBarrier.started, secondBarrier.started])

    let drained = false
    const close = fixture.session.close().then(() => { drained = true })
    expect(() => fixture.session.ensureDirectory(['late'])).toThrowError(
      expect.objectContaining({ name: 'InvalidStateError' }),
    )
    await Promise.resolve()
    expect(drained).toBe(false)

    secondBarrier.release()
    await second
    await Promise.resolve()
    expect(drained).toBe(false)

    firstBarrier.release()
    await first
    await close
    expect(drained).toBe(true)
  })

  it('pauses transferred file transactions concurrently before completing the drain', async () => {
    const fixture = await materializationFixture()
    const firstBarrier = fixture.tree.deferFileClose(['first.bin'])
    const secondBarrier = fixture.tree.deferFileClose(['second.bin'])
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['first.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(51) }),
    })
    const second = await fixture.session.beginFile({
      materializationRelativePath: ['second.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(52) }),
    })
    await first.writeRange(0n, Uint8Array.of(1))
    await second.writeRange(0n, Uint8Array.of(2))

    let drained = false
    const close = fixture.session.close().then(() => { drained = true })
    await Promise.all([firstBarrier.started, secondBarrier.started])
    expect(drained).toBe(false)

    firstBarrier.release()
    await first.close()
    expect(drained).toBe(false)

    secondBarrier.release()
    await second.close()
    await close
    expect(drained).toBe(true)
  })

  it('keeps a file admitted across its first await when close takes the external cut', async () => {
    const fixture = await materializationFixture()
    const revisionBarrier = deferred<OpenedFileRevision>()
    const beginning = fixture.session.beginFile({
      materializationRelativePath: ['late.bin'],
      openRevision: () => revisionBarrier.promise,
    })
    let drained = false
    const close = fixture.session.close().then(() => { drained = true })
    await expect(fixture.session.beginFile({
      materializationRelativePath: ['rejected.bin'],
      openRevision: async () => revision(0n),
    })).rejects.toMatchObject({ name: 'InvalidStateError' })
    await Promise.resolve()
    expect(drained).toBe(false)

    revisionBarrier.resolve(revision(0n))
    const transaction = await beginning
    await close
    expect(drained).toBe(true)
    await expect(transaction.writeRange(0n, new Uint8Array()))
      .rejects.toMatchObject({ kind: 'output-state' })
  })

  it('isolates one failed file without removing another successful visible file', async () => {
    const fixture = await materializationFixture()
    const good = await fixture.session.beginFile({
      materializationRelativePath: ['good.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(41) }),
    })
    await good.writeRange(0n, Uint8Array.of(1))
    await good.commit()

    const bad = await fixture.session.beginFile({
      materializationRelativePath: ['bad.bin'],
      openRevision: async () => ({ ...revision(1n), fileId: identity(42) }),
    })
    fixture.tree.failVerification(['bad.bin'], 'writer-open', 1)
    await expect(bad.writeRange(0n, Uint8Array.of(2))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(fixture.tree.visible(['good.bin'])).toEqual(Uint8Array.of(1))
    expect(fixture.tree.visible(['bad.bin'])).toEqual(new Uint8Array())
  })
})

async function materializationFixture(
  diagnostics?: OutputDiagnosticsPorts,
  maximumConcurrentInitialClaimInspections?: number,
) {
  const binding = durableCheckpointNamespaceIdentity({
    operationId: identity(1),
    receiveIntentDigest: identity(2, 32),
    materializationBindingDigest: identity(3, 32),
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(4, 32),
  })
  const events: string[] = []
  const tree = new MemoryTree(events, binding)
  const checkpoints = new MemoryCheckpointRepository(binding)
  const session = await PersistentTreeOutputSession.open({
    tree,
    checkpoints, semantic: checkpoints,
    ...(maximumConcurrentInitialClaimInspections === undefined
      ? {}
      : { maximumConcurrentInitialClaimInspections }),
    ...(diagnostics === undefined ? {} : { diagnostics }),
  })
  return { binding, events, tree, checkpoints, session }
}

function revision(exactSize: bigint): OpenedFileRevision {
  return Object.freeze({ fileId: FILE_ID, fileRevision: FILE_REVISION, exactSize })
}

function preservingRecoveryPolicy() {
  return Object.freeze({
    kind: 'preserve' as const,
    costBudget: Object.freeze({
      maximumPrefixCopyBytes: 1_024n,
      maximumCumulativeWriteAmplificationBytes: 2_048n,
      maximumPeakTemporaryBytes: 1_024n,
    }),
  })
}

class MemoryCheckpointRepository implements FileCheckpointJournal {
  readonly binding: FileCheckpointJournal['binding']
  readonly #candidates = new Map<string, FileCheckpointV2>()
  readonly #committed = new Map<string, FileCheckpointV2>()
  readonly #handles = new Map<string, PersistentHandleRecord<unknown>>()
  readonly #finalProofs = new Map<string, NonNullable<Awaited<ReturnType<
    SemanticPersistentOutputJournal['readMaterializationFinalProof']
  >>>>()
  readonly #directoryAdmissions = new Map<string, string>()
  readonly #directoryFinalizations = new Map<string, string>()
  #failNextCommit = false
  #failCreatedFile = false
  #failFinal: 'before' | 'after' | undefined
  commitCreatedFileCount = 0
  commitFinalFileCount = 0
  readCommittedCount = 0
  finalCheckpointProofCount = 0
  readonly lineageBatchSizes: number[] = []
  readonly lineageBatches: string[][] = []
  readonly installedClaimBatches: string[][] = []

  constructor(binding: FileCheckpointJournal['binding']) {
    this.binding = binding
  }

  async lookupLineage(
    request: CheckpointLineageLookupRequest,
  ): Promise<CheckpointLineageDecision> {
    return this.#lineageDecision(request)
  }

  async classifyLineages(
    requests: readonly CheckpointLineageLookupRequest[],
  ): Promise<readonly CheckpointLineageDecision[]> {
    this.lineageBatchSizes.push(requests.length)
    this.lineageBatches.push(requests.map(request => request.canonicalPath.join('/')))
    await Promise.resolve()
    return Promise.all(requests.map(request => this.lookupLineage(request)))
  }

  installInitialClaims(
    candidates: readonly FileCheckpointV2[],
  ): Promise<readonly InitialCheckpointCASResult[]> {
    this.installedClaimBatches.push(candidates.map(candidate => candidate.canonicalPath.join('/')))
    return Promise.all(candidates.map(candidate => this.createInitialCheckpoint(candidate)))
  }

  async createInitialCheckpoint(
    candidate: FileCheckpointV2,
  ): Promise<InitialCheckpointCASResult> {
    validateFileCheckpoint(candidate)
    const lineageId = deriveCheckpointLineageID(candidate)
    const decision = this.#lineageDecision({
      lineageId,
      fileId: candidate.fileId,
      canonicalPath: candidate.canonicalPath,
      fileRevision: candidate.fileRevision,
      exactSize: candidate.exactSize,
    })
    if (decision.kind !== 'absent') return decision
    this.#candidates.set(candidate.recordId, candidate)
    return Object.freeze({ kind: 'installed', lineageId, record: candidate })
  }

  async commitCreatedFile(
    input: Parameters<SemanticPersistentOutputJournal['commitCreatedFile']>[0],
  ): Promise<void> {
    this.commitCreatedFileCount += 1
    if (this.#failCreatedFile) {
      this.#failCreatedFile = false
      throw new Error('simulated atomic created-file commit failure')
    }
    validateFileCheckpointTransition(input.candidate, input.committed)
    if (this.#candidates.get(input.candidate.recordId)?.checksum !== input.candidate.checksum) {
      throw new DOMException('created-file candidate missing', 'InvalidStateError')
    }
    this.#handles.set(input.handle.id, input.handle)
    this.#committed.set(input.committed.recordId, input.committed)
    this.#candidates.delete(input.candidate.recordId)
  }

  async commitDurableCut(previous: FileCheckpointV2, durable: FileCheckpointV2): Promise<void> {
    await this.#replaceCommitted(previous, durable)
  }

  async resumePausedCheckpoint(paused: FileCheckpointV2, active: FileCheckpointV2): Promise<void> {
    await this.#replaceCommitted(paused, active)
  }

  async restartOwnedFile(
    input: Parameters<SemanticPersistentOutputJournal['restartOwnedFile']>[0],
  ) {
    const current = this.#committed.get(input.previous.recordId)
    if (current?.checksum === input.reset.checksum) return 'idempotent' as const
    if (current?.checksum !== input.previous.checksum ||
        this.#handles.get(input.expectedHandle.id)?.ownedObjectId !== input.previous.ownedObjectId) {
      throw new DOMException('owned-file restart authority changed', 'InvalidStateError')
    }
    validateFileCheckpoint(input.reset)
    this.#committed.set(input.reset.recordId, input.reset)
    return 'restart' as const
  }

  async commitFinalFile(
    input: Parameters<SemanticPersistentOutputJournal['commitFinalFile']>[0],
  ) {
    this.commitFinalFileCount += 1
    if (this.#failFinal === 'before') {
      this.#failFinal = undefined
      throw new Error('simulated pre-final-transaction crash')
    }
    const current = this.#committed.get(input.expectedCommittedCheckpoint.recordId)
    const final = input.records.finalCheckpoint
    const existingProof = this.#finalProofs.get(input.records.finalProof.proofId)
    const idempotent = current?.checksum === final.checksum &&
      existingProof?.proofDigest === input.records.finalProof.proofDigest
    if (!idempotent) {
      if (current?.checksum !== input.expectedCommittedCheckpoint.checksum) {
        throw new DOMException('final checkpoint predecessor changed', 'InvalidStateError')
      }
      this.#committed.set(final.recordId, final)
      this.#finalProofs.set(input.records.finalProof.proofId, input.records.finalProof)
    }
    const receipt = Object.freeze({
      classification: idempotent ? 'idempotent' as const : 'insert' as const,
      finalCheckpoint: final,
      finalProof: input.records.finalProof,
      ledgerEntry: input.records.ledgerEntry,
    })
    if (this.#failFinal === 'after') {
      this.#failFinal = undefined
      throw new Error('simulated ambiguous final transaction response')
    }
    return receipt
  }

  async readMaterializationFinalProof(
    _binding: Parameters<SemanticPersistentOutputJournal['readMaterializationFinalProof']>[0],
    proofId: string,
  ) {
    return this.#finalProofs.get(proofId)
  }

  async appendDirectoryAdmission(
    _binding: Parameters<SemanticPersistentOutputJournal['appendDirectoryAdmission']>[0],
    entry: Parameters<SemanticPersistentOutputJournal['appendDirectoryAdmission']>[1],
  ) {
    const existing = this.#directoryAdmissions.get(entry.entryId)
    if (existing !== undefined && existing !== entry.entryDigest) {
      throw new DOMException('directory admission changed', 'ConstraintError')
    }
    this.#directoryAdmissions.set(entry.entryId, entry.entryDigest)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  async appendDirectoryFinalization(
    _binding: Parameters<SemanticPersistentOutputJournal['appendDirectoryFinalization']>[0],
    entry: Parameters<SemanticPersistentOutputJournal['appendDirectoryFinalization']>[1],
  ) {
    const existing = this.#directoryFinalizations.get(entry.entryId)
    if (existing !== undefined && existing !== entry.entryDigest) {
      throw new DOMException('directory finalization changed', 'ConstraintError')
    }
    this.#directoryFinalizations.set(entry.entryId, entry.entryDigest)
    return existing === undefined ? 'insert' as const : 'idempotent' as const
  }

  failNextCreatedFileCommit(): void { this.#failCreatedFile = true }
  failNextFinalCommit(cut: 'before' | 'after'): void { this.#failFinal = cut }

  committed(fileId: string): FileCheckpointV2 {
    const records = [...this.#committed.values()].filter(record => record.fileId === fileId)
    const latest = records.sort((left, right) =>
      left.stateGeneration < right.stateGeneration ? 1 : -1)[0]
    if (latest === undefined) throw new Error('test checkpoint is missing')
    return latest
  }

  async resolveCandidate(
    candidate: FileCheckpointV2,
    observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  ): Promise<void> {
    const current = this.#candidates.get(candidate.recordId)
    if (current?.checksum !== candidate.checksum) {
      throw new DOMException('candidate changed during recovery', 'InvalidStateError')
    }
    const resolved = observation.kind === 'verified'
      ? observation.committed
      : observation.checkpoint
    this.#committed.set(candidate.recordId, resolved)
    this.#candidates.delete(candidate.recordId)
  }

  #lineageDecision(request: CheckpointLineageLookupRequest): CheckpointLineageDecision {
    const spec = Object.freeze({
      ...this.binding,
      fileId: request.fileId,
      canonicalPath: request.canonicalPath,
    })
    if (deriveCheckpointLineageID(spec) !== request.lineageId) {
      throw new TypeError('lineage lookup ID does not match its coordinates')
    }
    const physical = new Map(this.#committed)
    for (const [recordId, candidate] of this.#candidates) {
      if (!physical.has(recordId)) physical.set(recordId, candidate)
    }
    const records = [...physical.values()].filter(record =>
      sameCheckpointLineageSpec(record, spec))
    const decision = classifyCheckpointLineage(
      { fileRevision: request.fileRevision, exactSize: request.exactSize },
      records.map(record => ({
        fileRevision: record.fileRevision,
        exactSize: record.exactSize,
        ownedObjectId: record.ownedObjectId,
      })),
    )
    if (decision === 'absent') {
      return Object.freeze({ kind: 'absent', lineageId: request.lineageId })
    }
    if (decision === 'exact') {
      return Object.freeze({ kind: 'exact', lineageId: request.lineageId, record: records[0]! })
    }
    return Object.freeze({
      kind: decision,
      lineageId: request.lineageId,
      records: Object.freeze(records),
    })
  }

  async stageCheckpointUpdate(
    previous: FileCheckpointV2,
    candidate: FileCheckpointV2,
  ): Promise<void> {
    validateFileCheckpointTransition(previous, candidate)
    if (previous.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        this.#committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('committed checkpoint predecessor missing', 'InvalidStateError')
    }
    const current = this.#candidates.get(candidate.recordId)
    if (current !== undefined && current.checksum !== candidate.checksum) {
      throw new DOMException('checkpoint candidate changed', 'InvalidStateError')
    }
    this.#candidates.set(candidate.recordId, candidate)
  }

  async commitCheckpointCandidate(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2,
  ): Promise<void> {
    if (this.#failNextCommit) {
      this.#failNextCommit = false
      throw new Error('simulated post-object crash')
    }
    validateFileCheckpointTransition(candidate, committed)
    if (candidate.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE ||
        committed.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
      throw new TypeError('memory repository commits only verified checkpoints')
    }
    const currentCandidate = this.#candidates.get(candidate.recordId)
    const previous = this.#committed.get(candidate.recordId)
    if (currentCandidate === undefined && previous?.checksum === committed.checksum) return
    if (currentCandidate?.checksum !== candidate.checksum) {
      throw new DOMException('candidate missing', 'InvalidStateError')
    }
    if (previous !== undefined) validateFileCheckpointTransition(previous, committed)
    this.#committed.set(committed.recordId, committed)
    this.#candidates.delete(committed.recordId)
  }

  seedCommittedForTest(record: FileCheckpointV2): void {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding) ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
      throw new TypeError('test checkpoint seed escaped its namespace')
    }
    this.#committed.set(record.recordId, record)
  }

  seedCandidateForTest(record: FileCheckpointV2): void {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding) ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('test candidate seed escaped its namespace')
    }
    this.#candidates.set(record.recordId, record)
  }

  async readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined> {
    this.readCommittedCount += 1
    return this.#committed.get(recordId)
  }

  async scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(this.#committed, scan)
  }

  async scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return this.#scan(this.#candidates, scan)
  }

  async finalCheckpointProof(
    recordId: string,
    generation: bigint,
  ): Promise<FinalFileCheckpointProof> {
    this.finalCheckpointProofCount += 1
    const record = this.#committed.get(recordId)
    if (record === undefined || record.checkpointGeneration !== generation ||
        !fileCheckpointIsComplete(record)) {
      throw new DOMException('final checkpoint missing', 'NotFoundError')
    }
    return finalFileCheckpointProof(record)
  }

  failNextCommit(): void {
    this.#failNextCommit = true
  }

  async retireOperation(): Promise<void> {
    this.#candidates.clear()
    this.#committed.clear()
  }

  async #replaceCommitted(previous: FileCheckpointV2, next: FileCheckpointV2): Promise<void> {
    validateFileCheckpointTransition(previous, next)
    if (this.#committed.get(previous.recordId)?.checksum !== previous.checksum) {
      throw new DOMException('durable checkpoint predecessor changed', 'InvalidStateError')
    }
    this.#committed.set(next.recordId, next)
  }

  #scan(
    records: ReadonlyMap<string, FileCheckpointV2>,
    scan: FileCheckpointScan,
  ): FileCheckpointPage {
    const sorted = [...records.values()]
      .filter((record) => scan.fileId === undefined || record.fileId === scan.fileId)
      .sort((left, right) => left.recordId.localeCompare(right.recordId))
    if (scan.direction === 'descending') sorted.reverse()
    const after = scan.cursor === undefined
      ? sorted
      : sorted.filter((record) => scan.direction === 'ascending'
          ? record.recordId > scan.cursor!
          : record.recordId < scan.cursor!)
    const limit = scan.limit ?? 128
    const page = after.slice(0, limit)
    return Object.freeze({
      records: Object.freeze(page),
      ...(after.length >= limit && page.at(-1) !== undefined
        ? { nextCursor: page.at(-1)!.recordId }
        : {}),
    })
  }
}
