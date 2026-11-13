import { describe, expect, it } from 'vitest'
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
    await expect(session.beginFile({ ...request, openRevision: async () => ({ ...current, fileRevision: deliveryIdentity(73, 16) }) }))
      .rejects.toThrow(/applicability/)
    expect(f.repository.files.get(source.fileId)?.digest).toBe(historical.digest)
    await session.close()
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

  it.each([undefined, deliveryIdentity(90)])('retains candidates when reopened metadata is %s', async sourceMetadata => {
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
    }) })).rejects.toThrow(/applicability/)
    expect(f.repository.files.get(source.fileId)?.source).toEqual(before.source)
    expect(f.stageTree.file([source.fileId]).snapshot()).toEqual(Uint8Array.of(7))
    await f.session.close()
  })
})
