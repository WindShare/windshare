import { describe, expect, it, vi } from 'vitest'
import { IndexedDbBrowserDeliveryRepository } from '../../src/output/browser-delivery/indexeddb'
import { readBrowserDeliveryRecoverySummary } from '../../src/output/browser-delivery/recovery/inventory'
import { deriveBrowserDeliveryLifecycle, type LocalFileSetLifecycle } from '../../src/output/browser-delivery/recovery/authority'
import { snapshotBrowserDeliveryRecord } from '../../src/output/browser-delivery/records'
import { createFSARecoveryCheckpointSnapshot, deriveFSARecoverySummary } from '../../src/output/file-system-access/recovery-summary'
import { scanAllFSAFileCheckpoints } from '../../src/output/file-system-access/checkpoint-repository'
import { newFileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import type { DirectTreeIntent } from '../../src/output/file-system-access/settlement-proof'
import { deliveryEngineFixture } from './browser-delivery-engine-fixture'
import { deliveryIdentity } from './browser-delivery-fixture'
import { localDeliveryFixture } from './browser-delivery-local-fixture'

describe('browser local delivery lifecycle authority', () => {
  it.each([3n, 8n])('recovers a crash after target checkpoint %s before journal advancement', async end => {
    const f = await localDeliveryFixture()
    const record = snapshotBrowserDeliveryRecord(f.policy, {
      ...f.initial, generation: 2n,
      localMutation: { lifecycleGeneration: f.lifecycle.generation, checkpointSetDigest: f.lifecycle.checkpointSetDigest,
        priorTargetCheckpoint: f.baseline },
    })
    const checkpoints = [f.target(end, 2n)]
    const next = await deriveBrowserDeliveryLifecycle({ ...f, records: [record], checkpoints })
    expect(next.generation).toBe(8n)
    expect(next.completedBytes).toBe(end === 8n ? 8n : 0n)
    const snapshot = await createFSARecoveryCheckpointSnapshot(f.intent, next.generation, checkpoints)
    await expect(deriveFSARecoverySummary({ intent: f.intent, lifecycle: next, snapshot })).resolves.toMatchObject({
      verifiedPartialBytes: end === 8n ? 0n : end,
    })
    expect(await deriveBrowserDeliveryLifecycle({ ...f, lifecycle: next, records: [record], checkpoints })).toBe(next)
  })

  it('does not authorize unrelated target changes, ownership replacement, or changed selection totals', async () => {
    const f = await localDeliveryFixture()
    const record = snapshotBrowserDeliveryRecord(f.policy, { ...f.initial, generation: 2n, localMutation: {
      lifecycleGeneration: 7n, checkpointSetDigest: f.lifecycle.checkpointSetDigest, priorTargetCheckpoint: f.baseline,
    } })
    await expect(deriveBrowserDeliveryLifecycle({ ...f, records: [], checkpoints: [f.target(8n, 2n)] })).rejects.toThrow(/authorization/)
    const foreign = newFileCheckpointV2({ ...f.target(8n, 2n), ownedObjectId: deliveryIdentity(81) })
    await expect(deriveBrowserDeliveryLifecycle({ ...f, records: [record], checkpoints: [foreign] })).rejects.toThrow(/ownership/)
    const unrelated = newFileCheckpointV2({ ...f.target(8n, 2n), fileId: deliveryIdentity(82, 16),
      canonicalPath: ['extra.bin'], ownedObjectId: deliveryIdentity(83) })
    await expect(deriveBrowserDeliveryLifecycle({ ...f, records: [record], checkpoints: [f.target(8n, 2n), unrelated] })).rejects.toThrow(/digest/)
    await expect(deriveBrowserDeliveryLifecycle({ ...f, lifecycle: { ...f.lifecycle, completedFileCount: 1n, completedBytes: 8n },
      records: [record], checkpoints: [f.target(8n, 2n)] })).rejects.toThrow(/totals/)
  })

  it('keeps inventory coherent when actual local copy commits after its checkpoint scan and rejects changed lineage', async () => {
    const f = await deliveryEngineFixture()
    const file = await f.session.beginFile(f.request(11, 4n))
    await file.writeRange(0n, Uint8Array.of(1, 2, 3, 4))
    f.onTargetWrite(async () => { throw new Error('destination unavailable') })
    await expect(file.commit()).rejects.toThrow(/destination/)
    await f.session.close()
    const intent = { operationId: f.policy.operationId, digest: f.policy.receiveIntentDigest,
      plan: { kind: 'direct-tree', reservation: {
        digest: f.policy.target.materializationBindingDigest, authorityRef: f.policy.target.authorityRef,
      } } } as DirectTreeIntent
    const baseline = await scanAllFSAFileCheckpoints(f.targetCheckpoints, 'committed')
    const old = await createFSARecoveryCheckpointSnapshot(intent, 7n, baseline)
    const lifecycle: LocalFileSetLifecycle = {
      kind: 'resumable-receive', payloadKind: 'file-set', operationId: intent.operationId, receiveIntentDigest: intent.digest,
      generation: 7n, checkpointSetDigest: old.checkpointSetDigest, completedFileCount: 0n, completedBytes: 0n,
      selectionFacts: { discoveredFileCount: 2n, discoveredBytes: 8n, discovery: 'failed' },
    }
    for (const record of f.repository.files.values()) f.repository.files.set(record.fileId,
      snapshotBrowserDeliveryRecord(f.policy, { ...record, generation: record.generation + 1n, localMutation: {
        lifecycleGeneration: 7n, checkpointSetDigest: old.checkpointSetDigest, priorTargetCheckpoint: baseline[0]!,
      } }))
    f.onTargetWrite(undefined)
    const reopened = await f.reopen()
    const open = vi.spyOn(IndexedDbBrowserDeliveryRepository, 'open')
      .mockResolvedValue(f.repository as unknown as IndexedDbBrowserDeliveryRepository)
    const originalScan = f.targetCheckpoints.scanCommitted.bind(f.targetCheckpoints)
    let injected = false
    const scan = vi.spyOn(f.targetCheckpoints, 'scanCommitted').mockImplementation(async request => {
      const page = await originalScan(request)
      if (!injected) {
        injected = true
        await reopened.saveStagedFiles()
      }
      return page
    })
    const input = { intent, lifecycle, checkpoints: f.targetCheckpoints, databaseName: 'local-copy-race' }
    try {
      await expect(readBrowserDeliveryRecoverySummary(input)).resolves.toMatchObject({
        completedBytes: 0n, completedFileCount: 0n, checkpointSetDigest: old.checkpointSetDigest,
      })
      expect(injected).toBe(true)
      expect(scan.mock.calls.filter(([request]) => request.fileId === undefined)).toHaveLength(1)
      expect(reopened.getSummary().targetSavedBytes).toBe(4n)
      await expect(readBrowserDeliveryRecoverySummary(input)).resolves.toBeUndefined()
      scan.mockImplementation(async request => {
        const page = await originalScan(request)
        return { ...page, records: page.records.map(checkpoint => newFileCheckpointV2({
          ...checkpoint, fileRevision: deliveryIdentity(91, 16),
        })) }
      })
      await expect(readBrowserDeliveryRecoverySummary(input)).rejects.toThrow(/ownership|authenticated source/)
    } finally {
      scan.mockRestore()
      open.mockRestore()
      await reopened.close()
    }
    const checkpoints = await scanAllFSAFileCheckpoints(f.targetCheckpoints, 'committed')
    const next = await deriveBrowserDeliveryLifecycle({ intent, lifecycle, policy: f.policy,
      records: [...f.repository.files.values()], checkpoints })
    expect(next).toMatchObject({ generation: 8n, completedBytes: 4n, completedFileCount: 1n })
    expect(next.selectionFacts).toEqual(lifecycle.selectionFacts)
    expect(reopened.getSummary().targetSavedBytes).toBe(4n)
  })
})
