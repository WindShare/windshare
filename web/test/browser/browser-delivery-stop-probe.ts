import { openBrowserFolderDelivery } from '../../src/output/browser-delivery/assembly'
import { IndexedDbBrowserDeliveryRepository } from '../../src/output/browser-delivery/indexeddb'
import { createBrowserDeliveryRecord, createBrowserSavePolicy } from '../../src/output/browser-delivery'
import { FILE_CHECKPOINT_MATERIALIZER_FSA_TREE, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE } from '../../src/output/persistence/checkpoint'
import { readBrowserDeliveryResumeSummary } from '../../src/output/browser-delivery/retained-authority'
import { IndexedDbFileCheckpointRepository, IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { DEFAULT_OUTPUT_DATABASE_NAME } from '../../src/output/browser/indexeddb-database'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import { reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { ORIGIN_PRIVATE_RAW_FILE_CONTAINER } from '../../src/output/origin-private/workspace-root'
import { UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES } from '../../src/output/planning/file-receiving-placement'
import { IndexedDbStagingBudgetStore } from '../../src/output/staging-budget/indexeddb-store'
import { initialReceiveLifecycleState } from '../../src/output/workspace/state'
import { reduceReceiveLifecycle } from '../../src/output/workspace/lifecycle'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import type { ReceiveIntent } from '../../src/transfer/intent'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import { outputExecutionProfile, outputSessionIdentity, TransferStopRequestedError } from '../../src/transfer/output-session'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import { runRetainedBrowserFolderAction } from '../../src/ui/browser-receive/fsa/local-delivery'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import { retainedBrowserDeliveryActions } from '../../src/ui/browser-receive/fsa/retained-delivery'
import { bindTask, resultRootArtifact, type FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'
import { deliveryIdentity } from '../output/browser-delivery-fixture'
import { createTestCheckpointAuthorities } from '../output/persistent-tree-file-fixture'

const FILE_ID = deliveryIdentity(90, 16)
const SOURCE_REVISION = deliveryIdentity(91, 16)
const FILE_NAME = 'incomplete.bin'
const PREFIX = Uint8Array.of(1, 3, 5, 7, 9, 11, 13, 15)
const TRANSFER_JOB_ID = deliveryIdentity(92, 16)
const SIGNAL = new AbortController().signal
const EMPTY_MATERIALIZATION = { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n }
const storageFacts = async () => ({
  opfs: 'usable' as const, pressure: 'normal' as const,
  persistence: 'not-persisted' as const, quota: { kind: 'unknown' as const },
})

export interface StopStorageFixture extends FsaNamespaceFixture {
  readonly intent: ReceiveIntent
  readonly stageRootName: string
  readonly stageObjectId: string
  readonly targetName: string
}
export type StopStorageCut = 'stop' | 'pause' | 'cleanup-failure' | 'complete-stage'

function request(exactSize = UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES) {
  return {
    sourceAuthenticationPath: ['photos', FILE_NAME], materializationRelativePath: [FILE_NAME],
    recovery: { pausedFile: 'preserve' as const },
    openRevision: async () => ({
      fileId: FILE_ID, fileRevision: SOURCE_REVISION, exactSize,
    }),
  }
}

function deliveryOptions(fixture: FsaNamespaceFixture, intent: ReceiveIntent, leaseId: string) {
  return {
    intent, preference: 'automatic' as const, storage: navigator.storage, storageFacts,
    databaseName: fixture.databaseName,
    operationLease: { operationId: intent.operationId, leaseId },
  }
}

export async function prepareStopStorage(parentName: string, cut: StopStorageCut) {
  const fixture = { databaseName: DEFAULT_OUTPUT_DATABASE_NAME, parentName }
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName, { create: true })
  const operations = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const target = await bindTask(fixture, parent, operations, await resultRootArtifact(), 70)
  const lease = await acquireBrowserReceiveOperationLease(operations, target.intent.operationId)
  if (cut === 'complete-stage') await pinTinyRetainedStage(target.intent)
  const delivery = await openBrowserFolderDelivery({ target, ...deliveryOptions(fixture, target.intent, lease.leaseId) })
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName: fixture.databaseName })
  const originalRemove = FileSystemDirectoryHandle.prototype.removeEntry
  let injectedFailures = 0
  try {
    await startReceiving(operations, target.intent, lease.leaseId)
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: target.intent, repository: operations, lifecycleLeaseId: lease.leaseId, transferJobId: TRANSFER_JOB_ID,
    })
    const execution = await createPersistentDirectTreeExecution({
      intent: target.intent as Parameters<typeof createPersistentDirectTreeExecution>[0]['intent'],
      materialization: delivery, settlement: settlement.bindMaterialization(target),
      outputIdentity: outputSessionIdentity({ backend: 'browser-stop-regression', outputSessionId: 'stop-output' }),
      executionProfile: outputExecutionProfile({ maximumConcurrentFilePipelines: 1,
        maximumOutstandingWriteBytes: 1024n, maximumBufferedBytes: 1024n }),
      ...createTestCheckpointAuthorities(),
    })
    // Authenticate a large exact size but write only a tiny prefix: the reservation,
    // not a giant fixture or timer, reproduces the stranded-capacity defect.
    const transaction = await delivery.beginFile(request(cut === 'complete-stage' ? BigInt(PREFIX.length) : undefined))
    await transaction.writeRange(0n, PREFIX)
    await transaction.checkpoint()
    const record = await journal.readFile(target.intent.operationId, FILE_ID)
    const policy = await journal.readPolicy(target.intent.operationId)
    if (record?.state.kind !== 'receiving' || record.state.checkpoint === undefined || policy?.staging === undefined) {
      throw new Error('Expected incomplete authenticated staging before terminal settlement')
    }
    const checkpoints = await IndexedDbFileCheckpointRepository.open(policy.staging, fixture.databaseName)
    let stageRootName: string
    try {
      const root = (await checkpoints.listHandles()).find(handle => handle.ownedObjectId === policy.staging!.authorityRef)
      if (root === undefined) throw new Error('Missing persisted staging root authority')
      stageRootName = (root.handle as FileSystemDirectoryHandle).name
    } finally { checkpoints.close() }
    const savedFixture = { ...fixture, intent: target.intent, stageRootName,
      stageObjectId: record.state.checkpoint.ownedObjectId, targetName: target.reservation.physicalName }
    const before = await inspectStorage(savedFixture)
    if (cut === 'cleanup-failure') {
      FileSystemDirectoryHandle.prototype.removeEntry = function(name, options) {
        if (name === savedFixture.stageObjectId) {
          injectedFailures++
          return Promise.reject(new DOMException('Injected owned staging deletion failure', 'UnknownError'))
        }
        return originalRemove.call(this, name, options)
      }
    }
    execution.beginTerminal(cut === 'pause' ? 'pause' : 'stop')
    const worker = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)
    const state = cut === 'pause'
      ? await execution.pause({ worker, materialization: EMPTY_MATERIALIZATION,
          reason: new Error('Pause regression'), selectionFacts: {
            discoveredFileCount: 1n, discoveredBytes: UNKNOWN_SPEED_STAGING_THRESHOLD_BYTES, discovery: 'failed',
          } }, SIGNAL)
      : await execution.stop!({ transferJobId: TRANSFER_JOB_ID, worker,
          materialization: EMPTY_MATERIALIZATION, reason: new TransferStopRequestedError() }, SIGNAL)
    FileSystemDirectoryHandle.prototype.removeEntry = originalRemove
    return { fixture: savedFixture, before, after: await inspectStorage(savedFixture),
      lifecycle: state.kind, injectedFailures }
  } finally {
    FileSystemDirectoryHandle.prototype.removeEntry = originalRemove
    await delivery.close().catch(() => undefined)
    await lease.release()
    journal.close()
    operations.close()
  }
}

