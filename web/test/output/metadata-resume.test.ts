import { describe, expect, it, vi } from 'vitest'
import { materializationFixture, revision, FILE_ID } from './persistent-tree-session-fixture'
import { identity } from './planning/fixture'
import { encodeFileCheckpointV2, decodeFileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import { admitCheckpointMetadataReuse } from '../../src/output/persistence/metadata-reuse'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
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

  it('continues a checkpoint-only workspace from its fully flushed file before final commit', async () => {
    const f = await materializationFixture()
    await f.session.close()
    const workspace = { tree: f.tree, checkpoints: f.checkpoints }
    const first = await PersistentTreeOutputSession.open(workspace)
    const request = { fileId: FILE_ID, expectedSize: previous.exactSize, materializationRelativePath: path }
    const file = await first.beginFile({ ...request, openRevision: async () => previous })
    const bytes = Uint8Array.of(1, 2, 3, 4, 5, 6)
    await file.writeRange(0n, bytes)
    await file.pause()
    await first.close()

    const resumed = await PersistentTreeOutputSession.open(workspace)
    const offline = vi.fn(async () => { throw new Error('sender unavailable') })
    const saved = await resumed.beginFile({ ...request, openRevision: offline })
    expect(offline).not.toHaveBeenCalled()
    expect(saved.ownedObjectId).toBe(file.ownedObjectId)
    expect((await saved.commit()).fileRevision).toBe(previous.fileRevision)
    expect(f.tree.visible(path)).toEqual(bytes)
    await resumed.close()
  })

  it('looks up retained content on a same-session revisit without probing each new file', async () => {
    const f = await materializationFixture()
    const request = { fileId: FILE_ID, expectedSize: previous.exactSize, materializationRelativePath: path }
    const first = await f.session.beginFile({ ...request, openRevision: async () => previous })
    await first.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5, 6))
    await first.pause()
    const offline = vi.fn(async () => { throw new Error('sender unavailable') })
    const local = await f.session.beginFile({ ...request, openRevision: offline })
    expect(offline).not.toHaveBeenCalled()
    expect((await local.commit()).fileRevision).toBe(previous.fileRevision)

    const other = { ...previous, fileId: identity(73) }
    const reads = f.checkpoints.readCommittedCount
    const fresh = await f.session.beginFile({ ...request, fileId: other.fileId,
      materializationRelativePath: ['fresh.bin'], openRevision: async () => other })
    expect(f.checkpoints.readCommittedCount).toBe(reads)
    await fresh.close()
    await f.session.close()
  })

  it('readmits all flushed bytes under matching source metadata before the final receipt exists', async () => {
    const f = await materializationFixture()
    const request = { fileId: FILE_ID, expectedSize: previous.exactSize, materializationRelativePath: path }
    const first = await f.session.beginFile({ ...request, openRevision: async () => previous })
    const bytes = Uint8Array.of(1, 2, 3, 4, 5, 6)
    await first.writeRange(0n, bytes)
    await first.pause()
    expect(f.checkpoints.commitFinalFileCount).toBe(0)
    const openRevision = vi.fn(async () => current)
    const next = await f.session.beginFile({ ...request, requireRevisionOpen: true, openRevision })
    expect(openRevision).toHaveBeenCalledOnce()
    expect(next.initialDurableRanges).toEqual([{ start: 0n, end: 6n }])
    expect(next.ownedObjectId).toBe(first.ownedObjectId)
    expect((await next.commit()).fileRevision).toBe(current.fileRevision)
    expect(f.tree.visible(path)).toEqual(bytes)
    await f.session.close()
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

  it.each([0n, 6n])('short-circuits a completed %s-byte file on reopen without calling openRevision or bumping generation', async size => {
    const f = await materializationFixture()
    const initial = { ...revision(size), sourceMetadata }
    const first = await f.session.beginFile({
      fileId: FILE_ID, expectedSize: size, materializationRelativePath: path,
      openRevision: async () => initial,
    })
    if (size > 0n) await first.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5, 6))
    const initialCommit = await first.commit()
    const delivered = f.checkpoints.committed(FILE_ID)
    await f.session.close()

    const reopened = await PersistentTreeOutputSession.open({
      tree: f.tree, checkpoints: f.checkpoints, semantic: f.checkpoints,
    })
    const openRevision = vi.fn(async () => ({ ...initial, fileRevision: current.fileRevision }))
    const revisited = await reopened.beginFile({
      fileId: FILE_ID, expectedSize: size, materializationRelativePath: path,
      openRevision,
    })
    expect(openRevision).not.toHaveBeenCalled()
    expect(revisited.revision.fileRevision).toBe(initial.fileRevision)
    expect(revisited.initialDurableRanges).toEqual(size === 0n ? [] : [{ start: 0n, end: size }])
    const commit = await revisited.commit()
    expect(commit.checkpointGeneration).toBe(initialCommit.checkpointGeneration)
    expect(commit.recordDigest).toBe(initialCommit.recordDigest)
    expect(f.checkpoints.committed(FILE_ID)).toEqual(delivered)
    await reopened.close()
  })
})
