import { describe, expect, it } from 'vitest'

import {
  FileSystemAccessOutputSession,
  type FileSystemAccessInnerSession,
  type FileSystemAccessSessionLease,
  type FileSystemAccessSessionRepository,
} from '../../src/output/file-system-access/session'
import {
  type BeginOutputFileResult,
  outputCapabilities,
  outputSessionIdentity,
} from '../../src/transfer/output-session'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome, type JobOutcome } from '../../src/transfer/outcome'

const OUTCOME = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const ACTIVE_SIGNAL = new AbortController().signal

describe('File System Access output lifecycle', () => {
  it('lets published finish own deferred repository cleanup despite a reentrant abort', async () => {
    const inner = new DeferredInner(false)
    const repository = new DeferredRepository(true)
    const lease = new RecordingLease()
    const output = new FileSystemAccessOutputSession(inner, repository, lease)
    let abort: Promise<void> | undefined
    inner.onPublished = () => {
      abort = output.abortJob(new DOMException('late cancellation', 'AbortError'))
    }

    const finish = output.finishJob(OUTCOME, ACTIVE_SIGNAL)
    await inner.finishStarted.promise
    inner.publish()
    await repository.deleteStarted.promise
    await Promise.resolve()

    expect(repository.deleteCalls).toBe(1)
    expect(repository.closed).toBe(false)
    repository.releaseDelete()
    await expect(finish).resolves.toBeUndefined()
    if (abort === undefined) throw new Error('Inner session did not trigger the publication race')
    await expect(abort).resolves.toBeUndefined()
    expect(inner.abortCalls).toBe(0)
    expect(repository.closeCalls).toBe(1)
    expect(lease.releaseCalls).toBe(1)
  })

  it('aborts a deferred pre-publication finish before deleting output state', async () => {
    const inner = new DeferredInner(true)
    const repository = new DeferredRepository(false)
    const lease = new RecordingLease()
    const output = new FileSystemAccessOutputSession(inner, repository, lease)
    const reason = new DOMException('cancelled before publication', 'AbortError')

    const finish = output.finishJob(OUTCOME, ACTIVE_SIGNAL)
    await inner.finishStarted.promise
    const abort = output.abortJob(reason)

    await expect(finish).rejects.toBe(reason)
    await expect(abort).resolves.toBeUndefined()
    expect(inner.abortCalls).toBe(1)
    expect(repository.deleteCalls).toBe(1)
    expect(repository.closeCalls).toBe(1)
    expect(lease.releaseCalls).toBe(1)
  })
})

class DeferredInner implements FileSystemAccessInnerSession {
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

  async ensureDirectory(): Promise<void> {}

  async finalizeDirectory(): Promise<void> {}

  async beginFile(): Promise<BeginOutputFileResult> {
    throw new Error('Lifecycle test does not open files')
  }

  async finishJob(_outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    this.finishStarted.resolve()
    if (this.#honorAbort) {
      await abortable(this.#publication.promise, signal)
    } else {
      await this.#publication.promise
    }
    this.onPublished?.()
  }

  async abortJob(): Promise<void> {
    this.abortCalls += 1
  }

  async suspendJob(): Promise<void> {}

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