export async function reopenStopStorage(fixture: StopStorageFixture, cut: StopStorageCut) {
  const before = await inspectStorage(fixture)
  if (cut !== 'pause') {
    if (cut === 'cleanup-failure' || cut === 'complete-stage') {
      await runRetainedBrowserFolderAction(window as unknown as BrowserReceiveWindow, {
        operationId: fixture.intent.operationId, receiveIntentDigest: fixture.intent.digest,
        lifecycleGeneration: BigInt(before.lifecycleGeneration!),
      }, cut === 'complete-stage' ? 'save-staged-files' : 'cleanup-staging', SIGNAL)
    }
    return { before, after: await inspectStorage(fixture), resumedRanges: [] }
  }
  const operations = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const lease = await acquireBrowserReceiveOperationLease(operations, fixture.intent.operationId)
  let resumedRanges: string[]
  try {
    const target = await reopenFileSystemAccessOutput({
      intent: fixture.intent, operationRepository: operations, databaseName: fixture.databaseName,
    })
    const delivery = await openBrowserFolderDelivery({ target, ...deliveryOptions(fixture, fixture.intent, lease.leaseId) })
    try {
      const transaction = await delivery.beginFile(request())
      resumedRanges = transaction.initialDurableRanges.map(range => range.start + ':' + range.end)
    } finally { await delivery.close() }
    return { before, after: await inspectStorage(fixture), resumedRanges }
  } finally { await lease.release(); operations.close() }
}

