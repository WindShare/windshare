import { describe, expect, it } from 'vitest'

import { directoryId } from '../../src/catalog/model'
import {
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  type DirectoryTreeArtifact,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import type { DirectoryAdmission } from '../../src/transfer/directory-admission'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import type { BrowserLockManagerRuntime } from '../../src/output/browser/namespace-mutation'
import { verifyFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import {
  bindNewFileSystemAccessOutput,
  reopenFileSystemAccessOutput,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSAOutputTraceEvent,
  type FileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import {
  discardReopenedFileSystemAccessOutput,
  type ReopenedFileSystemAccessDiscardOperation,
} from '../../src/output/file-system-access/fresh-page-discard'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  fileCheckpointIsComplete,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  checkpointMatchesNamespace,
  finalFileCheckpointProof,
  type CheckpointNamespaceBinding,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FinalFileCheckpointProof,
  type PersistentHandleRecord,
} from '../../src/output/persistence/journal'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import {
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
  receiveOperationLeaseRecord,
  type PersistedReceiveRecord,
  type ReceiveOperationHandleRecord,
  type ReceiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import { createExpiryReceipt, persistedReceiptRecord } from '../../src/output/workspace/receipts'
import {
  prepareReceiveOperationTransition,
  type ReceiveOperationRepository,
  type ReceiveOperationTransition,
} from '../../src/output/workspace/repository'
import { reduceReceiveLifecycle } from '../../src/output/workspace/lifecycle'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { initialReceiveLifecycleState, type ReceiveLifecycleState } from '../../src/output/workspace/state'
import { outputSessionIdentity } from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import { identity } from './planning/fixture'

const SIGNAL = new AbortController().signal
const SUCCESS = transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const PAUSED = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)

describe('File System Access DirectTree lifecycle', () => {
  it('persists a collision suffix, creates one task root, and reopens it after restart', async () => {
    const parent = new MemoryDirectory('downloads')
    const unrelated = await parent.getDirectoryHandle('photos', { create: true })
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const artifact = await resultRootArtifact()
    const first = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 10,
    })

    expect(first.reservation.collisionIndex).toBe(1)
    expect(first.reservation.reservedName).not.toBe('photos')
    const taskRoot = await parent.getDirectoryHandle(first.reservation.reservedName)
    expect(await parent.getDirectoryHandle('photos')).toBe(unrelated)
    expect(parent.directoryNames()).toEqual(['photos', first.reservation.reservedName].sort())
    await first.ensureDirectory(['nested'])
    const firstFile = await first.beginFile({
      artifactPath: ['nested', 'photo.bin'],
      openRevision: async () => ({
        fileId: identity(80),
        fileRevision: identity(81),
        exactSize: 2n,
      }),
    })
    await firstFile.writeRange(0n, Uint8Array.of(7, 8))
    await firstFile.commit()
    const nested = await taskRoot.getDirectoryHandle('nested')
    const visibleFile = await (await nested.getFileHandle('photo.bin')).getFile()
    expect(new Uint8Array(await visibleFile.arrayBuffer())).toEqual(Uint8Array.of(7, 8))
    const frozenIntent = first.intent
    await first.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent: frozenIntent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
    })
    expect(reopened.reservation.reservedName).toBe(first.reservation.reservedName)
    expect(await parent.getDirectoryHandle(reopened.reservation.reservedName)).toBe(taskRoot)
    await expect(reopened.ensureDirectory(['nested'])).resolves.toMatchObject({ created: false })
    const reopenedFile = await reopened.beginFile({
      artifactPath: ['nested', 'photo.bin'],
      openRevision: async () => ({
        fileId: identity(80),
        fileRevision: identity(81),
        exactSize: 2n,
      }),
    })
    expect(reopenedFile.ownedObjectId).toBe(firstFile.ownedObjectId)
    expect(parent.directoryNames()).toHaveLength(2)
    await reopened.close()

    const second = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 20,
    })
    expect(second.reservation.reservedName).not.toBe(first.reservation.reservedName)
    expect(parent.directoryNames()).toHaveLength(3)
    await second.close()
  })

  it('uses single-file DirectoryTree directly below the parent and creates after revision open', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const artifact = await singleFileArtifact()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 30,
    })

    expect(parent.entryNames()).toEqual([])
    const ordering: string[] = []
    parent.onFileCreated = () => ordering.push('created')
    const transaction = await session.beginFile({
      artifactPath: [session.reservation.reservedName],
      openRevision: async () => {
        ordering.push('revision-opened')
        return {
          fileId: identity(3),
          fileRevision: identity(33),
          exactSize: 4n,
        }
      },
    })
    expect(ordering).toEqual(['revision-opened', 'created'])
    expect(parent.directoryNames()).toEqual([])
    expect(parent.fileNames()).toEqual([session.reservation.reservedName])

    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    expect(await parent.fileBytes(session.reservation.reservedName)).toEqual(Uint8Array.of(1, 2))
    await transaction.checkpoint()
    await transaction.writeRange(2n, Uint8Array.of(3, 4))
    await transaction.commit()
    const intent = session.intent
    await session.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
    })
    const resumed = await reopened.beginFile({
      artifactPath: [reopened.reservation.reservedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(33),
        exactSize: 4n,
      }),
    })
    expect(resumed.ownedObjectId).toBe(transaction.ownedObjectId)
    expect(parent.entryNames()).toEqual([session.reservation.reservedName])
    await reopened.close()
  })

  it('turns an external task-root race into NeedsAttention without merging', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const artifact = await resultRootArtifact()
    const trace: FSAOutputTraceEvent[] = []
    let external: FileSystemDirectoryHandle | undefined
    repository.afterFirstCommit = async () => {
      external = await parent.getDirectoryHandle('photos', { create: true })
    }

    await expect(bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact,
      operationSeed: 40,
      trace: (event) => trace.push(event),
    })).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
    expect(await parent.getDirectoryHandle('photos')).toBe(external)
    expect(parent.directoryNames()).toEqual(['photos'])
    expect(trace.at(-1)).toMatchObject({
      name: 'receive.operation.needs_attention',
      needs_attention_reason: 'target-ownership-unknown',
    })
  })

  it('rechecks the file identity before writer acquisition and preserves the replacement', async () => {
    const parent = new MemoryDirectory('downloads')
    const trace: FSAOutputTraceEvent[] = []
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 50,
      trace: (event) => trace.push(event),
    })
    const transaction = await session.beginFile({
      artifactPath: [session.reservation.reservedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(34),
        exactSize: 1n,
      }),
    })
    const replacement = parent.replaceFile(session.reservation.reservedName, Uint8Array.of(9))
    await expect(transaction.writeRange(0n, Uint8Array.of(1))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(await replacement.bytes()).toEqual(Uint8Array.of(9))
    expect(trace.at(-1)).toMatchObject({
      name: 'receive.operation.needs_attention',
      needs_attention_reason: 'target-ownership-unknown',
    })
    await session.close().catch(() => undefined)
  })
})

