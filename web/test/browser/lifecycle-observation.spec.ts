import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

test('observes only committed lifecycle transitions from the owned IndexedDB handle', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const proof = await page.evaluate(async () => {
    const repositoryPath = '/src/output/browser/indexeddb/receive-operation-repository.ts'
    const statePath = '/src/output/workspace/state.ts'
    const codecPath = '/src/output/workspace/state-codec.ts'
    const bytesPath = '/src/crypto/bytes.ts'
    const { IndexedDbReceiveOperationRepository } = await import(repositoryPath) as
      typeof import('../../src/output/browser/indexeddb/receive-operation-repository')
    const { initialReceiveLifecycleState, nextReceiveLifecycleState } = await import(statePath) as
      typeof import('../../src/output/workspace/state')
    const { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } = await import(codecPath) as
      typeof import('../../src/output/workspace/state-codec')
    const { encodeBase64Url } = await import(bytesPath) as typeof import('../../src/crypto/bytes')
    const identity = (fill: number, width = 16) => encodeBase64Url(new Uint8Array(width).fill(fill))
    const databaseName = 'lifecycle-observation-' + crypto.randomUUID()
    const owner = await IndexedDbReceiveOperationRepository.open(databaseName)
    const reader = await IndexedDbReceiveOperationRepository.open(databaseName)
    const observed: string[] = []
    const committedReads: Promise<string>[] = []
    let otherHandleNotifications = 0
    let rejection = ''
    try {
      reader.subscribeLifecycle(() => { otherHandleNotifications += 1 })
      owner.subscribeLifecycle(() => { throw new Error('Broken view must not fail a committed write') })
      const unsubscribe = owner.subscribeLifecycle(state => {
        observed.push(state.kind)
        committedReads.push(reader.readLifecycle(state.operationId).then(record => {
          if (record === undefined) throw new Error('Notification preceded the durable commit')
          return decodeStoredReceiveLifecycleState(record).kind
        }))
      })
      const initial = initialReceiveLifecycleState({
        operationId: identity(1), receiveIntentDigest: identity(2, 32),
      })
      await owner.commitTransition({ operationId: initial.operationId, lifecycle: initial })
      await Promise.all(committedReads)
      const receiving = nextReceiveLifecycleState(initial, { kind: 'receiving', activeLeaseId: identity(3) })
      // Some authorities supply canonical records instead of the convenience lifecycle field.
      await owner.commitTransition({ operationId: initial.operationId,
        expectedLifecycleGeneration: initial.generation, records: [await storedReceiveLifecycleState(receiving)] })
      await Promise.all(committedReads)
      try {
        await owner.commitTransition({ operationId: initial.operationId,
          expectedLifecycleGeneration: initial.generation, lifecycle: receiving })
      } catch (error) { rejection = error instanceof DOMException ? error.name : String(error) }
      await owner.commitTransition({ operationId: initial.operationId })
      unsubscribe()
      await owner.commitTransition({ operationId: initial.operationId,
        expectedLifecycleGeneration: receiving.generation,
        lifecycle: nextReceiveLifecycleState(receiving, { kind: 'finalizing-tree', activeLeaseId: identity(3) }) })
      return { observed, committedReads: await Promise.all(committedReads), otherHandleNotifications, rejection }
    } finally {
      owner.close()
      reader.close()
      await new Promise<void>((resolve, reject) => {
        const deletion = indexedDB.deleteDatabase(databaseName)
        deletion.onsuccess = () => resolve()
        deletion.onerror = () => reject(deletion.error)
        deletion.onblocked = () => reject(new Error('Test repository stayed open'))
      })
    }
  })
  expect(proof).toEqual({
    observed: ['intent-frozen', 'receiving'],
    committedReads: ['intent-frozen', 'receiving'],
    otherHandleNotifications: 0,
    rejection: 'InvalidStateError',
  })
})