export async function replaceStoppedTargetAndSave(fixture: StopStorageFixture) {
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName)
  const target = await parent.getDirectoryHandle(fixture.targetName)
  await target.removeEntry(FILE_NAME)
  const writable = await (await target.getFileHandle(FILE_NAME, { create: true })).createWritable()
  await writable.write(Uint8Array.of(20, 40, 60))
  await writable.close()
  const failures: string[] = []
  // A refused attempt must not grant ownership to the same replacement on retry.
  for (let attempt = 0; attempt < 2; attempt++) {
    try { await reopenStopStorage(fixture, 'complete-stage') }
    catch (error) { failures.push(error instanceof Error ? error.name + ': ' + error.message : String(error)) }
  }
  return { failures, after: await inspectStorage(fixture) }
}

export async function inspectStorage(fixture: StopStorageFixture) {
  const journal = await IndexedDbBrowserDeliveryRepository.open({ databaseName: fixture.databaseName })
  const capacity = await IndexedDbStagingBudgetStore.open()
  const operations = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  try {
    const record = await journal.readFile(fixture.intent.operationId, FILE_ID)
    const summary = await readBrowserDeliveryResumeSummary({
      repository: journal, operationId: fixture.intent.operationId, databaseName: fixture.databaseName,
    })
    const reservations = await capacity.transact(inventory => ({
      result: inventory.records.filter(row => row.operationId === fixture.intent.operationId)
        .map(row => ({ exactSize: row.exactSize.toString(), verifiedBytes: row.verifiedStagedBytes.toString() })),
    }))
    const stored = await operations.readLifecycle(fixture.intent.operationId)
    const lifecycle = stored === undefined ? undefined : decodeStoredReceiveLifecycleState(stored)
    return {
      deliveryState: record?.state.kind,
      stageBytes: await stageBytes(fixture),
      reservations,
      reservedBytes: summary?.reservedStagingBytes.toString(),
      retainedBytes: summary?.stagedBytes.toString(),
      continuation: summary?.localContinuation,
      actions: lifecycle === undefined ? [] : retainedBrowserDeliveryActions(lifecycle, summary, []),
      lifecycle: lifecycle?.kind,
      lifecycleGeneration: lifecycle?.generation.toString(),
      targetBytes: await targetBytes(fixture),
    }
  } finally { journal.close(); capacity.close(); operations.close() }
}

async function stageBytes(fixture: StopStorageFixture): Promise<number[] | null> {
  try {
    const root = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.stageRootName)
    const container = await root.getDirectoryHandle(ORIGIN_PRIVATE_RAW_FILE_CONTAINER)
    const file = await (await container.getFileHandle(fixture.stageObjectId)).getFile()
    return [...new Uint8Array(await file.arrayBuffer())]
  } catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return null
    throw error
  }
}

async function targetBytes(fixture: StopStorageFixture): Promise<number[] | null> {
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName)
  const target = await parent.getDirectoryHandle(fixture.targetName)
  try {
    const file = await (await target.getFileHandle(FILE_NAME)).getFile()
    return [...new Uint8Array(await file.arrayBuffer())]
  } catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return null
    throw error
  }
}

async function pinTinyRetainedStage(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'direct-tree') throw new Error('Expected folder plan')
  const policy = createBrowserSavePolicy({
    operationId: intent.operationId, receiveIntentDigest: intent.digest, preference: 'automatic',
    target: { operationId: intent.operationId, receiveIntentDigest: intent.digest,
      materializationBindingDigest: intent.plan.reservation.digest,
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE, authorityRef: intent.plan.reservation.authorityRef },
    staging: { operationId: deliveryIdentity(93, 16), receiveIntentDigest: intent.digest,
      materializationBindingDigest: deliveryIdentity(94), materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
      authorityRef: deliveryIdentity(95) },
  })
  const journal = await IndexedDbBrowserDeliveryRepository.open()
  try {
    await journal.installPolicy(policy)
    // Placement is already pinned in retained records. Keeping this fixture tiny
    // exercises completed-stage storage and terminal save without a full large-file copy.
    await journal.createFile(createBrowserDeliveryRecord({
      policy, source: { ...await request(BigInt(PREFIX.length)).openRevision(), canonicalPath: ['photos', FILE_NAME] },
      materializationRelativePath: [FILE_NAME], placement: 'staged', placementReason: 'retained-complete-stage-fixture',
    }))
  } finally { journal.close() }
}

async function startReceiving(repository: IndexedDbReceiveOperationRepository, intent: ReceiveIntent, leaseId: string) {
  const initial = initialReceiveLifecycleState({ operationId: intent.operationId, receiveIntentDigest: intent.digest })
  await repository.commitTransition({ operationId: intent.operationId, expectedLeaseId: leaseId, lifecycle: initial })
  const receiving = reduceReceiveLifecycle(initial, {
    kind: 'receive-started', expectedGeneration: initial.generation, leaseId,
  }, { planKind: 'direct-tree', preparationRequired: false, activeLeaseId: leaseId }).state
  await repository.commitTransition({ operationId: intent.operationId, expectedLifecycleGeneration: initial.generation,
    expectedLeaseId: leaseId, lifecycle: receiving })
}
