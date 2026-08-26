import { describe, expect, it } from 'vitest'

import {
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
} from '../../src/output/persistence/checkpoint'
import { PersistentPreservingWriterOpenError } from '../../src/output/persistent-tree/recovery'
import {
  createTestCheckpointAuthorities,
  preservingRecoveryPolicy,
} from './persistent-tree-file-fixture'
import {
  FILE_ID,
  materializationFixture,
  revision,
} from './persistent-tree-session-fixture'

describe('persistent tree checkpoint and paused recovery', () => {
  it('advances one queued automatic checkpoint and reopens preserving the durable prefix', async () => {
    const fixture = await materializationFixture()
    const authorities = createTestCheckpointAuthorities()
    const path = ['periodic.bin'] as const
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: path,
      recovery: preservingRecoveryPolicy(),
      automaticCheckpointAdmission: authorities.automaticCheckpointAdmission.enrollFile(path),
      preservingWriterCapacity: authorities.preservingWriterCapacity,
      openRevision: async () => revision(6n),
    })
    const file = fixture.tree.file(['periodic.bin'])
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await expect(transaction.automaticCheckpoint('pending-bytes')).resolves.toMatchObject({
      kind: 'advanced',
      cost: { prefixCopyBytes: 3n, writeAmplificationBytes: 3n, temporaryBytes: 3n },
    })
    await transaction.writeRange(3n, Uint8Array.of(4, 5, 6))
    await transaction.commit()

    expect(file.writerModes).toEqual(['truncate', 'preserve'])
    expect(file.preservingCostCount).toBe(1)
    expect(file.flushCount).toBe(2)
    expect(authorities.automaticCheckpointAdmission.snapshot()).toMatchObject({
      committedWriteAmplificationBytes: 3n,
      tentativeHolds: 0,
    })
    expect(authorities.preservingWriterCapacity.snapshot()).toMatchObject({
      heldTemporaryBytes: 0n,
      heldTokens: 0,
    })
  })

  it('defers automatic capacity contention without closing its writer', async () => {
    const fixture = await materializationFixture()
    const authorities = createTestCheckpointAuthorities()
    const capacityBlock = await authorities.preservingWriterCapacity.reservePaused({
      materializationRelativePath: ['other.bin'],
      trigger: 'paused-file-recovery',
      cost: {
        prefixCopyBytes: 1024n * 1024n * 1024n,
        writeAmplificationBytes: 1024n * 1024n * 1024n,
        temporaryBytes: 1024n * 1024n * 1024n,
      },
    })
    capacityBlock.commit()
    const path = ['deferred.bin'] as const
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: path,
      automaticCheckpointAdmission: authorities.automaticCheckpointAdmission.enrollFile(path),
      preservingWriterCapacity: authorities.preservingWriterCapacity,
      openRevision: async () => revision(4n),
    })
    const file = fixture.tree.file(path)
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await expect(transaction.automaticCheckpoint('pending-bytes')).resolves.toMatchObject({
      kind: 'deferred',
      reason: 'capacity-unavailable',
    })
    expect(file.flushCount).toBe(0)
    expect(file.writerOpenCount).toBe(1)
    await transaction.writeRange(2n, Uint8Array.of(3, 4))
    expect(file.writerOpenCount).toBe(1)
    capacityBlock.release('unused')
    await transaction.retire()
  })

  it('keeps the durable cut and rolls back both holds when replacement open fails', async () => {
    const fixture = await materializationFixture()
    const authorities = createTestCheckpointAuthorities()
    const path = ['replacement-failure.bin'] as const
    const transaction = await fixture.session.beginFile({
      materializationRelativePath: path,
      automaticCheckpointAdmission: authorities.automaticCheckpointAdmission.enrollFile(path),
      preservingWriterCapacity: authorities.preservingWriterCapacity,
      openRevision: async () => revision(4n),
    })
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    fixture.tree.failVerification(path, 'writer-open', 2)

    const failure = await transaction.automaticCheckpoint('pending-bytes')
      .catch((error: unknown) => error)

    expect(failure).toBeInstanceOf(PersistentPreservingWriterOpenError)
    expect(failure).toMatchObject({
      materializationRelativePath: path,
      purpose: 'automatic-checkpoint',
      cost: {
        prefixCopyBytes: 2n,
        writeAmplificationBytes: 2n,
        temporaryBytes: 2n,
      },
    })
    expect(fixture.checkpoints.committed(FILE_ID).verifiedRanges).toEqual([{ start: 0n, end: 2n }])
    expect(authorities.automaticCheckpointAdmission.snapshot()).toMatchObject({
      committedWriteAmplificationBytes: 0n,
      tentativeHolds: 0,
    })
    expect(authorities.preservingWriterCapacity.snapshot()).toMatchObject({
      heldTemporaryBytes: 0n,
      heldTokens: 0,
    })
    await transaction.pause()
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
        pausedFile: 'restart-owned-file',
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

  it('turns an active crash checkpoint into an exact owned-file restart cut', async () => {
    const fixture = await materializationFixture()
    const authorities = createTestCheckpointAuthorities()
    const path = ['active-restart.bin'] as const
    const request = {
      materializationRelativePath: path,
      automaticCheckpointAdmission: authorities.automaticCheckpointAdmission.enrollFile(path),
      preservingWriterCapacity: authorities.preservingWriterCapacity,
      openRevision: async () => revision(4n),
    }
    const recovery = preservingRecoveryPolicy()
    const first = await fixture.session.beginFile({ ...request, recovery })
    await first.writeRange(0n, Uint8Array.of(1, 2))
    await expect(first.automaticCheckpoint('pending-bytes')).resolves.toMatchObject({ kind: 'advanced' })
    await first.retire()
    const restarted = await fixture.session.beginFile({ ...request, recovery: { pausedFile: 'restart-owned-file' } })
    expect(restarted.initialDurableRanges).toEqual([])
    expect(fixture.checkpoints.committed(FILE_ID).phase).toBe(FILE_CHECKPOINT_PHASE_ACTIVE)
    await restarted.writeRange(0n, Uint8Array.of(5, 6, 7, 8))
    expect(fixture.tree.file(['active-restart.bin']).writerModes.at(-1)).toBe('truncate')
  })

  it('uses the pause action as authorization for one owned preserving capacity token', async () => {
    const fixture = await materializationFixture()
    const first = await fixture.session.beginFile({
      materializationRelativePath: ['preserve.bin'],
      openRevision: async () => revision(4n),
    })
    await first.writeRange(0n, Uint8Array.of(1, 2))
    await first.pause()
    const file = fixture.tree.file(['preserve.bin'])
    const authorities = createTestCheckpointAuthorities()
    const confirmed = await fixture.session.beginFile({
      materializationRelativePath: ['preserve.bin'],
      recovery: preservingRecoveryPolicy(),
      preservingWriterCapacity: authorities.preservingWriterCapacity,
      openRevision: async () => revision(4n),
    })
    await confirmed.writeRange(2n, Uint8Array.of(3, 4))
    expect(file.writerModes.at(-1)).toBe('preserve')
    expect(authorities.preservingWriterCapacity.snapshot()).toMatchObject({
      heldTemporaryBytes: 2n,
      heldTokens: 1,
    })
    await confirmed.pause()
    expect(authorities.preservingWriterCapacity.snapshot().heldTokens).toBe(0)
  })
})
