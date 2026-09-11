import { IndexedDbFileCheckpointRepository } from '../../src/output/browser/indexeddb-repository'
import { BrowserDeliveryStagingRoot } from '../../src/output/browser-delivery/staging-root'
import type { PersistentHandleRepository } from '../../src/output/persistence/journal'
import { deliveryPolicy } from '../output/browser-delivery-fixture'

export async function prepareStagingRootPromotion(databaseName: string, parentName: string) {
  const policy = deliveryPolicy()
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(parentName, { create: true })
  const repository = await IndexedDbFileCheckpointRepository.open(policy.staging!, databaseName)
  const handles: PersistentHandleRepository = {
    readHandle: id => repository.readHandle(id),
    putHandle: record => repository.putHandle(record),
    deleteHandle: async () => { throw new Error('injected receipt retirement failure') },
  }
  try {
    try {
      await BrowserDeliveryStagingRoot.open({ binding: policy.staging!, parentOperationId: policy.operationId, parent, handles })
      throw new Error('expected receipt retirement cut')
    } catch (error) {
      if (!(error instanceof Error) || error.message !== 'injected receipt retirement failure') throw error
    }
    const retained = await repository.listHandles()
    return { retainedHandles: retained.length, uniqueOwnedObjects: new Set(retained.map(record => record.ownedObjectId)).size }
  } finally { repository.close() }
}

export async function finishStagingRootPromotion(databaseName: string, parentName: string) {
  const policy = deliveryPolicy()
  const opfs = await navigator.storage.getDirectory()
  const parent = await opfs.getDirectoryHandle(parentName)
  const repository = await IndexedDbFileCheckpointRepository.open(policy.staging!, databaseName)
  try {
    const root = await BrowserDeliveryStagingRoot.open({ binding: policy.staging!, parentOperationId: policy.operationId, parent, handles: repository })
    await root.authorize()
    const retained = await repository.listHandles()
    return { retainedHandles: retained.length, finalRootObject: retained[0]?.ownedObjectId === policy.staging!.authorityRef }
  } finally {
    repository.close()
    await opfs.removeEntry(parentName, { recursive: true })
    await new Promise<void>((resolve, reject) => {
      const deletion = indexedDB.deleteDatabase(databaseName)
      deletion.onsuccess = () => resolve()
      deletion.onerror = () => reject(deletion.error)
    })
  }
}
