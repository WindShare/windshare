import { describe, expect, it, vi } from 'vitest'
import { cleanupReopenedPublishedFileSystemAccessOutput } from '../../src/output/file-system-access/published-cleanup'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import { createPersistedReceiveOperationMutationPort } from '../../src/output/resume/reopen-authority'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import { retainedOperationAuthority } from '../../src/ui/browser-receive/retained-operation-authority'
import {
  operationRecordId, RECEIVE_RECORD_CLEANUP, RECEIVE_RECORD_RECEIPT,
  receiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import type { ReceiveOperationTransition } from '../../src/output/workspace/repository'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import {
  absentCompatibleNameLedgerFactory, bindTask, fsaExecution, MemoryDirectory,
  MemoryLockManager, MemoryOperationRepository, memoryCheckpointFactory, type MemoryFile,
  outputFileRequest, SIGNAL, singleFileArtifact, startReceiving, SUCCESS,
} from './file-system-access-lifecycle-fixture'
import { MemoryLockManager as OperationLockManager } from './resume-reopen-authority-fixture'
import { identity } from './planning/fixture'

const CLEANUP_FAILURE = new DOMException('metadata retirement interrupted', 'UnknownError')
const FILE_BYTES = Uint8Array.of(4, 5)
const CLOCK = { now: () => 2_000 }

async function publishedFile(retirementFailures = 1) {
  const parent = new MemoryDirectory('downloads')
  const repository = new MemoryOperationRepository()
  const trace: OutputTraceEvent[] = []
  const outputTrace = { current: (event: OutputTraceEvent) => { trace.push(event) } }
  let attempts = 0
  const checkpointFactory = memoryCheckpointFactory(undefined, () => {
    attempts += 1
    if (attempts <= retirementFailures) throw CLEANUP_FAILURE
  })
  const session = await bindTask({
    parent, repository, checkpointFactory, locks: new MemoryLockManager(),
    artifact: await singleFileArtifact(), operationSeed: 101,
  })
  const intent = session.intent
  const leaseId = identity(102)
  const transferJobId = identity(103)
  await startReceiving(repository, intent, leaseId)
  const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(104))
  const opened = await execution.output.beginFile(await outputFileRequest({
    intent, fileId: identity(3), fileRevision: identity(105), exactSize: 2n,
  }), SIGNAL)
  await opened.transaction.writeRange(0n, FILE_BYTES, SIGNAL)
  await opened.transaction.commit(SIGNAL)
  await execution.settle({
    transferJobId, worker: SUCCESS,
    materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n },
  }, SIGNAL)
  await session.releaseRootLease()
  const file = await parent.getFileHandle(session.reservation.physicalName) as unknown as MemoryFile
  const current = async () => {
    const record = await repository.readLifecycle(intent.operationId)
    if (record === undefined) throw new Error('published lifecycle is missing')
    const state = decodeStoredReceiveLifecycleState(record)
    if (state.kind !== 'published') throw new Error('publication was lost')
    return state
  }
  const snapshots: MemoryOperationRepository[] = []
  const mutations = createPersistedReceiveOperationMutationPort({
    repositoryFactory: async () => {
      const reopened = new MemoryOperationRepository(repository)
      snapshots.push(reopened)
      return reopened
    },
    clock: CLOCK,
    leaseOptions: { manager: new OperationLockManager() },
    outputTrace,
    cleanupPublishedDirectTree: input => cleanupReopenedPublishedFileSystemAccessOutput({
      ...input, checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
    }),
  })
  const authority = new ReceiveOperationResumeAuthority({
    source: { listLifecycleStates: async () => [await current()] },
    mutations,
  })
  const retry = async () => {
    const inventory = await authority.listResumeState()
    try {
      const reference = inventory.operations[0]!
      expect(reference.descriptor.continuation).toBe('retry-cleanup')
      return await authority.cleanup(reference)
    } finally {
      inventory.close()
    }
  }
  return {
    parent, repository, intent, leaseId, checkpointFactory, current, retry,
    file, snapshots, trace, attempts: () => attempts,
  }
}

async function cleanupInput(fixture: Awaited<ReturnType<typeof publishedFile>>) {
  return {
    intent: fixture.intent, lifecycle: await fixture.current(),
    repository: fixture.repository, leaseId: fixture.leaseId,
    checkpointRepositoryFactory: fixture.checkpointFactory,
    openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
  }
}

