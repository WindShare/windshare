import { describe, expect, it, vi } from 'vitest'
import { materializationFixture, revision, FILE_ID } from './persistent-tree-session-fixture'
import { identity } from './planning/fixture'
import { encodeFileCheckpointV2, decodeFileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import { admitCheckpointMetadataReuse } from '../../src/output/persistence/metadata-reuse'
import { preservingRecoveryPolicy } from './persistent-tree-file-fixture'

const sourceMetadata = identity(71, 32)
const previous = { ...revision(6n), sourceMetadata }
const current = { ...previous, fileRevision: identity(72) }
const path = ['resume.bin'] as const

async function partial() {
  const f = await materializationFixture()
  const tx = await f.session.beginFile({ materializationRelativePath: path, openRevision: async () => previous })
  await tx.writeRange(0n, Uint8Array.of(1, 2, 3))
  await tx.pause()
  return f
}

describe('metadata resume admission', () => {
  it('reuses the owned durable prefix under a new revision and completes only that revision', async () => {
    const f = await partial()
    const old = f.checkpoints.committed(FILE_ID)
    const tx = await f.session.beginFile({
      materializationRelativePath: path, openRevision: async () => current,
      recovery: preservingRecoveryPolicy(),
    })
    expect(tx.initialDurableRanges).toEqual([{ start: 0n, end: 3n }])
    const accepted = f.checkpoints.committed(FILE_ID)
    expect(accepted).toMatchObject({
      recordId: old.recordId, ownedObjectId: old.ownedObjectId,
      fileRevision: current.fileRevision, reusedFrom: previous.fileRevision, sourceMetadata,
    })
    expect(decodeFileCheckpointV2(encodeFileCheckpointV2(accepted))).toEqual(accepted)
    expect(f.tree.visible(path)).toEqual(Uint8Array.of(1, 2, 3))
    await tx.writeRange(3n, Uint8Array.of(4, 5, 6))
    await tx.commit()
    expect(f.tree.visible(path)).toEqual(Uint8Array.of(1, 2, 3, 4, 5, 6))
    expect(f.checkpoints.committed(FILE_ID).fileRevision).toBe(current.fileRevision)
  })

  it.each([undefined, identity(73, 32)])('retains candidates when metadata is %s', async metadata => {
    const f = await partial()
    const old = f.checkpoints.committed(FILE_ID)
    const opened = { ...revision(6n), fileRevision: current.fileRevision,
      ...(metadata === undefined ? {} : { sourceMetadata: metadata }) }
    await expect(f.session.beginFile({ materializationRelativePath: path,
      openRevision: async () => opened })).rejects.toThrow()
    expect(f.checkpoints.committed(FILE_ID)).toEqual(old)
    expect(f.tree.visible(path)).toEqual(Uint8Array.of(1, 2, 3))
  })

  it('does not bypass local object verification with matching source metadata', async () => {
    const f = await partial()
    const old = f.checkpoints.committed(FILE_ID)
    f.tree.failVerification(path, 'checkpoint', f.tree.file(path).verificationCount('checkpoint') + 1)
    await expect(f.session.beginFile({ materializationRelativePath: path,
      openRevision: async () => current })).rejects.toThrow()
    expect(f.checkpoints.committed(FILE_ID).fileRevision).toBe(old.fileRevision)
  })

  it('keeps the old durable predecessor after an interrupted admission and retries it once', async () => {
    const f = await partial()
    const commit = vi.spyOn(f.checkpoints, 'commitDurableCut')
    commit.mockRejectedValueOnce(new Error('interrupted admission'))
    await expect(f.session.beginFile({ materializationRelativePath: path,
      openRevision: async () => current })).rejects.toThrow('interrupted admission')
    const old = f.checkpoints.committed(FILE_ID)
    expect(old.fileRevision).toBe(previous.fileRevision)
    const tx = await f.session.beginFile({ materializationRelativePath: path,
      openRevision: async () => current })
    expect(tx.initialDurableRanges).toEqual(old.verifiedRanges)
    await tx.pause()
  })

  it('never rebinds historical completion receipts to a new revision', async () => {
    const f = await partial()
    const tx = await f.session.beginFile({ materializationRelativePath: path,
      openRevision: async () => previous })
    await tx.writeRange(3n, Uint8Array.of(4, 5, 6))
    await tx.commit()
    const delivered = f.checkpoints.committed(FILE_ID)
    await expect(admitCheckpointMetadataReuse(delivered, current, f.checkpoints, f.checkpoints))
      .rejects.toThrow('historical delivery')
    expect(f.checkpoints.committed(FILE_ID)).toEqual(delivered)
  })
})
