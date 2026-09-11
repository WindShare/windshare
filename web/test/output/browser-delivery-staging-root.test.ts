import { describe, expect, it } from 'vitest'
import { BrowserDeliveryStagingRoot } from '../../src/output/browser-delivery/staging-root'
import type { PersistentHandleRecord, PersistentHandleRepository } from '../../src/output/persistence/journal'
import { deliveryPolicy } from './browser-delivery-fixture'
import { MemoryDirectory } from './file-system-access-memory-fs'

class RootHandles implements PersistentHandleRepository {
  readonly records = new Map<string, PersistentHandleRecord>()
  failFinalRoot = false
  failReceiptDelete = false
  async readHandle(id: string) { return this.records.get(id) }
  async putHandle(record: PersistentHandleRecord) {
    if (this.failFinalRoot && !record.id.endsWith('/creation')) {
      this.failFinalRoot = false
      throw new DOMException('root authority commit failed', 'QuotaExceededError')
    }
    for (const existing of this.records.values()) {
      if (existing.id !== record.id && existing.operationId === record.operationId && existing.ownedObjectId === record.ownedObjectId) {
        throw new DOMException('Duplicate operation owned object', 'ConstraintError')
      }
    }
    this.records.set(record.id, record)
  }
  async deleteHandle(id: string) {
    if (this.failReceiptDelete) { this.failReceiptDelete = false; throw new Error('receipt cleanup failed') }
    this.records.delete(id)
  }
}

function fixture() {
  const parent = new MemoryDirectory('opfs')
  const handles = new RootHandles()
  const policy = deliveryPolicy()
  const open = () => BrowserDeliveryStagingRoot.open({ parent: parent as unknown as FileSystemDirectoryHandle,
    handles, binding: policy.staging!, parentOperationId: policy.operationId })
  return { parent, handles, open, rootName: 'windshare-file-staging-v1-' + policy.staging!.operationId }
}

describe('staging root creation authority', () => {
  it('reopens the empty root after OPFS creation precedes a failed durable root-handle commit', async () => {
    const f = fixture()
    f.handles.failFinalRoot = true
    await expect(f.open()).rejects.toThrow('root authority commit failed')
    expect(f.parent.directoryNames()).toEqual([f.rootName])
    expect([...f.handles.records.keys()].every(id => id.endsWith('/creation'))).toBe(true)
    const root = await f.open()
    await root.authorize()
    expect(f.handles.records.size).toBe(1)
  })

  it('reopens a committed root after receipt deletion fails', async () => {
    const f = fixture()
    f.handles.failReceiptDelete = true
    await expect(f.open()).rejects.toThrow('receipt cleanup failed')
    expect(f.handles.records.size).toBe(2)
    expect(new Set([...f.handles.records.values()].map(record => record.ownedObjectId)).size).toBe(2)
    await (await f.open()).authorize()
    expect(f.handles.records.size).toBe(1)
  })

  it('never adopts a preexisting directory without prior creation authority', async () => {
    const f = fixture()
    await f.parent.getDirectoryHandle(f.rootName, { create: true })
    await expect(f.open()).rejects.toThrow()
    expect(f.handles.records.size).toBe(0)
  })

  it('rejects a nonempty interrupted root because receiving could not have started before root authority committed', async () => {
    const f = fixture()
    f.handles.failFinalRoot = true
    await expect(f.open()).rejects.toThrow()
    const root = await f.parent.getDirectoryHandle(f.rootName)
    await root.getFileHandle('unrelated', { create: true })
    await expect(f.open()).rejects.toThrow()
    expect(await root.getFileHandle('unrelated')).toBeDefined()
  })
})