describe('File System Access settlement authority', () => {
  it('publishes only after final directory and checkpoint ownership observation', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    let retired = 0
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(() => { retired += 1 }),
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 60,
    })
    const leaseId = identity(61)
    const transferJobId = identity(62)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(63))
    const root = await execution.directories.admitDirectory({
      source: {
        directoryId: directoryId(session.intent.syntheticRoot),
        generation: identity(64),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(65),
      fileRevision: identity(66),
      artifactPath: ['payload.bin'],
      exactSize: 2n,
      parentAdmission: root,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(4, 5), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    await execution.directories.finalizeDirectory(root, SIGNAL)

    await expect(execution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'published' })
    expect(retired).toBe(1)
    expect(locks.releaseCount).toBe(1)
    expect(repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(1)
  })

  it('persists PartialDirectory instead of publishing worker failures', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 70,
    })
    const leaseId = identity(71)
    const transferJobId = identity(72)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(73))
    const root = await execution.directories.admitDirectory({
      source: {
        directoryId: session.intent.syntheticRoot,
        generation: identity(74),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(75),
      fileRevision: identity(76),
      artifactPath: ['kept.bin'],
      exactSize: 1n,
      parentAdmission: root,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(8), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    const worker = transferWorkerSettlement('CompletedWithErrors', {
      failures: Object.freeze([{
        kind: 'directory' as const,
        directoryId: directoryId(session.intent.syntheticRoot),
        reason: new Error('directory metadata failed'),
      }]),
      failureCount: 1,
      omittedFailureCount: 0,
    })

    await expect(execution.settle({
      transferJobId,
      worker,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 1n },
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'partial-directory',
      successCount: 1n,
      failureCount: 1n,
    })
  })

  it('reduces a replacement at final observation to NeedsAttention without retiring proof', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    let retired = 0
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(() => { retired += 1 }),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 80,
    })
    const leaseId = identity(81)
    const transferJobId = identity(82)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(83))
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(3),
      fileRevision: identity(84),
      artifactPath: [session.reservation.reservedName],
      exactSize: 1n,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(1), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    parent.replaceFile(session.reservation.reservedName, Uint8Array.of(9))

    await expect(execution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 1n },
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
    })
    expect(retired).toBe(0)
  })

  it('preserves paused ranges, releases once, and publishes a reopened single file', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    let retired = 0
    const checkpointFactory = memoryCheckpointFactory(() => { retired += 1 })
    const first = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await singleFileArtifact(),
      operationSeed: 90,
    })
    const leaseId = identity(91)
    const transferJobId = identity(92)
    await startReceiving(repository, first.intent, leaseId)
    const firstExecution = await fsaExecution(first, repository, leaseId, transferJobId, identity(93))
    const opened = await firstExecution.output.beginFile(outputFileRequest({
      intent: first.intent,
      fileId: identity(3),
      fileRevision: identity(94),
      artifactPath: [first.reservation.reservedName],
      exactSize: 4n,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(1, 2), SIGNAL)
    await opened.transaction.pause('cancelled')

    await expect(firstExecution.pause({
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new DOMException('cancelled', 'AbortError'),
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'resumable-receive',
      completedFileCount: 0n,
      completedBytes: 0n,
    })
    expect(locks.releaseCount).toBe(1)
    expect(retired).toBe(0)

    await resumeReceiving(repository, first.intent, leaseId)
    const reopened = await reopenFileSystemAccessOutput({
      intent: first.intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
    })
    const reopenedExecution = await fsaExecution(
      reopened,
      repository,
      leaseId,
      transferJobId,
      identity(95),
    )
    const resumed = await reopenedExecution.output.beginFile(outputFileRequest({
      intent: reopened.intent,
      fileId: identity(3),
      fileRevision: identity(94),
      artifactPath: [reopened.reservation.reservedName],
      exactSize: 4n,
    }), SIGNAL)
    expect(resumed.durableRanges.ranges).toEqual([{ start: 0n, end: 2n }])
    await resumed.transaction.writeRange(2n, Uint8Array.of(3, 4), SIGNAL)
    await resumed.transaction.commit(SIGNAL)
    await expect(reopenedExecution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 4n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'published' })
    expect(locks.releaseCount).toBe(2)
    expect(retired).toBe(1)
  })

  it('records an unopened single-file abort without creating a namespace entry', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 100,
    })
    const leaseId = identity(101)
    const transferJobId = identity(102)
    await installIntentFrozen(repository, session.intent, leaseId)
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: session.intent,
      repository,
      lifecycleLeaseId: leaseId,
      transferJobId,
      clock: () => 1_000,
    })

    await expect(settlement.abortUnopened(session.intent, 'cancelled', SIGNAL))
      .resolves.toMatchObject({ kind: 'discarded' })
    expect(parent.entryNames()).toEqual([])
    await session.close()
  })
})

