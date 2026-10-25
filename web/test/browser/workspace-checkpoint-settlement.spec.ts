import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

test('archive settlement atomically rejects stale content evidence without releasing its lease', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async () => {
    const repositoryPath = '/src/output/browser/indexeddb-repository.ts'
    const checkpointPath = '/src/output/origin-private/task-checkpoint/indexeddb-store.ts'
    const statePath = '/src/transfer/lifecycle/state.ts'
    const recordsPath = '/src/output/workspace/records.ts'
    const codecPath = '/src/output/workspace/state-codec.ts'
    const { IndexedDbReceiveOperationRepository } = await import(repositoryPath) as typeof import('../../src/output/browser/indexeddb-repository')
    const { IndexedDbTaskCheckpointStore } = await import(checkpointPath) as typeof import('../../src/output/origin-private/task-checkpoint/indexeddb-store')
    const { initialReceiveLifecycleState, nextReceiveLifecycleState } = await import(statePath) as typeof import('../../src/transfer/lifecycle/state')
    const { receiveOperationLeaseRecord } = await import(recordsPath) as typeof import('../../src/output/workspace/records')
    const { decodeStoredReceiveLifecycleState } = await import(codecPath) as typeof import('../../src/output/workspace/state-codec')
    const identity = (size: number, value: number) => btoa(String.fromCharCode(...new Uint8Array(size).fill(value)))
      .replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
    const operationId = identity(16, 1)
    const leaseId = identity(16, 2)
    const databaseName = `archive-settlement-${crypto.randomUUID()}`
    const object = { operationId, objectId: identity(32, 3), kind: 'zip-archive' as const, handleId: 'archive-handle' }
    const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
    const store = await IndexedDbTaskCheckpointStore.open(object, databaseName, leaseId)
    try {
      const initial = initialReceiveLifecycleState({ operationId, receiveIntentDigest: identity(32, 4) })
      await repository.commitTransition({ operationId, lifecycle: initial,
        lease: { kind: 'put', record: receiveOperationLeaseRecord({ operationId, leaseId, acquiredAt: 1 }) } })
      const receiving = nextReceiveLifecycleState(initial, { kind: 'receiving', activeLeaseId: leaseId })
      await repository.commitTransition({ operationId, expectedLifecycleGeneration: initial.generation,
        expectedLeaseId: leaseId, lifecycle: receiving })
      const first = { object, generation: 1n, allocatedLength: 0n, physicalLength: 0n, entryCount: 0n,
        discoveryComplete: false, selectedPaths: [[]], artifactState: 'receiving' as const }
      await store.commit({ expectedGeneration: undefined, checkpoint: first, entries: [] })
      const current = { ...first, generation: 2n, discoveryComplete: true }
      await store.commit({ expectedGeneration: first.generation, checkpoint: current, entries: [] })
      let notifications = 0
      repository.subscribeLifecycle(() => { notifications++ })
      const paused = nextReceiveLifecycleState(receiving, {
        kind: 'resumable-receive', payloadKind: 'workspace-archive', objectId: object.objectId,
        checkpointGeneration: current.generation, occupiedBytes: 0n, completedFileCount: 0n,
        completedBytes: 0n, discoveryComplete: true,
      })
      const rejected: boolean[] = []
      for (const stale of [first, { ...current, discoveryComplete: false }]) {
        try {
          await repository.commitTransition({ operationId, expectedLifecycleGeneration: receiving.generation,
            expectedLeaseId: leaseId, workspaceCheckpoint: stale, lifecycle: paused,
            lease: { kind: 'delete', leaseId } })
          rejected.push(false)
        } catch { rejected.push(true) }
      }
      const before = decodeStoredReceiveLifecycleState((await repository.readLifecycle(operationId))!)
      const retainedLease = await repository.readLease(operationId)
      const rejectedNotifications = notifications
      await repository.commitTransition({ operationId, expectedLifecycleGeneration: receiving.generation,
        expectedLeaseId: leaseId, workspaceCheckpoint: current, lifecycle: paused })
      const after = decodeStoredReceiveLifecycleState((await repository.readLifecycle(operationId))!)
      return { rejected, before: before.kind, leaseRetained: retainedLease?.leaseId === leaseId,
        rejectedNotifications, committedNotifications: notifications, after: after.kind }
    } finally {
      store.close()
      repository.close()
      indexedDB.deleteDatabase(databaseName)
    }
  })
  expect(result).toEqual({ rejected: [true, true], before: 'receiving', leaseRetained: true,
    rejectedNotifications: 0, committedNotifications: 1, after: 'resumable-receive' })
})
