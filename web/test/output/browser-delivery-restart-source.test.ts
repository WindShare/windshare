import { describe, expect, it, vi } from 'vitest'
import { advanceBrowserDeliveryReceiving, authorizeBrowserDeliveryRestart } from '../../src/output/browser-delivery/lifecycle'
import { newFileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'

async function pausedDirectFile() {
  const f = await deliveryEngineFixture()
  const request = f.request(11, 2n)
  const previous = { ...await request.openRevision(), sourceMetadata: deliveryIdentity(71) }
  const first = await f.session.beginFile({ ...request, openRevision: async () => previous })
  await first.writeRange(0n, Uint8Array.of(8))
  await first.pause()
  await f.session.close()
  const current = { ...previous, fileRevision: deliveryIdentity(72, 16) }
  const restart = { ...request, openRevision: async () => current,
    recovery: { pausedFile: 'restart-owned-file' as const } }
  return { f, request, previous, current, restart, ownedObjectId: first.ownedObjectId }
}

describe('direct delivery restart across source reopen', () => {
  it('commits the new source and authorized empty receiving cut together', async () => {
    const { f, request, previous, current, restart, ownedObjectId } = await pausedDirectFile()
    const session = await f.reopen()
    const restarted = await session.beginFile(restart)
    expect(restarted.ownedObjectId).toBe(ownedObjectId)
    expect(restarted.initialDurableRanges).toEqual([])
    expect(f.events).toEqual(expect.arrayContaining(['restart-authorized', 'restart-completed']))
    expect(f.repository.files.get(current.fileId)).toMatchObject({
      source: current,
      state: { kind: 'receiving', checkpoint: {
        fileRevision: current.fileRevision, reusedFrom: previous.fileRevision,
        ownedObjectId, verifiedRanges: [],
      } },
    })
    await restarted.writeRange(0n, Uint8Array.of(1, 2))
    await restarted.commit()
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(1, 2))
    expect(f.repository.files.get(current.fileId)).toMatchObject({ source: current, state: { kind: 'cleaned' } })
    await session.close()
  })

  it.each(['before-source-admission', 'after-source-admission'] as const)(
    'recovers an interrupted restart %s without reverting admitted source identity', async interruption => {
      const { f, request, previous, current, restart, ownedObjectId } = await pausedDirectFile()
      const session = await f.reopen()
      const commit = f.targetCheckpoints.commitDurableCut.bind(f.targetCheckpoints)
      const admission = vi.spyOn(f.targetCheckpoints, 'commitDurableCut').mockImplementation(async (before, after) => {
        if (interruption === 'before-source-admission' && before.fileRevision !== after.fileRevision) {
          throw new Error('source admission interrupted')
        }
        await commit(before, after)
      })
      if (interruption === 'after-source-admission') f.repository.failTransition = 'receiving'
      await expect(session.beginFile(restart)).rejects.toThrow(
        interruption === 'before-source-admission' ? 'source admission interrupted' : 'journal failure',
      )
      admission.mockRestore()
      const durableRevision = interruption === 'before-source-admission' ? previous.fileRevision : current.fileRevision
      expect(f.targetCheckpoints.committed(current.fileId)).toMatchObject({
        fileRevision: durableRevision, ownedObjectId, verifiedRanges: [],
      })
      expect(f.repository.files.get(current.fileId)).toMatchObject({
        source: previous, state: { kind: 'restart-authorized' },
      })
      await session.close()

      const recovered = await f.reopen()
      expect(f.repository.files.get(current.fileId)).toMatchObject({
        source: { fileRevision: durableRevision }, state: { kind: 'receiving' },
      })
      expect(f.targetCheckpoints.committed(current.fileId).fileRevision).toBe(durableRevision)
      expect(f.events).toEqual(expect.arrayContaining(['restart-recovering', 'restart-completed']))
      const reopenedSource = { ...current, fileRevision: deliveryIdentity(73, 16) }
      const resumed = await recovered.beginFile({ ...request, openRevision: async () => reopenedSource })
      expect(resumed.ownedObjectId).toBe(ownedObjectId)
      expect(resumed.initialDurableRanges).toEqual([])
      await resumed.writeRange(0n, Uint8Array.of(3, 4))
      await resumed.commit()
      expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(3, 4))
      await recovered.close()
    },
  )

  it.each([undefined, deliveryIdentity(90)])('keeps the saved prefix when source evidence is %s', async sourceMetadata => {
    const { f, previous, current, request, restart } = await pausedDirectFile()
    const session = await f.reopen()
    const before = f.targetCheckpoints.committed(current.fileId)
    const source = { fileId: current.fileId, fileRevision: current.fileRevision, exactSize: current.exactSize,
      ...(sourceMetadata === undefined ? {} : { sourceMetadata }) }
    await expect(session.beginFile({ ...restart, openRevision: async () => source })).rejects.toThrow(/applicability/)
    expect(f.targetCheckpoints.committed(current.fileId)).toEqual(before)
    expect(f.repository.files.get(current.fileId)).toMatchObject({ source: previous, state: { kind: 'receiving' } })
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(8))
    await session.close()
  })

  it('requires explicit restart authority before source admission can discard durable coverage', async () => {
    const { f, previous, current } = await pausedDirectFile()
    const session = await f.reopen()
    const record = f.repository.files.get(current.fileId)!
    const before = f.targetCheckpoints.committed(current.fileId)
    const reset = newFileCheckpointV2({ ...before,
      fileRevision: current.fileRevision, reusedFrom: previous.fileRevision,
      verifiedRanges: [], stateGeneration: before.stateGeneration + 2n,
      checkpointGeneration: before.checkpointGeneration + 2n,
    })
    expect(() => advanceBrowserDeliveryReceiving(f.policy, record, reset)).toThrow(/coverage/)
    const authorized = authorizeBrowserDeliveryRestart(f.policy, record, before, 'restart-authorization')
    expect(advanceBrowserDeliveryReceiving(f.policy, authorized, reset)).toMatchObject({
      source: current, state: { kind: 'receiving', checkpoint: reset },
    })
    for (const changes of [
      { ownedObjectId: deliveryIdentity(99) },
      { verifiedRanges: before.verifiedRanges },
      { reusedFrom: deliveryIdentity(98, 16) },
    ]) {
      const invalid = newFileCheckpointV2({ ...reset, ...changes })
      expect(() => advanceBrowserDeliveryReceiving(f.policy, authorized, invalid)).toThrow()
    }
    await session.close()
  })
})