describe('published DirectTree metadata cleanup', () => {
  it('reopens pending publication through retained cleanup without touching completed files', async () => {
    const fixture = await publishedFile()
    const before = await fixture.current()
    expect(before.cleanupState).toBe('cleanup-pending')
    fixture.parent.onEntryLookup = () => { throw new Error('cleanup must not reopen output entries') }
    fixture.parent.onRemoveEntry = async () => { throw new Error('cleanup must not delete output entries') }

    const result = await fixture.retry()
    expect(result).toMatchObject({
      kind: 'cleanup', result: { kind: 'published-cleanup-completed' },
    })
    const after = await fixture.current()
    expect(after).toMatchObject({
      kind: 'published', cleanupState: 'clean', receiptDigest: before.receiptDigest,
      generation: before.generation + 1n,
    })
    expect(await fixture.file.bytes()).toEqual(FILE_BYTES)
    expect(fixture.snapshots).toHaveLength(1)
    expect(fixture.snapshots[0]).not.toBe(fixture.repository)
    expect(await fixture.repository.readLease(fixture.intent.operationId)).toBeUndefined()
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    const continuation = receiveOperationResumeDescriptor(after)!.continuation
    expect(retainedOperationAuthority(continuation, true, false, false).actions).not.toContain('delete')
    expect(fixture.trace).toContainEqual(expect.objectContaining({
      eventName: 'cleanup', payload: expect.objectContaining({
        transition: 'completed', cleanup_kind: 'published_metadata',
        operation_id: fixture.intent.operationId, lifecycle_generation: after.generation.toString(),
      }),
    }))
  })

  it('finishes an old pending publication whose recovery metadata is already absent', async () => {
    const fixture = await publishedFile(0)
    const clean = await fixture.current()
    expect(clean.cleanupState).toBe('clean')
    await fixture.repository.commitTransition({
      operationId: fixture.intent.operationId,
      expectedLifecycleGeneration: clean.generation,
      lifecycle: { ...clean, generation: clean.generation + 1n, cleanupState: 'cleanup-pending' },
    })
    await fixture.retry()
    expect((await fixture.current()).cleanupState).toBe('clean')
    expect(await fixture.file.bytes()).toEqual(FILE_BYTES)
  })

  it('keeps publication pending and releases the reopened lease when retirement fails, then retries', async () => {
    const fixture = await publishedFile(2)
    const before = await fixture.current()
    await expect(fixture.retry()).rejects.toBe(CLEANUP_FAILURE)
    expect(await fixture.current()).toEqual(before)
    expect(await fixture.repository.readLease(fixture.intent.operationId)).toBeUndefined()
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(0)
    await fixture.retry()
    expect((await fixture.current()).cleanupState).toBe('clean')
    expect(fixture.attempts()).toBe(3)
    expect(await fixture.file.bytes()).toEqual(FILE_BYTES)
    expect(fixture.trace).toContainEqual(expect.objectContaining({
      eventName: 'cleanup', payload: expect.objectContaining({ transition: 'retryable_failure' }),
    }))
  })

  it.each(['stale-generation', 'foreign-lease', 'missing-receipt'] as const)(
    'rejects %s before retiring recovery metadata', async fault => {
      const fixture = await publishedFile()
      const input = await cleanupInput(fixture)
      if (fault === 'stale-generation') {
        await fixture.repository.commitTransition({
          operationId: fixture.intent.operationId,
          expectedLifecycleGeneration: input.lifecycle.generation,
          lifecycle: { ...input.lifecycle, generation: input.lifecycle.generation + 1n },
        })
      } else if (fault === 'foreign-lease') {
        fixture.repository.leases.set(fixture.intent.operationId, receiveOperationLeaseRecord({
          operationId: fixture.intent.operationId, leaseId: identity(120), acquiredAt: CLOCK.now(),
        }))
      } else {
        fixture.repository.records.delete(operationRecordId(
          fixture.intent.operationId, RECEIVE_RECORD_RECEIPT, input.lifecycle.receiptDigest,
        ))
      }
      await expect(cleanupReopenedPublishedFileSystemAccessOutput(input)).rejects.toThrow()
      expect(fixture.attempts()).toBe(1)
      expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(0)
      expect(await fixture.file.bytes()).toEqual(FILE_BYTES)
    },
  )

  it.each(['before-commit', 'after-commit'] as const)(
    'reconciles cleanup persistence failure %s without losing publication', async fault => {
      const fixture = await publishedFile()
      const input = await cleanupInput(fixture)
      const commit = fixture.repository.commitTransition.bind(fixture.repository)
      const failure = new DOMException('cleanup commit interrupted', 'UnknownError')
      const spy = vi.spyOn(fixture.repository, 'commitTransition').mockImplementation(
        async (transition: ReceiveOperationTransition) => {
          if (fault === 'after-commit') await commit(transition)
          throw failure
        },
      )
      if (fault === 'before-commit') {
        await expect(cleanupReopenedPublishedFileSystemAccessOutput(input)).rejects.toBe(failure)
        expect(await fixture.current()).toEqual(input.lifecycle)
        expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(0)
        spy.mockRestore()
        await cleanupReopenedPublishedFileSystemAccessOutput(input)
      } else {
        await expect(cleanupReopenedPublishedFileSystemAccessOutput(input)).resolves.toMatchObject({
          lifecycle: { kind: 'published', cleanupState: 'clean' },
        })
        spy.mockRestore()
      }
      expect((await fixture.current()).receiptDigest).toBe(input.lifecycle.receiptDigest)
      expect(await fixture.file.bytes()).toEqual(FILE_BYTES)
    },
  )

  it('does not expose the published metadata path to retained unfinished work', async () => {
    const fixture = await publishedFile()
    const input = await cleanupInput(fixture)
    const receiving: ReceiveLifecycleState = {
      kind: 'receiving', operationId: fixture.intent.operationId,
      receiveIntentDigest: fixture.intent.digest, generation: input.lifecycle.generation + 1n,
      activeLeaseId: fixture.leaseId,
    }
    await fixture.repository.commitTransition({
      operationId: fixture.intent.operationId, lifecycle: receiving,
      expectedLifecycleGeneration: input.lifecycle.generation,
    })
    await expect(cleanupReopenedPublishedFileSystemAccessOutput(input)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    expect(fixture.attempts()).toBe(1)
  })
})
