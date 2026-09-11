import { describe, expect, it, vi } from 'vitest'
import { acquireArtifactReader } from '../../src/output/origin-private/export-readers'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'
import { deferred } from './persistent-tree-file-fixture'

describe('browser folder per-file staged delivery', () => {
  it('commits small files while one complete staged file is still copying, and preserves target proof identity', async () => {
    const f = await deliveryEngineFixture()
    const copy = deferred<void>()
    const started = deferred<void>()
    f.onTargetWrite(async request => { if (request.materializationRelativePath.at(-1) === 'file-11.bin') { started.resolve(); await copy.promise } })
    const large = await f.session.beginFile(f.request())
    expect(large.checkpointObjectId).not.toBe(large.ownedObjectId)
    await large.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    let committed = false
    const saving = large.commit().then(result => { committed = true; return result })
    await started.promise
    expect(f.session.summary).toMatchObject({ copyingFiles: 1, targetSavedBytes: 0n, stagedBytes: 4n })
    const small = await f.session.beginFile(f.request(12, 2n))
    await small.writeRange(0n, Uint8Array.of(8, 9))
    const smallCommit = await small.commit()
    expect(smallCommit.operationId).toBe(f.policy.operationId)
    expect(committed).toBe(false)
    expect(f.session.summary.targetSavedBytes).toBe(2n)
    f.setNow(45)
    copy.resolve()
    const largeCommit = await saving
    expect(largeCommit.operationId).toBe(f.policy.operationId)
    expect(largeCommit.ownedObjectId).toBe(large.ownedObjectId)
    expect(f.targetTree.file(['nested', 'file-11.bin']).snapshot()).toEqual(Uint8Array.of(1, 2, 3, 4))
    expect(f.session.summary).toMatchObject({ targetSavedFiles: 2, stagedBytes: 0n })
    expect(f.events.indexOf('budget-saved')).toBeLessThan(f.events.indexOf('delete-stage'))
    expect(f.events.indexOf('delete-stage')).toBeLessThan(f.events.indexOf('budget-released'))
    await f.session.close()
  })

  it.each([2, 4])('keeps authenticated source, internal stage, and single-file target coordinates independent for %i bytes', async size => {
    const f = await deliveryEngineFixture()
    const request = { ...f.request(11, BigInt(size), []), sourceAuthenticationPath: ['shared', 'source.bin'] }
    const file = await f.session.beginFile(request)
    const bytes = new Uint8Array(size).fill(9)
    await file.writeRange(0n, bytes)
    await file.commit()
    const record = f.repository.files.get(deliveryIdentity(11, 16))!
    expect(record.source.canonicalPath).toEqual(['shared', 'source.bin'])
    expect(record.materializationRelativePath).toEqual([])
    expect(f.targetTree.file([]).snapshot()).toEqual(bytes)
    if (size > 2) {
      const stage = (await f.stageCheckpoints.scanCommitted({ direction: 'ascending' })).records[0]!
      expect(stage.canonicalPath).toEqual([record.fileId])
    }
    await f.session.close()
  })

  it('retains complete staging on copy failure and reopens an offline local retry without source calls', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    const sourceOpen = vi.fn(request.openRevision)
    const transaction = await f.session.beginFile({ ...request, openRevision: sourceOpen })
    await transaction.writeRange(0n, Uint8Array.of(3, 4, 5, 6))
    f.onTargetWrite(async () => { throw new DOMException('destination offline', 'NotAllowedError') })
    await expect(transaction.commit()).rejects.toThrow('destination offline')
    expect(f.session.summary).toMatchObject({ stagedCompleteFiles: 1, targetSavedBytes: 0n, stagedBytes: 4n })
    expect(f.phases.get(deliveryIdentity(11, 16))).toBe('failed')
    await f.session.close()
    f.onTargetWrite(undefined)
    const recovered = await f.reopen()
    await recovered.saveStagedFiles()
    expect(sourceOpen).toHaveBeenCalledTimes(1)
    expect(recovered.summary).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 4n })
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(3, 4, 5, 6))
    await recovered.close()
  })

  it('keeps a saved target successful when cleanup fails and waits live export readers before releasing staging', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(1, 1, 1, 1))
    f.failDelete()
    await transaction.commit()
    expect(f.session.summary).toMatchObject({ cleanupPendingFiles: 1, targetSavedBytes: 4n, stagedBytes: 4n })
    const reader = await acquireArtifactReader(f.policy.staging!.operationId)
    let cleaned = false
    const cleanup = f.session.cleanupStaging().then(() => { cleaned = true })
    await Promise.resolve(); await Promise.resolve()
    expect(cleaned).toBe(false)
    expect(f.phases.get(deliveryIdentity(11, 16))).toBe('saved')
    reader.release()
    await cleanup
    expect(f.session.summary.stagedBytes).toBe(0n)
    await f.session.close()
  })

  it('reconciles a complete stage after its delivery-journal commit failed and copies locally', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(2, 2, 2, 2))
    f.repository.failTransition = 'staged-complete'
    await expect(transaction.commit()).rejects.toThrow('journal failure')
    await f.session.close()
    const recovered = await f.reopen()
    expect(recovered.summary.stagedCompleteFiles).toBe(1)
    await recovered.saveStagedFiles()
    expect(recovered.summary.targetSavedBytes).toBe(4n)
    await recovered.close()
  })

  it('recognizes a target already committed before the delivery journal failed and never copies it again', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(4, 4, 4, 4))
    f.repository.failTransition = 'target-saved'
    await expect(transaction.commit()).rejects.toThrow('journal failure')
    const writes = vi.fn(async () => undefined)
    f.onTargetWrite(writes)
    await f.session.close()
    const recovered = await f.reopen()
    await recovered.saveStagedFiles()
    expect(writes).not.toHaveBeenCalled()
    expect(recovered.summary).toMatchObject({ targetSavedBytes: 4n, stagedBytes: 0n })
    await recovered.close()
  })

  it('does not complete incomplete staging offline and preserves revision checks when receiving resumes', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    const transaction = await f.session.beginFile(request)
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await transaction.pause()
    await f.session.close()
    const recovered = await f.reopen()
    await recovered.saveStagedFiles()
    expect(recovered.summary).toMatchObject({ receivingFiles: 1, recoverableBytes: 2n, targetSavedBytes: 0n })
    await expect(recovered.beginFile({ ...request, openRevision: async () => ({ ...await request.openRevision(), fileRevision: deliveryIdentity(40, 16) }) })).rejects.toThrow('original authenticated')
    await recovered.close()
  })

  it('finishes a complete direct checkpoint locally before publishing its missing final target ledger proof', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request(11, 2n)
    const transaction = await f.session.beginFile(request)
    await transaction.writeRange(0n, Uint8Array.of(8, 9))
    await transaction.pause()
    expect(f.targetCheckpoints.commitFinalFileCount).toBe(0)
    await f.session.close()
    const reopened = await f.reopen()
    expect(f.targetCheckpoints.commitFinalFileCount).toBe(1)
    expect(reopened.summary.targetSavedBytes).toBe(2n)
    await reopened.close()
  })

  it('persists explicit redownload authority before resetting direct receiving coverage and reopens normally', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request(11, 2n)
    const original = await f.session.beginFile(request)
    await original.writeRange(0n, Uint8Array.of(8))
    await f.session.close()
    const resumed = await f.reopen()
    const restarted = await resumed.beginFile({ ...request, recovery: { pausedFile: 'restart-owned-file' } })
    expect(restarted.initialDurableRanges).toEqual([])
    await restarted.pause()
    await resumed.close()
    const reopened = await f.reopen()
    const continuation = await reopened.beginFile(request)
    expect(continuation.initialDurableRanges).toEqual([])
    await continuation.writeRange(0n, Uint8Array.of(1, 2))
    await continuation.commit()
    expect(f.targetTree.file(request.materializationRelativePath).snapshot()).toEqual(Uint8Array.of(1, 2))
    expect(f.repository.transitions).toContain('restart-authorized')
    await reopened.close()
  })

  it('completes a persisted restart after process loss before physical target reset', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request(11, 2n)
    const original = await f.session.beginFile(request)
    await original.writeRange(0n, Uint8Array.of(8))
    await f.session.close()
    const prior = f.repository.files.get(deliveryIdentity(11, 16))!
    const checkpoint = (await f.targetCheckpoints.scanCommitted({ direction: 'ascending' })).records[0]!
    await f.repository.authorizeRestart(prior, checkpoint, 'explicit-redownload-before-crash')
    const reopened = await f.reopen()
    expect(reopened.summary.recoverableBytes).toBe(0n)
    const resumed = await reopened.beginFile(request)
    expect(resumed.initialDurableRanges).toEqual([])
    await reopened.close()
  })

  it('recovers a reset committed before the restart marker could be retired', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request(11, 2n)
    const original = await f.session.beginFile(request)
    await original.writeRange(0n, Uint8Array.of(8))
    await f.session.close()
    const resumed = await f.reopen()
    f.repository.failTransition = 'receiving'
    await expect(resumed.beginFile({ ...request, recovery: { pausedFile: 'restart-owned-file' } })).rejects.toThrow('journal failure')
    expect(f.repository.files.get(deliveryIdentity(11, 16))?.state.kind).toBe('restart-authorized')
    await resumed.close()
    const reopened = await f.reopen()
    expect(reopened.summary.recoverableBytes).toBe(0n)
    const file = await reopened.beginFile(request)
    await file.writeRange(0n, Uint8Array.of(1, 2))
    await file.commit()
    expect(reopened.summary.targetSavedBytes).toBe(2n)
    await reopened.close()
  })

  it('rejects an unrelated occupied target before accepting staged bytes', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    f.targetTree.occupy(request.materializationRelativePath, deliveryIdentity(60))
    await expect(f.session.beginFile(request)).rejects.toThrow()
    expect((await f.stageCheckpoints.scanCommitted({ direction: 'ascending' })).records).toEqual([])
    await f.session.close()
  })

  it('retries the same completed stage after ending the old target and clearing its uncommitted full content', async () => {
    const f = await deliveryEngineFixture()
    const request = f.request()
    const transaction = await f.session.beginFile(request)
    await transaction.writeRange(0n, Uint8Array.of(5, 6, 7, 8))
    const target = f.targetTree.file(request.materializationRelativePath)
    target.failNextFlush()
    await expect(transaction.commit()).rejects.toThrow('simulated writer close failure')
    expect(target.snapshot()).toHaveLength(4)
    const beforeWrite: number[] = []
    f.onTargetWrite(async () => { beforeWrite.push(target.snapshot().byteLength) })
    await transaction.commit()
    expect(beforeWrite).toEqual([0])
    expect(target.writerModes.every(mode => mode === 'truncate')).toBe(true)
    expect(target.snapshot()).toEqual(Uint8Array.of(5, 6, 7, 8))
    await f.session.close()
  })

  it('retains a complete second file when its queued export is cancelled', async () => {
    const f = await deliveryEngineFixture()
    const first = await f.session.beginFile(f.request(11))
    const second = await f.session.beginFile(f.request(12))
    await first.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    await second.writeRange(0n, Uint8Array.of(4, 3, 2, 1))
    const started = deferred<void>()
    const release = deferred<void>()
    f.onTargetWrite(async request => { if (request.materializationRelativePath.at(-1) === 'file-11.bin') { started.resolve(); await release.promise } })
    const firstSave = first.commit()
    await started.promise
    const controller = new AbortController()
    const queued = deferred<void>()
    const unsubscribe = f.session.subscribe(summary => { if (summary.stagedCompleteFiles > 0) queued.resolve() })
    const secondSave = second.commit(controller.signal)
    const result = expect(secondSave).rejects.toThrow()
    await queued.promise
    unsubscribe()
    controller.abort(new DOMException('cancel queued save', 'AbortError'))
    release.resolve()
    await firstSave
    await result
    expect(f.session.summary).toMatchObject({ stagedBytes: 4n, targetSavedBytes: 4n })
    await f.session.close()
  })

  it('cleans retained saved staging after destination authority becomes unavailable', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    f.failDelete()
    await transaction.commit()
    await f.session.close()
    const cleanup = await f.reopenCleanup()
    await cleanup.cleanupStaging()
    expect(cleanup.getSummary()).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 4n })
    await cleanup.close()
  })

  it('discards retained incomplete staging without opening a destination', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await f.session.close()
    const cleanup = await f.reopenCleanup()
    await cleanup.discardStaging()
    expect(cleanup.getSummary()).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 0n })
    await cleanup.close()
  })

  it('explicitly discards incomplete owned staging without claiming the target was saved', async () => {
    const f = await deliveryEngineFixture()
    const transaction = await f.session.beginFile(f.request())
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await f.session.discardStaging()
    expect(f.repository.files.get(deliveryIdentity(11, 16))?.state.kind).toBe('discarded')
    expect(f.session.summary).toMatchObject({ stagedBytes: 0n, targetSavedBytes: 0n })
    expect(f.phases.get(deliveryIdentity(11, 16))).toBe('released')
    await f.session.close()
  })
})