describe('File System Access fresh-page discard authority', () => {
  it('removes one proven unfinished object and persists Discarded from a fresh repository', async () => {
    const fixture = await freshDiscardFixture({ seed: 110, successfulFile: false })

    await expect(discardFreshFixture(fixture)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
      receiptDigest: expect.any(String),
    })
    expect(fixture.parent.entryNames()).toEqual([])
    expect(fixture.retired()).toBe(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(fixture.repository.leases.has(fixture.intent.operationId)).toBe(false)
    expect(fixture.closed()).toBe(1)
  })

  it('preserves complete checkpoint files and persists PartialDirectory(stopped)', async () => {
    const fixture = await freshDiscardFixture({ seed: 120, successfulFile: true })

    const result = await discardFreshFixture(fixture)
    expect(result).toMatchObject({
      lifecycle: {
        kind: 'partial-directory',
        reason: 'stopped',
        successCount: 1n,
        failureCount: 0n,
      },
      receiptDigest: expect.any(String),
    })
    const root = await fixture.parent.getDirectoryHandle(
      fixture.reservationName,
    ) as unknown as MemoryDirectory
    expect(root.fileNames()).toEqual(['kept.bin'])
    expect(await root.fileBytes('kept.bin')).toEqual(Uint8Array.of(7, 8))
    expect(fixture.retired()).toBe(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(2)
  })

  it('persists target ownership attention for changed parent or file identity', async () => {
    const changedFile = await freshDiscardFixture({ seed: 130, successfulFile: false })
    const replacement = changedFile.parent.replaceFile(
      changedFile.reservationName,
      Uint8Array.of(9),
    )
    await expect(discardFreshFixture(changedFile)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    expect(await replacement.bytes()).toEqual(Uint8Array.of(9))
    expect(changedFile.retired()).toBe(0)

    const changedParent = await freshDiscardFixture({ seed: 140, successfulFile: false })
    const parentRecord = changedParent.repository.handles.get(
      changedParent.operation.binding.parentHandleId,
    )
    if (parentRecord === undefined) throw new Error('fresh discard parent fixture is missing')
    changedParent.repository.handles.set(parentRecord.id, Object.freeze({
      ...parentRecord,
      handle: new MemoryDirectory('replacement-downloads'),
    }))
    await expect(discardFreshFixture(changedParent)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    expect(changedParent.parent.fileNames()).toEqual([changedParent.reservationName])
    expect(changedParent.retired()).toBe(0)
  })

  it('persists cleanup attention when deletion has no conclusive result', async () => {
    const fixture = await freshDiscardFixture({ seed: 150, successfulFile: false })
    fixture.parent.onRemoveEntry = async (name) => {
      if (name === fixture.reservationName) throw new DOMException('delete unknown', 'OperationError')
    }

    await expect(discardFreshFixture(fixture)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'cleanup-unknown' },
    })
    expect(fixture.parent.fileNames()).toEqual([fixture.reservationName])
    expect(fixture.retired()).toBe(0)
  })

  it('fences stale lifecycle, foreign lease, reused authority, and concurrent discard', async () => {
    const stale = await freshDiscardFixture({ seed: 160, successfulFile: false })
    const staleOperation = Object.freeze({
      ...stale.operation,
      lifecycle: Object.freeze({
        ...stale.operation.lifecycle,
        generation: stale.operation.lifecycle.generation - 1n,
      }),
    })
    await expect(discardFreshFixture(stale, staleOperation)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    expect(stale.parent.fileNames()).toEqual([stale.reservationName])

    const foreign = await freshDiscardFixture({ seed: 170, successfulFile: false })
    const foreignOperation = Object.freeze({
      ...foreign.operation,
      lease: Object.freeze({
        operationId: foreign.intent.operationId,
        leaseId: identity(199),
      }),
    })
    await expect(discardFreshFixture(foreign, foreignOperation)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    expect(foreign.parent.fileNames()).toEqual([foreign.reservationName])

    const concurrent = await freshDiscardFixture({ seed: 180, successfulFile: false })
    const first = discardFreshFixture(concurrent)
    await expect(discardFreshFixture(concurrent)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    await expect(first).resolves.toMatchObject({ lifecycle: { kind: 'discarded' } })
    await expect(discardFreshFixture(concurrent)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
  })

  it('cleans at the exact retention boundary only after Expired(cleanup-pending)', async () => {
    const fixture = await freshDiscardFixture({ seed: 190, successfulFile: true })
    const expired = await persistFreshFixtureExpiry(fixture)
    const operation = Object.freeze({ ...fixture.operation, lifecycle: expired })

    await expect(discardFreshFixture(fixture, operation, expired.expiresAt)).resolves.toMatchObject({
      lifecycle: {
        kind: 'expired',
        priorStableState: 'resumable-receive',
        cleanupState: 'clean',
      },
      receiptDigest: expect.any(String),
    })
    const root = await fixture.parent.getDirectoryHandle(
      fixture.reservationName,
    ) as unknown as MemoryDirectory
    expect(root.fileNames()).toEqual(['kept.bin'])
    expect(fixture.retired()).toBe(1)
  })
})

interface FreshDiscardFixture {
  readonly parent: MemoryDirectory
  readonly repository: MemoryOperationRepository
  readonly checkpointFactory: FSAFileCheckpointRepositoryFactory
  readonly locks: MemoryLockManager
  readonly intent: ReceiveIntent
  readonly reservationName: string
  readonly leaseId: string
  readonly operation: ReopenedFileSystemAccessDiscardOperation
  retired(): number
  closed(): number
}

async function freshDiscardFixture(input: Readonly<{
  seed: number
  successfulFile: boolean
}>): Promise<FreshDiscardFixture> {
  const parent = new MemoryDirectory(`downloads-${input.seed}`)
  const primaryRepository = new MemoryOperationRepository()
  const locks = new MemoryLockManager()
  let retireCount = 0
  const checkpointFactory = memoryCheckpointFactory(() => { retireCount += 1 })
  const session = await bindTask({
    parent,
    repository: primaryRepository,
    checkpointFactory,
    locks,
    artifact: input.successfulFile ? await resultRootArtifact() : await singleFileArtifact(),
    operationSeed: input.seed,
  })
  const initialLeaseId = identity(input.seed + 3)
  const transferJobId = identity(input.seed + 4)
  await startReceiving(primaryRepository, session.intent, initialLeaseId)
  const execution = await fsaExecution(
    session,
    primaryRepository,
    initialLeaseId,
    transferJobId,
    identity(input.seed + 5),
  )

  let parentAdmission: DirectoryAdmission | undefined
  if (input.successfulFile) {
    parentAdmission = await execution.directories.admitDirectory({
      source: {
        directoryId: directoryId(session.intent.syntheticRoot),
        generation: identity(input.seed + 6),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const complete = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(input.seed + 7),
      fileRevision: identity(input.seed + 8),
      artifactPath: ['kept.bin'],
      exactSize: 2n,
      parentAdmission,
    }), SIGNAL)
    await complete.transaction.writeRange(0n, Uint8Array.of(7, 8), SIGNAL)
    await complete.transaction.commit(SIGNAL)
  }

  const unfinished = await execution.output.beginFile(outputFileRequest({
    intent: session.intent,
    fileId: input.successfulFile ? identity(input.seed + 9) : identity(3),
    fileRevision: identity(input.seed + 10),
    artifactPath: input.successfulFile
      ? ['unfinished.bin']
      : [session.reservation.reservedName],
    exactSize: 2n,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
  }), SIGNAL)
  await unfinished.transaction.writeRange(0n, Uint8Array.of(1), SIGNAL)
  await unfinished.transaction.pause('fresh-page-discard')
  await execution.pause({
    worker: PAUSED,
    materialization: input.successfulFile
      ? { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n }
      : { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    reason: new DOMException('page closed', 'AbortError'),
  }, SIGNAL)

  // A distinct repository object proves that cleanup does not depend on the old page's runtime.
  const repository = new MemoryOperationRepository({
    records: primaryRepository.records,
    handles: primaryRepository.handles,
    leases: primaryRepository.leases,
  })
  const lifecycle = await lifecycleState(repository, session.intent.operationId)
  const leaseId = identity(input.seed + 11)
  await repository.commitTransition({
    operationId: session.intent.operationId,
    expectedLifecycleGeneration: lifecycle.generation,
    expectedLeaseId: initialLeaseId,
    lease: {
      kind: 'put',
      record: receiveOperationLeaseRecord({
        operationId: session.intent.operationId,
        leaseId,
        acquiredAt: 1_500,
      }),
    },
  })
  const binding = await verifyFSAOperationBinding({
    repository,
    intent: session.intent,
  })
  let closeCount = 0
  let closePromise: Promise<void> | undefined
  const operation: ReopenedFileSystemAccessDiscardOperation = Object.freeze({
    kind: 'direct-tree',
    intent: session.intent,
    lifecycle,
    binding,
    lease: Object.freeze({ operationId: session.intent.operationId, leaseId }),
    repository,
    close: () => {
      closePromise ??= (async () => {
        closeCount += 1
        await repository.commitTransition({
          operationId: session.intent.operationId,
          expectedLeaseId: leaseId,
          lease: { kind: 'delete', leaseId },
        })
        repository.close()
      })()
      return closePromise
    },
  })
  return Object.freeze({
    parent,
    repository,
    checkpointFactory,
    locks,
    intent: session.intent,
    reservationName: session.reservation.reservedName,
    leaseId,
    operation,
    retired: () => retireCount,
    closed: () => closeCount,
  })
}

function discardFreshFixture(
  fixture: FreshDiscardFixture,
  operation: ReopenedFileSystemAccessDiscardOperation = fixture.operation,
  nowMilliseconds = 2_000,
) {
  return discardReopenedFileSystemAccessOutput({
    operation,
    lockManager: fixture.locks,
    checkpointRepositoryFactory: fixture.checkpointFactory,
    clock: () => nowMilliseconds,
  })
}

async function persistFreshFixtureExpiry(
  fixture: FreshDiscardFixture,
): Promise<Extract<ReceiveLifecycleState, { kind: 'expired' }>> {
  const current = await lifecycleState(fixture.repository, fixture.intent.operationId)
  if (current.kind !== 'resumable-receive') {
    throw new TypeError('fresh discard expiry fixture is not resumable')
  }
  const receipt = await createExpiryReceipt({
    operationId: fixture.intent.operationId,
    receiveIntentDigest: fixture.intent.digest,
    priorStableState: 'resumable-receive',
    expiresAt: current.expiresAt,
    retainedSuccessCount: current.completedFileCount,
    cleanupState: 'cleanup-pending',
  })
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'expiry-observed',
    expiryReceiptDigest: receipt.digest,
    cleanupState: 'cleanup-pending',
    expectedGeneration: current.generation,
    leaseId: fixture.leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: fixture.leaseId,
    nowMilliseconds: current.expiresAt,
  })
  if (reduction.status !== 'applied' || reduction.state.kind !== 'expired') {
    throw new TypeError('fresh discard expiry fixture did not become Expired')
  }
  await fixture.repository.commitTransition({
    operationId: fixture.intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: fixture.leaseId,
    records: [await persistedReceiptRecord(receipt)],
    lifecycle: reduction.state,
  })
  return reduction.state
}

async function bindTask(input: Readonly<{
  parent: MemoryDirectory
  repository: MemoryOperationRepository
  checkpointFactory: FSAFileCheckpointRepositoryFactory
  locks: MemoryLockManager
  artifact: DirectoryTreeArtifact
  operationSeed: number
  trace?: (event: FSAOutputTraceEvent) => void
}>) {
  const selection = await selectionSpec()
  return bindNewFileSystemAccessOutput({
    authority: acquiredParent(input.parent),
    artifact: input.artifact,
    operationRepository: input.repository,
    lockManager: input.locks,
    checkpointRepositoryFactory: input.checkpointFactory,
    operationId: identity(input.operationSeed),
    reservationId: identity(input.operationSeed + 1),
    authorityRef: identity(input.operationSeed + 2, 32),
    ...(input.trace === undefined ? {} : { trace: input.trace }),
    freezeIntent: async (reservation) => createReceiveIntent({
      selection,
      artifact: input.artifact,
      plan: await createDirectTreePlan(input.artifact, reservation),
    }),
  })
}

async function fsaExecution(
  session: FileSystemAccessOutputSession,
  repository: MemoryOperationRepository,
  lifecycleLeaseId: string,
  transferJobId: string,
  outputSessionId: string,
) {
  const settlement = await createFileSystemAccessSettlementAuthority({
    intent: session.intent,
    repository,
    lifecycleLeaseId,
    transferJobId,
    clock: () => 1_000,
  })
  return createPersistentDirectTreeExecution({
    intent: directTreeIntent(session.intent),
    materialization: session,
    outputIdentity: outputSessionIdentity({ backend: 'fsa-test', outputSessionId }),
    settlement: settlement.bindMaterialization(session),
  })
}

function outputFileRequest(input: Readonly<{
  intent: ReceiveIntent
  fileId: string
  fileRevision: string
  artifactPath: readonly string[]
  exactSize: bigint
  parentAdmission?: DirectoryAdmission
}>) {
  return Object.freeze({
    source: Object.freeze({ shareInstance: input.intent.shareInstance, fileId: input.fileId }),
    sourcePath: Object.freeze([...input.artifactPath]),
    artifactPath: Object.freeze([...input.artifactPath]),
    expectedSize: input.exactSize,
    ...(input.parentAdmission === undefined ? {} : { parentAdmission: input.parentAdmission }),
    openRevision: async () => Object.freeze({
      shareInstance: input.intent.shareInstance,
      fileId: input.fileId,
      fileRevision: input.fileRevision,
      exactSize: input.exactSize,
    }),
  })
}

function directTreeIntent(
  intent: ReceiveIntent,
): ReceiveIntent & Readonly<{ plan: DirectTreePlan }> {
  if (intent.plan.kind !== 'direct-tree') throw new TypeError('test intent is not DirectTree')
  return intent as ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
}

async function installIntentFrozen(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  await repository.commitTransition({
    operationId: intent.operationId,
    lifecycle: initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    }),
    lease: {
      kind: 'put',
      record: receiveOperationLeaseRecord({
        operationId: intent.operationId,
        leaseId,
        acquiredAt: 1_000,
      }),
    },
  })
}

async function startReceiving(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  await installIntentFrozen(repository, intent, leaseId)
  const current = await lifecycleState(repository, intent.operationId)
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'receive-started',
    expectedGeneration: current.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: 1_000,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
}

async function resumeReceiving(
  repository: MemoryOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
): Promise<void> {
  const current = await lifecycleState(repository, intent.operationId)
  const reduction = reduceReceiveLifecycle(current, {
    kind: 'resume-started',
    expectedGeneration: current.generation,
    leaseId,
  }, {
    planKind: 'direct-tree',
    preparationRequired: false,
    activeLeaseId: leaseId,
    nowMilliseconds: 1_500,
  })
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
}

async function lifecycleState(
  repository: MemoryOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('test lifecycle is missing')
  return decodeStoredReceiveLifecycleState(record)
}

async function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

async function resultRootArtifact(): Promise<DirectoryTreeArtifact> {
  return createResultRootDirectoryTreeArtifact(
    createCompleteDirectoryResultRoot(identity(70), 'photos'),
  )
}

async function singleFileArtifact(): Promise<DirectoryTreeArtifact> {
  return createSingleFileDirectoryTreeArtifact({
    fileId: identity(3),
    sourcePath: 'report.bin',
    outputName: 'report.bin',
  })
}

function acquiredParent(parent: MemoryDirectory): AcquiredFSAParentAuthority {
  const offer = fsaParentOffer()
  return Object.freeze({
    kind: 'fsa-parent-directory-authority',
    environmentTargetOfferId: offer.id,
    offer,
    parent: parent as unknown as FileSystemDirectoryHandle,
  })
}

interface MemoryOperationStore {
  readonly records: Map<string, PersistedReceiveRecord>
  readonly handles: Map<string, ReceiveOperationHandleRecord>
  readonly leases: Map<string, ReceiveOperationLeaseRecord>
}

class MemoryOperationRepository implements ReceiveOperationRepository {
  readonly records: Map<string, PersistedReceiveRecord>
  readonly handles: Map<string, ReceiveOperationHandleRecord>
  readonly leases: Map<string, ReceiveOperationLeaseRecord>
  afterFirstCommit: (() => Promise<void>) | undefined
  #commitCount = 0

  constructor(state: MemoryOperationStore = {
    records: new Map(),
    handles: new Map(),
    leases: new Map(),
  }) {
    this.records = state.records
    this.handles = state.handles
    this.leases = state.leases
  }

  async commitTransition(transition: ReceiveOperationTransition): Promise<void> {
    if (transition.expectedLifecycleGeneration !== undefined) {
      const current = await this.readLifecycle(transition.operationId)
      if (current === undefined ||
          decodeStoredReceiveLifecycleState(current).generation !==
            transition.expectedLifecycleGeneration) {
        throw new DOMException('lifecycle generation changed', 'InvalidStateError')
      }
    }
    if (transition.expectedLeaseId !== undefined &&
        this.leases.get(transition.operationId)?.leaseId !== transition.expectedLeaseId) {
      throw new DOMException('operation lease changed', 'InvalidStateError')
    }
    const prepared = await prepareReceiveOperationTransition(transition)
    for (const record of prepared.records) this.records.set(record.id, record)
    for (const handle of prepared.handles) this.handles.set(handle.id, handle)
    for (const id of prepared.deleteRecordIds) this.records.delete(id)
    for (const id of prepared.deleteHandleIds) this.handles.delete(id)
    if (prepared.lease?.kind === 'put') {
      this.leases.set(prepared.operationId, prepared.lease.record)
    } else if (prepared.lease?.kind === 'delete') {
      this.leases.delete(prepared.operationId)
    }
    this.#commitCount += 1
    if (this.#commitCount === 1) await this.afterFirstCommit?.()
  }

  async readRecord(id: string): Promise<PersistedReceiveRecord | undefined> {
    return this.records.get(id)
  }

  async readLifecycle(operationId: string): Promise<PersistedReceiveRecord | undefined> {
    return [...this.records.values()].find(record =>
      record.operationId === operationId && record.kind === RECEIVE_RECORD_LIFECYCLE_STATE)
  }

  async listRecords(operationId: string): Promise<readonly PersistedReceiveRecord[]> {
    return [...this.records.values()].filter((record) => record.operationId === operationId)
  }

  async listManifestPages(): Promise<readonly []> {
    return []
  }

  async readHandle<T = unknown>(id: string): Promise<ReceiveOperationHandleRecord<T> | undefined> {
    return this.handles.get(id) as ReceiveOperationHandleRecord<T> | undefined
  }

  async listHandles(operationId: string): Promise<readonly ReceiveOperationHandleRecord[]> {
    return [...this.handles.values()]
      .filter(handle => handle.operationId === operationId)
      .sort((left, right) => left.id.localeCompare(right.id))
  }

  async readLease(operationId: string): Promise<ReceiveOperationLeaseRecord | undefined> {
    return this.leases.get(operationId)
  }

  recordsOfKind(kind: PersistedReceiveRecord['kind']): readonly PersistedReceiveRecord[] {
    return [...this.records.values()].filter(record => record.kind === kind)
  }

  close(): void {}
}

class MemoryLockManager implements BrowserLockManagerRuntime {
  readonly #held = new Set<string>()
  releaseCount = 0

  async request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: { readonly name: string } | null) => Promise<void>,
  ): Promise<void> {
    if (this.#held.has(name)) {
      await callback(null)
      return
    }
    this.#held.add(name)
    try {
      await callback({ name })
    } finally {
      this.#held.delete(name)
      this.releaseCount += 1
    }
  }
}

interface MemoryCheckpointStore {
  readonly candidates: Map<string, FileCheckpointV2>
  readonly committed: Map<string, FileCheckpointV2>
  readonly handles: Map<string, PersistentHandleRecord>
}

function memoryCheckpointFactory(onRetire?: () => void): FSAFileCheckpointRepositoryFactory {
  const stores = new Map<string, MemoryCheckpointStore>()
  return async (binding) => {
    let store = stores.get(binding.operationId)
    if (store === undefined) {
      store = { candidates: new Map(), committed: new Map(), handles: new Map() }
      stores.set(binding.operationId, store)
    }
    return new MemoryCheckpointRepository(binding, store, onRetire)
  }
}

class MemoryCheckpointRepository implements FSAFileCheckpointRepository {
  readonly binding: CheckpointNamespaceBinding
  readonly #store: MemoryCheckpointStore
  readonly #onRetire: (() => void) | undefined

  constructor(
    binding: CheckpointNamespaceBinding,
    store: MemoryCheckpointStore,
    onRetire?: () => void,
  ) {
    this.binding = binding
    this.#store = store
    this.#onRetire = onRetire
  }

  async putCandidate(record: FileCheckpointV2): Promise<void> {
    validateFileCheckpoint(record)
    if (!checkpointMatchesNamespace(record, this.binding) ||
        record.commitState !== FILE_CHECKPOINT_COMMIT_CANDIDATE) {
      throw new TypeError('candidate escaped its operation')
    }
    const previous = this.#store.candidates.get(record.recordId)
    if (previous !== undefined) validateFileCheckpointTransition(previous, record)
    this.#store.candidates.set(record.recordId, record)
  }

  async commit(record: FileCheckpointV2): Promise<void> {
    const candidate = this.#store.candidates.get(record.recordId)
    if (candidate === undefined || record.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED) {
      throw new DOMException('checkpoint candidate missing', 'InvalidStateError')
    }
    validateFileCheckpointTransition(candidate, record)
    const previous = this.#store.committed.get(record.recordId)
    if (previous !== undefined) validateFileCheckpointTransition(previous, record)
    this.#store.committed.set(record.recordId, record)
    this.#store.candidates.delete(record.recordId)
  }

  async readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined> {
    return this.#store.committed.get(recordId)
  }

  async scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return scanRecords(this.#store.committed, scan)
  }

  async scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
    return scanRecords(this.#store.candidates, scan)
  }

  async finalCheckpointProof(
    recordId: string,
    generation: bigint,
  ): Promise<FinalFileCheckpointProof> {
    const record = this.#store.committed.get(recordId)
    if (record === undefined || record.checkpointGeneration !== generation ||
        !fileCheckpointIsComplete(record)) {
      throw new DOMException('final checkpoint missing', 'NotFoundError')
    }
    return finalFileCheckpointProof(record)
  }

  async retireOperation(): Promise<void> {
    this.#store.candidates.clear()
    this.#store.committed.clear()
    this.#store.handles.clear()
    this.#onRetire?.()
  }

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.#store.handles.set(record.id, record)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    return this.#store.handles.get(id)
  }

  async listHandles(): Promise<readonly PersistentHandleRecord[]> {
    return [...this.#store.handles.values()].sort((left, right) => left.id.localeCompare(right.id))
  }

  async deleteHandle(id: string): Promise<void> {
    this.#store.handles.delete(id)
  }

  close(): void {}
}

function scanRecords(
  records: ReadonlyMap<string, FileCheckpointV2>,
  scan: FileCheckpointScan,
): FileCheckpointPage {
  const sorted = [...records.values()]
    .filter((record) => scan.fileId === undefined || record.fileId === scan.fileId)
    .sort((left, right) => {
      if (left.recordId === right.recordId) return 0
      return left.recordId < right.recordId ? -1 : 1
    })
  if (scan.direction === 'descending') sorted.reverse()
  const after = scan.cursor === undefined
    ? sorted
    : sorted.filter((record) => scan.direction === 'ascending'
        ? record.recordId > scan.cursor!
        : record.recordId < scan.cursor!)
  const limit = scan.limit ?? 128
  const page = after.slice(0, limit)
  return Object.freeze({
    records: Object.freeze(page),
    ...(after.length >= limit && page.at(-1) !== undefined
      ? { nextCursor: page.at(-1)!.recordId }
      : {}),
  })
}

class MemoryDirectory {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  readonly #entries = new Map<string, MemoryDirectory | MemoryFile>()
  onFileCreated: (() => void) | undefined
  onRemoveEntry: ((name: string) => Promise<void>) | undefined

  constructor(name: string) {
    this.name = name
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return (other as MemoryDirectory).#token === this.#token
  }

  async queryPermission(): Promise<PermissionState> {
    return 'granted'
  }

  async requestPermission(): Promise<PermissionState> {
    return 'granted'
  }

  async getDirectoryHandle(
    name: string,
    options?: FileSystemGetDirectoryOptions,
  ): Promise<FileSystemDirectoryHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryDirectory) return existing as unknown as FileSystemDirectoryHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryDirectory(name)
    this.#entries.set(name, created)
    return created as unknown as FileSystemDirectoryHandle
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    const existing = this.#entries.get(name)
    if (existing instanceof MemoryFile) return existing as unknown as FileSystemFileHandle
    if (existing !== undefined) throw domError('TypeMismatchError')
    if (options?.create !== true) throw domError('NotFoundError')
    const created = new MemoryFile(name)
    this.#entries.set(name, created)
    this.onFileCreated?.()
    return created as unknown as FileSystemFileHandle
  }

  async removeEntry(name: string): Promise<void> {
    await this.onRemoveEntry?.(name)
    if (!this.#entries.delete(name)) throw domError('NotFoundError')
  }

  async *entries(): AsyncIterableIterator<[string, FileSystemHandle]> {
    for (const [name, handle] of [...this.#entries]) {
      yield [name, handle as unknown as FileSystemHandle]
    }
  }

  directoryNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryDirectory] => entry[1] instanceof MemoryDirectory)
      .map(([name]) => name)
      .sort()
  }

  fileNames(): string[] {
    return [...this.#entries.entries()]
      .filter((entry): entry is [string, MemoryFile] => entry[1] instanceof MemoryFile)
      .map(([name]) => name)
      .sort()
  }

  entryNames(): string[] {
    return [...this.#entries.keys()].sort()
  }

  async fileBytes(name: string): Promise<Uint8Array> {
    const file = this.#entries.get(name)
    if (!(file instanceof MemoryFile)) throw new Error('memory file is missing')
    return file.bytes()
  }

  replaceFile(name: string, bytes: Uint8Array): MemoryFile {
    const file = new MemoryFile(name, Uint8Array.from(bytes))
    this.#entries.set(name, file)
    return file
  }
}

class MemoryFile {
  readonly kind = 'file' as const
  readonly name: string
  readonly #token = crypto.randomUUID()
  #bytes: Uint8Array

  constructor(name: string, bytes = new Uint8Array()) {
    this.name = name
    this.#bytes = bytes.slice()
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other instanceof MemoryFile && other.#token === this.#token
  }

  async getFile(): Promise<File> {
    const copy = new Uint8Array(this.#bytes.byteLength)
    copy.set(this.#bytes)
    return new Blob([copy.buffer]) as File
  }

  async createWritable(
    options?: FileSystemCreateWritableOptions,
  ): Promise<FileSystemWritableFileStream> {
    if (options?.keepExistingData !== true) this.#bytes = new Uint8Array()
    return {
      write: async (data: FileSystemWriteChunkType) => {
        if (typeof data !== 'object' || data === null || !('type' in data) || data.type !== 'write') {
          throw new TypeError('memory writer requires positioned writes')
        }
        const command = data as WriteParams
        if (!(command.data instanceof Uint8Array)) {
          throw new TypeError('memory writer accepts Uint8Array writes')
        }
        const position = Number(command.position ?? 0)
        const source = command.data
        const next = new Uint8Array(Math.max(this.#bytes.byteLength, position + source.byteLength))
        next.set(this.#bytes)
        next.set(source, position)
        this.#bytes = next
      },
      close: async () => {},
    } as unknown as FileSystemWritableFileStream
  }

  async bytes(): Promise<Uint8Array> {
    return this.#bytes.slice()
  }
}

function domError(name: string): DOMException {
  return new DOMException(name, name)
}
