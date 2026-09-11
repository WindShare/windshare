import { describe, expect, it, vi } from 'vitest'
import { acquireArtifactReader } from '../../src/output/origin-private/export-readers'
import { browserDeliveryStagingPath } from '../../src/output/browser-delivery/records'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'
import { deferred } from './persistent-tree-file-fixture'

const FILE_ID = deliveryIdentity(11, 16)
const STAGE_PATH = browserDeliveryStagingPath(FILE_ID)
const CONTENT = Uint8Array.of(1, 2, 3, 4)

describe('browser delivery Stop disposition', () => {
  it.each(['pause', 'stop'] as const)('%s keeps its own receiving-storage semantics', async kind => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT.slice(0, 2))
    if (kind === 'pause') await f.session.closeForTerminalSettlement()
    else await f.session.closeForStopSettlement()
    expect(f.session.summary).toMatchObject({
      stagedBytes: kind === 'pause' ? 2n : 0n,
      reservedStagingBytes: kind === 'pause' ? 4n : 0n,
      targetSavedBytes: 0n,
    })
    if (kind === 'pause') {
      const resumed = await f.reopen()
      const remaining = await resumed.beginFile(f.request())
      expect(remaining.initialDurableRanges).toEqual([{ start: 0n, end: 2n }])
      await remaining.writeRange(2n, CONTENT.slice(2))
      await remaining.commit()
      expect(f.targetTree.file(['nested', 'file-11.bin']).snapshot()).toEqual(CONTENT)
      await resumed.close()
    } else {
      expect(() => f.stageTree.file(STAGE_PATH)).toThrow()
      expect(f.phases.get(FILE_ID)).toBe('released')
      expect(f.events.indexOf('discard-started')).toBeLessThan(f.events.indexOf('delete-stage'))
      expect(f.events.indexOf('delete-stage')).toBeLessThan(f.events.indexOf('budget-released'))
    }
  })

  it('fences new receiving while an accepted revision opening drains into owned cleanup', async () => {
    const f = await deliveryEngineFixture()
    const opening = deferred<void>()
    const admitted = deferred<void>()
    const request = f.request()
    const file = f.session.beginFile({ ...request, openRevision: async () => {
      admitted.resolve(); await opening.promise; return request.openRevision()
    } })
    await admitted.promise
    let closed = false
    const stopping = f.session.closeForStopSettlement().then(() => { closed = true })
    await expect(f.session.beginFile(f.request(12))).rejects.toThrow(/closed/)
    await expect(f.session.saveStagedFiles()).rejects.toThrow(/closed/)
    expect(closed).toBe(false)
    opening.resolve()
    await file
    await stopping
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe('discarded')
    expect(() => f.stageTree.file(STAGE_PATH)).toThrow()
  })

  it('does not classify or release a stage until its accepted write and current checkpoint drain', async () => {
    const f = await deliveryEngineFixture()
    const barrier = f.stageTree.deferFileWrite(STAGE_PATH)
    const file = await f.session.beginFile(f.request())
    const writing = file.writeRange(0n, CONTENT.slice(0, 2))
    await barrier.accepted
    const discard = vi.spyOn(f.stage, 'discard')
    const stopping = f.session.closeForStopSettlement()
    await Promise.resolve()
    expect(discard).not.toHaveBeenCalled()
    expect(f.phases.get(FILE_ID)).toBe('receiving')
    barrier.release()
    await writing
    await stopping
    expect(discard).toHaveBeenCalledWith(expect.objectContaining({ fileId: FILE_ID }),
      expect.objectContaining({ verifiedRanges: [{ start: 0n, end: 2n }] }))
    expect(f.phases.get(FILE_ID)).toBe('released')
  })

  it('drains an accepted local copy and keeps its confirmed target bytes', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT)
    const entered = deferred<void>()
    const release = deferred<void>()
    f.onTargetWrite(async () => { entered.resolve(); await release.promise })
    const saving = file.commit()
    await entered.promise
    let closed = false
    const stopping = f.session.closeForStopSettlement().then(() => { closed = true })
    await expect(f.session.saveStagedFiles()).rejects.toThrow(/closed/)
    expect(closed).toBe(false)
    expect(f.session.summary.copyingFiles).toBe(1)
    release.resolve()
    await saving
    await stopping
    expect(f.session.summary).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 4n, discardedFiles: 0 })
    expect(f.targetTree.file(['nested', 'file-11.bin']).snapshot()).toEqual(CONTENT)
  })

  it('preserves a complete authoritative checkpoint when the delivery journal still says receiving', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT)
    f.repository.failTransition = 'staged-complete'
    await expect(file.commit()).rejects.toThrow(/journal/)
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe('receiving')
    await f.session.closeForStopSettlement()
    expect(f.session.summary).toMatchObject({ stagedBytes: 4n, stagedCompleteFiles: 1, targetSavedBytes: 0n })
    expect(f.stageTree.file(STAGE_PATH).snapshot()).toEqual(CONTENT)
    expect(f.events).toContain('stop-staging-preserved')
    const resumed = await f.reopen()
    await resumed.saveStagedFiles()
    expect(f.targetTree.file(['nested', 'file-11.bin']).snapshot()).toEqual(CONTENT)
    await resumed.close()
  })

  it('retains failed deletion as discoverable cleanup while continuing other stopped-file disposal', async () => {
    const f = await deliveryEngineFixture()
    const first = await f.session.beginFile(f.request())
    const second = await f.session.beginFile(f.request(12))
    await first.writeRange(0n, CONTENT.slice(0, 2))
    await second.writeRange(0n, CONTENT.slice(0, 2))
    f.failDelete()
    await f.session.closeForStopSettlement()
    await f.session.close()
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe('discarding')
    expect(f.repository.files.get(deliveryIdentity(12, 16))?.state.kind).toBe('discarded')
    expect(f.phases.get(FILE_ID)).not.toBe('released')
    expect(f.session.summary).toMatchObject({ localContinuation: 'retry-staging-cleanup', reservedStagingBytes: 4n })
    const cleanup = await f.reopenCleanup()
    await cleanup.cleanupStaging()
    expect(cleanup.getSummary()).toMatchObject({ reservedStagingBytes: 0n, stagedBytes: 0n })
    await cleanup.close()
  })

  it.each(['discarding', 'discarded'] as const)('recovers a journal failure at %s across reopen', async transition => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT.slice(0, 2))
    f.repository.failTransition = transition
    await f.session.closeForStopSettlement()
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe(transition === 'discarding' ? 'receiving' : 'discarding')
    expect(f.phases.get(FILE_ID) === 'released').toBe(transition === 'discarded')
    if (transition === 'discarding') expect(f.stageTree.file(STAGE_PATH).snapshot()).toEqual(CONTENT.slice(0, 2))
    else expect(() => f.stageTree.file(STAGE_PATH)).toThrow()
    const cleanup = await f.reopenCleanup()
    await cleanup.discardIncompleteStaging()
    expect(cleanup.getSummary()).toMatchObject({ reservedStagingBytes: 0n, discardedFiles: 1 })
    expect(f.phases.get(FILE_ID)).toBe('released')
    await cleanup.close()
  })

  it('holds reservation and deletion until an existing staging reader releases', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT.slice(0, 2))
    const reader = await acquireArtifactReader(f.policy.staging!.operationId)
    const entered = deferred<void>()
    const original = f.stage.discard
    vi.spyOn(f.stage, 'discard').mockImplementation(async (...args) => { entered.resolve(); await original(...args) })
    const stopping = f.session.closeForStopSettlement()
    await entered.promise
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe('discarding')
    expect(f.phases.get(FILE_ID)).not.toBe('released')
    expect(f.stageTree.file(STAGE_PATH).snapshot()).toEqual(CONTENT.slice(0, 2))
    reader.release()
    await stopping
    expect(f.phases.get(FILE_ID)).toBe('released')
  })

  it('retains bytes and capacity when writer drain fails and permits later owned cleanup', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT.slice(0, 2))
    const physical = f.stageTree.file(STAGE_PATH)
    vi.spyOn(f.stage, 'readCheckpoint').mockRejectedValueOnce(new Error('checkpoint unavailable'))
    await expect(f.session.closeForStopSettlement()).rejects.toThrow(/close failed/)
    expect(physical.abortCount).toBe(1)
    expect(physical.snapshot()).toEqual(CONTENT.slice(0, 2))
    expect(f.phases.get(FILE_ID)).not.toBe('released')
    expect(f.events).not.toContain('discard-started')
    const cleanup = await f.reopenCleanup()
    await cleanup.discardIncompleteStaging()
    expect(f.phases.get(FILE_ID)).toBe('released')
    await cleanup.close()
  })

  it('persists retry authority when capacity release fails after verified deletion', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT.slice(0, 2))
    vi.spyOn(f.holds.get(FILE_ID)!, 'releaseDiscarded').mockRejectedValueOnce(new Error('budget journal unavailable'))
    await f.session.closeForStopSettlement()
    expect(() => f.stageTree.file(STAGE_PATH)).toThrow()
    expect(f.repository.files.get(FILE_ID)?.state.kind).toBe('discarding')
    expect(f.session.summary.localContinuation).toBe('retry-staging-cleanup')
    const cleanup = await f.reopenCleanup()
    await cleanup.cleanupStaging()
    expect(f.phases.get(FILE_ID)).toBe('released')
    expect(cleanup.getSummary().reservedStagingBytes).toBe(0n)
    await cleanup.close()
  })

  it.each(['matching-prefix', 'foreign-content'] as const)('verifies %s before a retained target can be truncated', async existing => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request())
    await file.writeRange(0n, CONTENT)
    await file.checkpoint()
    await f.session.closeForStopSettlement()
    const target = f.targetTree.file(['nested', 'file-11.bin'])
    const original = existing === 'matching-prefix' ? CONTENT.slice(0, 2) : Uint8Array.of(9, 8, 7)
    await target.writeAt(0n, original)
    const resumed = await f.reopen()
    if (existing === 'matching-prefix') {
      await resumed.saveStagedFiles()
      expect(target.snapshot()).toEqual(CONTENT)
      expect(resumed.summary).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 4n })
    } else {
      for (let attempt = 0; attempt < 2; attempt += 1) {
        await expect(resumed.saveStagedFiles()).rejects.toThrow(/ownership/)
        expect(target.snapshot()).toEqual(original)
        expect(resumed.summary).toMatchObject({ stagedBytes: 4n, reservedStagingBytes: 4n, targetSavedBytes: 0n })
      }
    }
    await resumed.close()
  })

  it('repairs previously stopped receiving records locally while preserving complete staging', async () => {
    const f = await deliveryEngineFixture()
    const incomplete = await f.session.beginFile(f.request())
    const complete = await f.session.beginFile(f.request(12))
    await incomplete.writeRange(0n, CONTENT.slice(0, 2))
    await complete.writeRange(0n, CONTENT)
    f.onTargetWrite(async () => { throw new Error('destination unavailable') })
    await expect(complete.commit()).rejects.toThrow(/destination/)
    await f.session.close()
    const cleanup = await f.reopenCleanup()
    await cleanup.discardIncompleteStaging()
    expect(cleanup.getSummary()).toMatchObject({ discardedFiles: 1, stagedCompleteFiles: 1, targetSavedBytes: 0n })
    expect(f.stageTree.file(browserDeliveryStagingPath(deliveryIdentity(12, 16))).snapshot()).toEqual(CONTENT)
    await cleanup.close()
  })
})
