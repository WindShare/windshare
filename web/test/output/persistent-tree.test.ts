import { describe, expect, it } from 'vitest'

import type {
  OutputDiagnosticsPorts,
  OutputFailureObservation,
  OutputTraceEvent,
} from '../../src/output/diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { FILE_CHECKPOINT_BATCH_REQUEST_LIMIT } from '../../src/output/persistence/journal'
import type {
  OpenedFileRevision,
  PersistentTreeTraceEvent,
} from '../../src/output/persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { identity } from './planning/fixture'
import {
  beginInitialClaim,
  deferred,
  preservingRecoveryPolicy,
  seedOccupiedClaim,
} from './persistent-tree-file-fixture'
import {
  FILE_ID,
  materializationFixture,
  revision,
} from './persistent-tree-session-fixture'

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
