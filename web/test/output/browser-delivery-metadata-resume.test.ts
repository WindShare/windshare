import { describe, expect, it, vi } from 'vitest'
import {
  CheckpointSourceEvidenceUnavailableError, SourceRevisionChangedError,
} from '../../src/output/persistent-tree/errors'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'

describe('folder delivery metadata recovery', () => {
  it.each([2n, 4n])('keeps the owned prefix for a reopened %s-byte source through final delivery', async size => {
    const f = await deliveryEngineFixture()
    const original = f.request(11, size)
    const source = { ...await original.openRevision(), sourceMetadata: deliveryIdentity(71) }
    const first = await f.session.beginFile({ ...original, openRevision: async () => source })
    await first.writeRange(0n, Uint8Array.of(7))
    await first.pause()
    const owned = first.ownedObjectId
    await f.session.close()

    const session = await f.reopen()
    const current = { ...source, fileRevision: deliveryIdentity(72, 16) }
    const request = { ...original, openRevision: async () => current }
    const resumed = await session.beginFile(request)
    expect(resumed.ownedObjectId).toBe(owned)
    expect(resumed.initialDurableRanges).toEqual([{ start: 0n, end: 1n }])
    expect(f.repository.files.get(source.fileId)?.source).toMatchObject(current)
    await resumed.writeRange(1n, new Uint8Array(Number(size - 1n)).fill(8))
    await resumed.commit()
    const historical = f.repository.files.get(source.fileId)!
    expect(historical.state.kind).toBe('cleaned')
    expect(historical.source.fileRevision).toBe(current.fileRevision)
    expect(f.targetTree.file(original.materializationRelativePath).snapshot())
      .toEqual(Uint8Array.from([7, ...new Array(Number(size - 1n)).fill(8)]))

    // Folder resume revisits completed siblings locally without reopening source handles or mutating history.
    const reopenedSource = vi.fn(async () => ({ ...current, fileRevision: deliveryIdentity(73, 16) }))
    const revisited = await session.beginFile({ ...request, openRevision: reopenedSource })
    expect(reopenedSource).not.toHaveBeenCalled()
    expect(revisited.ownedObjectId).toBe(owned)
    expect(revisited.revision.fileRevision).toBe(current.fileRevision)
    expect(revisited.initialDurableRanges).toEqual([{ start: 0n, end: size }])
    const proof = await revisited.commit()
    expect(proof.fileRevision).toBe(current.fileRevision)
    expect(f.repository.files.get(source.fileId)?.digest).toBe(historical.digest)

    // Forced readmission still cannot rebind a completed historical delivery to a new revision.
    await expect(session.beginFile({ ...request, requireRevisionOpen: true, openRevision: reopenedSource }))
      .rejects.toMatchObject({ decision: 'reuse-state-blocked' })
    expect(reopenedSource).toHaveBeenCalledTimes(1)
    const resized = vi.fn(async () => ({ ...current, exactSize: size + 1n, fileRevision: deliveryIdentity(74, 16) }))
    await expect(session.beginFile({ ...request, expectedSize: size + 1n, openRevision: resized }))
      .rejects.toBeInstanceOf(SourceRevisionChangedError)
    expect(resized).toHaveBeenCalledOnce()
    expect(f.repository.files.get(source.fileId)?.digest).toBe(historical.digest)
    await session.close()
  })

  it('uses the stage until delivery, then the verified target even if cleanup was interrupted', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    const source = await request.openRevision()
    const bytes = Uint8Array.of(3, 4, 5, 6)
    const first = await f.session.beginFile(request)
    await first.writeRange(0n, bytes)
    f.onTargetWrite(async () => { throw new DOMException('destination paused', 'AbortError') })
    await expect(first.commit()).rejects.toThrow('destination paused')
    await f.session.close()

    f.onTargetWrite(undefined)
    const reopened = await f.reopen()
    const openRevision = vi.fn(async () => { throw new Error('sender unavailable') })
    const openLocal = () => reopened.beginFile({ ...request, openRevision })
    const staged = await openLocal()
    expect(staged.revision.fileRevision).toBe(source.fileRevision)
    expect(staged.contentReplica?.operationId).toBe(f.policy.staging!.operationId)
    expect(staged.checkpointObjectId).toBe(staged.contentReplica?.objectId)
    expect(staged.initialDurableRanges).toEqual([{ start: 0n, end: 4n }])
    // The browser removed the physical stage before the journal recorded cleanup.
    f.repository.failTransition = 'cleaned'
    const delivered = await staged.commit()
    expect(reopened.summary.cleanupPendingFiles).toBe(1)
    expect(await f.stageTree.openFile([source.fileId], staged.checkpointObjectId!)).toBeUndefined()

    const saved = await openLocal()
    expect(saved.contentReplica?.operationId).toBe(f.policy.target.operationId)
    expect(saved.checkpointObjectId).toBe(saved.ownedObjectId)
    expect(saved.initialDurableRanges).toEqual([{ start: 0n, end: 4n }])
    expect((await saved.commit()).checkpointProof).toEqual(delivered.checkpointProof)
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(bytes)
    await f.targetTree.removeFile(request.materializationRelativePath, saved.ownedObjectId)
    await expect(openLocal()).rejects.toMatchObject({ reason: 'target-ownership-unknown' })
    expect(openRevision).not.toHaveBeenCalled()
    await reopened.close()
  })

  it.each([
    { size: 0n, requireRevisionOpen: false },
    { size: 0n, requireRevisionOpen: true },
    { size: 2n, requireRevisionOpen: true },
  ])('opens the source for an unfinalized $size-byte direct file (forced=$requireRevisionOpen)', async ({ size, requireRevisionOpen }) => {
    const f = await deliveryEngineFixture()
    const request = f.request(11, size)
    const original = { ...await request.openRevision(), sourceMetadata: deliveryIdentity(71) }
    const first = await f.session.beginFile({ ...request, recovery: { pausedFile: 'restart-owned-file' }, openRevision: async () => original })
    const bytes = new Uint8Array(Number(size)).fill(7)
    if (size > 0n) await first.writeRange(0n, bytes)
    await first.pause()
    expect(f.targetCheckpoints.commitFinalFileCount).toBe(0)

    const current = { ...original, fileRevision: deliveryIdentity(72, 16) }
    const openRevision = vi.fn(async () => current)
    const readmitted = await f.session.beginFile({ ...request, requireRevisionOpen, openRevision })
    expect(openRevision).toHaveBeenCalledOnce()
    expect(readmitted.ownedObjectId).toBe(first.ownedObjectId)
    expect((await readmitted.commit()).fileRevision).toBe(current.fileRevision)
    expect(f.targetCheckpoints.commitFinalFileCount).toBe(1)
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(bytes)
    await f.session.close()
  })

  it('reconciles storage admission after the delivery journal commit was interrupted', async () => {
    const f = await deliveryEngineFixture()
    const original = f.request()
    const source = { ...await original.openRevision(), sourceMetadata: deliveryIdentity(71) }
    const first = await f.session.beginFile({ ...original, openRevision: async () => source })
    await first.writeRange(0n, Uint8Array.of(7, 8))
    await first.pause()
    await f.session.close()
    const current = { ...source, fileRevision: deliveryIdentity(72, 16) }
    const session = await f.reopen()
    f.repository.failTransition = 'receiving'
    await expect(session.beginFile({ ...original, openRevision: async () => current })).rejects.toThrow('journal failure')
    expect(f.repository.files.get(source.fileId)?.source.fileRevision).toBe(source.fileRevision)
    expect((await f.stage.readCheckpoint(source.fileId))?.fileRevision).toBe(current.fileRevision)
    await session.close()
    const recovered = await f.reopen()
    const resumed = await recovered.beginFile({ ...original, openRevision: async () => current })
    expect(resumed.initialDurableRanges).toEqual([{ start: 0n, end: 2n }])
    await resumed.writeRange(2n, Uint8Array.of(9, 10))
    await resumed.commit()
    expect(f.targetTree.file(original.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(7, 8, 9, 10))
    await recovered.close()
  })

  it.each([
    [undefined, CheckpointSourceEvidenceUnavailableError],
    [deliveryIdentity(90), SourceRevisionChangedError],
  ] as const)('retains candidates when reopened metadata is %s', async (sourceMetadata, errorType) => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    const source = { ...await request.openRevision(), sourceMetadata: deliveryIdentity(71) }
    const first = await f.session.beginFile({ ...request, openRevision: async () => source })
    await first.writeRange(0n, Uint8Array.of(7))
    await first.pause()
    const before = f.repository.files.get(source.fileId)!
    await expect(f.session.beginFile({ ...request, openRevision: async () => ({
      fileId: source.fileId, exactSize: source.exactSize, fileRevision: deliveryIdentity(72, 16),
      ...(sourceMetadata === undefined ? {} : { sourceMetadata }),
    }) })).rejects.toBeInstanceOf(errorType)
    expect(f.repository.files.get(source.fileId)?.source).toEqual(before.source)
    expect(f.stageTree.file([source.fileId]).snapshot()).toEqual(Uint8Array.of(7))
    await f.session.close()
  })
})
