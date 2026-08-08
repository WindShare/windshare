import { describe, expect, it } from 'vitest'

import {
  FileSystemAccessOutputSession,
  type FileSystemAccessInnerSession,
  type FileSystemAccessSessionLease,
  type FileSystemAccessSessionRepository,
} from '../../src/output/file-system-access/session'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import {
  type BeginOutputFileResult,
  type DirectoryAdmission,
  type DirectorySettlement,
  type JobSettlement,
  COMPLETED_JOB_SETTLEMENT,
  JobSettlementKind,
  type OutputDirectoryAdmission,
  type OutputFile,
  outputCapabilities,
  outputSessionIdentity,
} from '../../src/transfer/output-session'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome, type JobOutcome } from '../../src/transfer/outcome'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import {
  TEST_DIRECTORY_ADMISSION_SCOPE,
  testOutputIdentity,
} from './admission-fixture'
import { MEMORY_CHECKPOINT_BINDING, MemoryOutputJournal, MemoryOutputTree } from './fakes'

const OUTCOME = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const ACTIVE_SIGNAL = new AbortController().signal

describe('File System Access output lifecycle', () => {
  it('lets published finish own deferred repository cleanup despite a reentrant abort', async () => {
    const inner = new DeferredInner(false)
    const repository = new DeferredRepository(true)
    const lease = new RecordingLease()
    const output = new FileSystemAccessOutputSession(inner, repository, lease)
    let pause: Promise<JobSettlement> | undefined
    inner.onPublished = () => {
      pause = output.pauseJob(new DOMException('late pause', 'AbortError'))
    }

    const finish = output.completeJob(OUTCOME, ACTIVE_SIGNAL)
    await inner.finishStarted.promise
    inner.publish()
    await repository.deleteStarted.promise
    await Promise.resolve()

    expect(repository.deleteCalls).toBe(1)
    expect(repository.closed).toBe(false)
    repository.releaseDelete()
    await expect(finish).resolves.toEqual(COMPLETED_JOB_SETTLEMENT)
    if (pause === undefined) throw new Error('Inner session did not trigger the publication race')
    await expect(pause).resolves.toMatchObject({ kind: JobSettlementKind.NeedsAttention })
    expect(inner.abortCalls).toBe(0)
    expect(repository.closeCalls).toBe(1)
    expect(lease.releaseCalls).toBe(1)
  })

  it('reports attention instead of letting a late pause override completion', async () => {
    const inner = new DeferredInner(true)
    const repository = new DeferredRepository(false)
    const lease = new RecordingLease()
    const output = new FileSystemAccessOutputSession(inner, repository, lease)
    const reason = new DOMException('cancelled before publication', 'AbortError')

    const finish = output.completeJob(OUTCOME, ACTIVE_SIGNAL)
    await inner.finishStarted.promise
    const pause = output.pauseJob(reason)
    await expect(pause).resolves.toMatchObject({ kind: JobSettlementKind.NeedsAttention })
    inner.publish()
    await expect(finish).resolves.toEqual(COMPLETED_JOB_SETTLEMENT)
    expect(inner.abortCalls).toBe(0)
    expect(repository.deleteCalls).toBe(1)
    expect(repository.closeCalls).toBe(1)
    expect(lease.releaseCalls).toBe(1)
  })

  for (const race of [
    { close: 'pause', phase: 'DataWritten' },
    { close: 'complete', phase: 'CheckpointVerified' },
  ] as const) {
    it(`releases repository and namespace authority only after ${race.close} drains file mutation`, async () => {
      const reached = deferred<void>()
      const release = deferred<void>()
      let blocked = false
      const tree = new MemoryOutputTree()
      const inner = await PersistentTreeOutputSession.open({
        identity: outputSessionIdentity({
          backend: 'file-system-access',
          outputSessionId: `stable-${race.close}`,
        }),
        directoryAdmissionScope: TEST_DIRECTORY_ADMISSION_SCOPE,
        tree,
        journal: new MemoryOutputJournal([], {
          ...MEMORY_CHECKPOINT_BINDING,
          backend: 'file-system-access',
        }),
        crashHook: async (phase) => {
          if (phase !== race.phase || blocked) return
          blocked = true
          reached.resolve()
          await release.promise
        },
      })
      const repository = new DeferredRepository(false)
      const lease = new RecordingLease()
      const output = new FileSystemAccessOutputSession(inner, repository, lease)
      const root = await output.admitDirectory({
        directoryId: TEST_DIRECTORY_ADMISSION_SCOPE.syntheticRoot,
        generation: testOutputIdentity(`fsa-root-generation:${race.close}`),
        path: [],
      }, ACTIVE_SIGNAL)
      const file: OutputFile = {
        source: {
          shareInstance: testOutputIdentity('fsa-share'),
          fileId: testOutputIdentity(`fsa-file:${race.close}`),
          fileRevision: testOutputIdentity(`fsa-revision:${race.close}`),
        },
        path: [`${race.close}.bin`],
        exactSize: 1n,
        parentAdmission: root,
      }
      const begun = await output.beginFile(file, ACTIVE_SIGNAL)
      if (race.close === 'complete') {
        await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
      }
      const mutation = race.close === 'pause'
        ? begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
        : begun.transaction.commit(ACTIVE_SIGNAL)
      await reached.promise

      const closing = race.close === 'pause'
        ? output.pauseJob(new Error('stable drain'))
        : output.completeJob(OUTCOME, ACTIVE_SIGNAL)
      await Promise.resolve()
      expect(repository.closeCalls).toBe(0)
      expect(lease.releaseCalls).toBe(0)

      release.resolve()
      await mutation
      await expect(closing).resolves.toMatchObject({
        kind: race.close === 'pause' ? JobSettlementKind.Paused : JobSettlementKind.Completed,
      })
      expect(repository.deleteCalls).toBe(race.close === 'complete' ? 1 : 0)
      expect(repository.closeCalls).toBe(1)
      expect(lease.releaseCalls).toBe(1)
    })
  }
})

