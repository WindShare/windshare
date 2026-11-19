import { deliveryIdentity } from '../output/browser-delivery-fixture'
import {
  IndexedDbFileCheckpointRepository,
  IndexedDbReceiveOperationRepository,
} from '../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { openBrowserFolderDelivery } from '../../src/output/browser-delivery/assembly'
import { IndexedDbBrowserDeliveryRepository } from '../../src/output/browser-delivery/indexeddb'
import type { BrowserDeliveryMaterialization } from '../../src/output/browser-delivery/session'
import { reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { originPrivateRawFileHandleId } from '../../src/output/origin-private/workspace-tree'
import { UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES } from '../../src/output/planning/file-receiving-placement'
import { IndexedDbStagingBudgetStore } from '../../src/output/staging-budget/indexeddb-store'
import type { ReceiveIntent } from '../../src/transfer/intent'
import { bindTask, resultRootArtifact, type FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'

const FILE_ID = deliveryIdentity(44, 16)
const INITIAL_REVISION = deliveryIdentity(45, 16)
const REOPENED_REVISION = deliveryIdentity(46, 16)
const SOURCE_METADATA = deliveryIdentity(47)
const FILE_NAME = 'receive.bin'
// The real automatic placement decision selects OPFS without transferring a large fixture.
const FILE_SIZE = UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES

export function prepareBrowserDeliveryResume(fixture: FsaNamespaceFixture) {
  return withFolderDelivery(fixture, undefined, async (delivery, intent) => {
    const first = await delivery.beginFile(fileRequest(intent, INITIAL_REVISION))
    const objectId = first.checkpointObjectId!
    await first.writeRange(0n, Uint8Array.of(1, 2, 3))
    await first.pause()

    const reopened = await delivery.beginFile(fileRequest(intent, REOPENED_REVISION))
    const readmitted = reopened.initialDurableRanges
    await reopened.writeRange(3n, Uint8Array.of(4, 5, 6))
    await reopened.pause()
    return { intent, objectId, readmitted, reopenedObjectId: reopened.checkpointObjectId }
  })
}

export function resumeAndDiscardBrowserDelivery(input: Readonly<{
  fixture: FsaNamespaceFixture
  intent: ReceiveIntent
  objectId: string
}>) {
  const { fixture, intent, objectId } = input
  return withFolderDelivery(fixture, intent, async delivery => {
    const resumed = await delivery.beginFile(fileRequest(intent, REOPENED_REVISION))
    const readmitted = resumed.initialDurableRanges
    await resumed.writeRange(6n, Uint8Array.of(7))
    await resumed.pause()

    const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName: fixture.databaseName })
    try {
      const policy = await journal.readPolicy(intent.operationId)
      if (policy?.staging === undefined) throw new TypeError('Fixture did not select browser staging')
      const checkpoints = await IndexedDbFileCheckpointRepository.open(policy.staging, fixture.databaseName)
      const capacity = await IndexedDbStagingBudgetStore.open(capacityDatabaseName(fixture))
      try {
        const handleId = originPrivateRawFileHandleId(policy.staging.operationId, objectId)
        const entry = await checkpoints.readHandle(handleId)
        const file = entry?.handle as FileSystemFileHandle | undefined
        if (file === undefined) throw new TypeError('Fixture lost the original staging handle')
        const bytes = Array.from(new Uint8Array(await (await file.getFile()).arrayBuffer()))
        const reserved = await capacity.transact(({ records }) => ({
          result: records.find(record => record.operationId === intent.operationId && record.fileId === FILE_ID),
        }))

        await delivery.discardIncompleteStaging()
        const fileRemoved = await file.getFile().then(() => false, (error: unknown) => {
          if (error instanceof DOMException && error.name === 'NotFoundError') return true
          throw error
        })
        const capacityReleased = await capacity.transact(({ records }) => ({
          result: records.every(record => record.operationId !== intent.operationId),
        }))
        return {
          readmitted,
          resumedObjectId: resumed.checkpointObjectId,
          bytes,
          reservedObjectId: reserved?.objectId,
          reservedSize: reserved?.exactSize,
          fileRemoved,
          handleRemoved: await checkpoints.readHandle(handleId) === undefined,
          capacityReleased,
          state: (await journal.readFile(intent.operationId, FILE_ID))?.state.kind,
        }
      } finally {
        capacity.close()
        checkpoints.close()
      }
    } finally {
      journal.close()
    }
  })
}

function fileRequest(intent: ReceiveIntent, fileRevision: string) {
  return {
    sourceAuthenticationPath: ['photos', FILE_NAME],
    materializationRelativePath: [FILE_NAME],
    shareInstance: intent.shareInstance,
    recovery: { pausedFile: 'preserve' as const },
    openRevision: async () => ({
      fileId: FILE_ID, fileRevision, exactSize: FILE_SIZE, sourceMetadata: SOURCE_METADATA,
    }),
  }
}

async function withFolderDelivery<T>(
  fixture: FsaNamespaceFixture,
  intent: ReceiveIntent | undefined,
  run: (delivery: BrowserDeliveryMaterialization, intent: ReceiveIntent) => Promise<T>,
): Promise<T> {
  const { databaseName, parentName } = fixture
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const target = intent === undefined
      ? await bindTask(fixture,
          await (await navigator.storage.getDirectory()).getDirectoryHandle(parentName, { create: true }),
          repository, await resultRootArtifact(), 20)
      : await reopenFileSystemAccessOutput({ intent, operationRepository: repository, databaseName })
    try {
      const lease = await acquireBrowserReceiveOperationLease(repository, target.intent.operationId)
      try {
        const delivery = await openBrowserFolderDelivery({
          target, intent: target.intent, storage: navigator.storage, preference: 'automatic',
          storageFacts: async () => ({ opfs: 'usable', persistence: 'not-persisted', pressure: 'normal',
            quota: { kind: 'estimated', quotaBytes: 10_000_000_000n, usageBytes: 0n } }),
          operationLease: { operationId: target.intent.operationId, leaseId: lease.leaseId },
          databaseName, capacityDatabaseName: capacityDatabaseName(fixture),
        })
        try { return await run(delivery, target.intent) }
        finally { await delivery.close() }
      } finally { await lease.release() }
    } finally { await target.close() }
  } finally { repository.close() }
}

function capacityDatabaseName(fixture: FsaNamespaceFixture): string {
  return fixture.databaseName + '-capacity'
}