class DeferredInner implements FileSystemAccessInnerSession {
  readonly #directoryAdmissions = new DirectoryAdmissionLedger(TEST_DIRECTORY_ADMISSION_SCOPE)
  readonly format = 'directory' as const
  readonly identity = outputSessionIdentity({
    backend: 'file-system-access',
    outputSessionId: 'lifecycle-test',
  })
  readonly capabilities = outputCapabilities({
    durability: 'ProcessRestart',
    randomWrite: true,
    fileFailureIsolation: true,
    modificationTime: true,
  })
  readonly finishStarted = deferred<void>()
  readonly #publication = deferred<void>()
  readonly #honorAbort: boolean
  abortCalls = 0
  onPublished: (() => void) | undefined

  constructor(honorAbort: boolean) {
    this.#honorAbort = honorAbort
  }

  admitDirectory(directory: OutputDirectoryAdmission, signal: AbortSignal): Promise<DirectoryAdmission> {
    return this.#directoryAdmissions.admitDirectory(directory, signal)
  }

  finalizeDirectory(
    admission: DirectoryAdmission,
    signal: AbortSignal,
  ): Promise<DirectorySettlement> {
    return this.#directoryAdmissions.finalizeDirectory(admission, signal)
  }

  async beginFile(): Promise<BeginOutputFileResult> {
    throw new Error('Lifecycle test does not open files')
  }

  async completeJob(_outcome: JobOutcome, signal: AbortSignal): Promise<JobSettlement> {
    this.finishStarted.resolve()
    if (this.#honorAbort) {
      await abortable(this.#publication.promise, signal)
    } else {
      await this.#publication.promise
    }
    this.onPublished?.()
    return COMPLETED_JOB_SETTLEMENT
  }

  async pauseJob(): Promise<JobSettlement> {
    this.abortCalls += 1
    return Object.freeze({ kind: JobSettlementKind.Paused, durability: 'ProcessRestart' })
  }

  publish(): void {
    this.#publication.resolve()
  }
}

class DeferredRepository implements FileSystemAccessSessionRepository {
  readonly deleteStarted = deferred<void>()
  readonly #delete = deferred<void>()
  readonly #deferDelete: boolean
  deleteCalls = 0
  closeCalls = 0
  closed = false

  constructor(deferDelete: boolean) {
    this.#deferDelete = deferDelete
  }

  async deleteSessionData(): Promise<void> {
    this.deleteCalls += 1
    if (this.deleteCalls > 1) throw new Error('Repository cleanup ran twice')
    if (this.closed) throw new Error('Repository was touched after close')
    this.deleteStarted.resolve()
    if (this.#deferDelete) await this.#delete.promise
  }

  close(): void {
    this.closeCalls += 1
    if (this.closeCalls > 1) throw new Error('Repository closed twice')
    this.closed = true
  }

  releaseDelete(): void {
    this.#delete.resolve()
  }
}

class RecordingLease implements FileSystemAccessSessionLease {
  releaseCalls = 0

  async release(): Promise<void> {
    this.releaseCalls += 1
    if (this.releaseCalls > 1) throw new Error('Lease released twice')
  }
}

async function abortable(operation: Promise<void>, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  let detach = () => {}
  const aborted = new Promise<never>((_resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    detach = () => signal.removeEventListener('abort', abort)
  })
  try {
    await Promise.race([operation, aborted])
  } finally {
    detach()
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
  readonly reject: (reason?: unknown) => void
} {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((complete, fail) => {
    resolve = complete
    reject = fail
  })
  return { promise, resolve, reject }
}
