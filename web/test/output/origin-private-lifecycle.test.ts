import { describe, expect, it } from 'vitest'

import type {
  AdmissionAggregateLimits,
  AdmissionLeaseRecord,
  OriginPrivateAdmissionAuthority,
} from '../../src/output/origin-private/admission-authority'
import { OriginPrivateStagingAdmission } from '../../src/output/origin-private/admission'
import {
  ORIGIN_PRIVATE_EXPORT_COMPLETE,
  OriginPrivateOutputSession,
  type OriginPrivateExportResult,
  type OriginPrivateOutputExporter,
} from '../../src/output/origin-private/session'
import {
  PersistentTreeOutputSession,
  type StagedOutputCatalog,
} from '../../src/output/persistent-tree/session'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome, type JobOutcome } from '../../src/transfer/outcome'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'
import { admittedOutputFile, testOutputIdentity } from './admission-fixture'

const IDENTITY = Object.freeze({
  backend: 'origin-private-staging',
  outputSessionId: 'lifecycle-test',
})
const OUTCOME = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const ACTIVE_SIGNAL = new AbortController().signal

describe('origin-private output lifecycle', () => {
  it('lets cancellation interrupt a blocked export before publication', async () => {
    const exporter = new ControlledExporter(false)
    const fixture = await lifecycleFixture(exporter, false)
    const finish = fixture.output.finishJob(OUTCOME, ACTIVE_SIGNAL)
    await exporter.started.promise
    const reason = new DOMException('cancelled while exporting', 'AbortError')
    const abort = fixture.output.abortJob(reason)

    await expect(finish).rejects.toBe(reason)
    await expect(abort).resolves.toBeUndefined()
    expect(exporter.abortReasons).toEqual([reason])
    expect(fixture.settlements).toEqual([true])
  })

  it('preserves a publication that wins the abort race', async () => {
    const exporter = new ControlledExporter(true)
    const fixture = await lifecycleFixture(exporter, true)
    const finish = fixture.output.finishJob(OUTCOME, ACTIVE_SIGNAL)
    await exporter.started.promise
    const abort = fixture.output.abortJob(new DOMException('late cancellation', 'AbortError'))
    exporter.releasePublication()

    await expect(finish).resolves.toBeUndefined()
    await expect(abort).resolves.toBeUndefined()
    expect(fixture.settlements).toEqual([false])
  })

  it('reports published success while cleanup remains retryable', async () => {
    const admission = await openAdmission(new MemoryAdmissionAuthority())
    let cleanupAttempts = 0
    const output = new OriginPrivateOutputSession(
      await openInner(),
      {
        export: async () => ORIGIN_PRIVATE_EXPORT_COMPLETE,
        abort: async () => {},
      },
      admission,
      async () => {
        cleanupAttempts += 1
        if (cleanupAttempts === 1) throw new Error('staging cleanup is temporarily blocked')
        await admission.release()
      },
      false,
    )

    await expect(output.finishJob(OUTCOME, ACTIVE_SIGNAL)).resolves.toBeUndefined()
    expect(output.finalization).toMatchObject({
      committed: true,
      outcome: OUTCOME,
      cleanupPending: true,
    })

    await expect(output.abortJob(new DOMException('late cancellation', 'AbortError')))
      .resolves.toBeUndefined()
    expect(output.finalization).toMatchObject({
      committed: true,
      outcome: OUTCOME,
      cleanupPending: false,
    })
    expect(cleanupAttempts).toBe(2)
  })

  it('singleflights explicit and abort-driven cleanup retries', async () => {
    const retryStarted = deferred<void>()
    const releaseRetry = deferred<void>()
    let retryCalls = 0
    const exporter: OriginPrivateOutputExporter = {
      export: async () => Object.freeze({ cleanupPending: true }),
      abort: async () => {},
      retryCleanup: async () => {
        retryCalls += 1
        if (retryCalls === 1) return Object.freeze({ cleanupPending: true })
        retryStarted.resolve()
        await releaseRetry.promise
        return ORIGIN_PRIVATE_EXPORT_COMPLETE
      },
    }
    const fixture = await lifecycleFixture(exporter, false)
    await fixture.output.finishJob(OUTCOME, ACTIVE_SIGNAL)
    expect(fixture.output.finalization?.cleanupPending).toBe(true)

    const explicit = fixture.output.retryCleanup()
    await retryStarted.promise
    const repeated = fixture.output.retryCleanup()
    const abort = fixture.output.abortJob(new DOMException('late cancellation', 'AbortError'))

    expect(repeated).toBe(explicit)
    expect(retryCalls).toBe(2)
    releaseRetry.resolve()
    await expect(explicit).resolves.toMatchObject({ cleanupPending: false, outcome: OUTCOME })
    await expect(repeated).resolves.toMatchObject({ cleanupPending: false, outcome: OUTCOME })
    await expect(abort).resolves.toBeUndefined()
    expect(fixture.output.finalization).toMatchObject({
      committed: true,
      cleanupPending: false,
      outcome: OUTCOME,
    })
  })

  it('retains cleanup authority so the same session object can retry', async () => {
    const exporter = new ControlledExporter(false)
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const inner = await openInner()
    let cleanupAttempts = 0
    const output = new OriginPrivateOutputSession(
      inner,
      exporter,
      admission,
      async () => {
        cleanupAttempts += 1
        if (cleanupAttempts === 1) throw new Error('injected cleanup failure')
        await admission.release()
      },
      false,
    )

    await expect(output.abortJob(new Error('transfer failed'))).rejects.toThrow('cleanup failed')
    await expect(output.abortJob(new Error('retry cleanup'))).resolves.toBeUndefined()
    expect(cleanupAttempts).toBe(2)
  })

  it('fails admission before commit can publish an unabortable staged file', async () => {
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const tree = new MemoryOutputTree()
    const inner = await PersistentTreeOutputSession.open({
      identity: IDENTITY,
      tree,
      journal: new MemoryOutputJournal(),
    })
    const output = new OriginPrivateOutputSession(
      inner,
      new ControlledExporter(false),
      admission,
      async () => {},
      false,
    )
    const file = {
      source: {
        shareInstance: testOutputIdentity('commit-boundary-share'),
        fileId: testOutputIdentity('commit-boundary-file'),
        fileRevision: testOutputIdentity('commit-boundary-revision'),
      },
      path: ['commit-boundary.bin'],
      exactSize: 1n,
    }
    const begun = await output.beginFile(await admittedOutputFile(output, file), ACTIVE_SIGNAL)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
    authority.failNextUpdate(new Error('admission version became obsolete'))

    await expect(begun.transaction.commit(ACTIVE_SIGNAL)).rejects.toThrow('version became obsolete')
    await expect(begun.transaction.abort(new Error('commit rejected')))
      .resolves.toBe('FileIsolated')
    expect(tree.has(file.path)).toBe(false)
    let stagedFiles = 0
    for await (const staged of inner.stagedCatalog().files()) {
      expect(staged.record.canonicalPath).toEqual(file.path)
      stagedFiles += 1
    }
    expect(stagedFiles).toBe(0)
    await admission.release()
  })

  it('cannot revoke a file after the inner publication boundary', async () => {
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const tree = new MemoryOutputTree()
    const postPublicationFailure = new Error('authority changed after file publication')
    let publicationObserved = false
    const journal = new PublicationHookJournal(() => {
      publicationObserved = true
      authority.failNextUpdate(postPublicationFailure)
    })
    const inner = await PersistentTreeOutputSession.open({
      identity: IDENTITY,
      tree,
      journal,
    })
    const output = new OriginPrivateOutputSession(
      inner,
      new ControlledExporter(false),
      admission,
      async () => {},
      false,
    )
    const file = {
      source: {
        shareInstance: testOutputIdentity('published-share'),
        fileId: testOutputIdentity('published-file'),
        fileRevision: testOutputIdentity('published-revision'),
      },
      path: ['published.bin'],
      exactSize: 1n,
    }
    const begun = await output.beginFile(await admittedOutputFile(output, file), ACTIVE_SIGNAL)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)

    await expect(begun.transaction.commit(ACTIVE_SIGNAL)).resolves.toBeUndefined()
    expect(publicationObserved).toBe(true)
    expect(tree.has(file.path)).toBe(true)
    expect(admission.snapshot().activeReservations).toBe(0)
    await admission.release()
  })

  it('rolls back quota reserved immediately before cancellation and permits a same-size retry', async () => {
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const output = new OriginPrivateOutputSession(
      await openInner(),
      new ControlledExporter(false),
      admission,
      async () => {},
      false,
    )
    const file = await admittedOutputFile(output, lifecycleFile('reserve-cancelled.bin'))
    const controller = new AbortController()
    const cancelled = new DOMException('cancel after quota reservation', 'AbortError')
    authority.afterUpdates(1, () => controller.abort(cancelled))

    await expect(output.beginFile(file, controller.signal)).rejects.toBe(cancelled)
    expect(admission.snapshot().activeReservations).toBe(0)

    const retry = await output.beginFile(file, ACTIVE_SIGNAL)
    await expect(retry.transaction.abort(new Error('release retry'))).resolves.toBe('FileIsolated')
    expect(admission.snapshot().activeReservations).toBe(0)
    await admission.release()
  })

  it('rolls back prepared commit admission when cancellation wins before inner publication', async () => {
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const output = new OriginPrivateOutputSession(
      await openInner(),
      new ControlledExporter(false),
      admission,
      async () => {},
      false,
    )
    const file = await admittedOutputFile(output, {
      ...lifecycleFile('commit-cancelled.bin'),
      exactSize: 2n,
    })
    const begun = await output.beginFile(file, ACTIVE_SIGNAL)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
    const controller = new AbortController()
    const cancelled = new DOMException('cancel after commit admission', 'AbortError')
    authority.afterUpdates(2, () => controller.abort(cancelled))

    await expect(begun.transaction.commit(controller.signal)).rejects.toBe(cancelled)
    expect(admission.snapshot().activeReservations).toBe(1)
    await expect(begun.transaction.abort(new Error('rollback cancelled commit'))).resolves.toBe('FileIsolated')
    expect(admission.snapshot().activeReservations).toBe(0)
    await admission.release()
  })

  it('attempts and preserves both inner abort and quota-release failures', async () => {
    const authority = new MemoryAdmissionAuthority()
    const admission = await openAdmission(authority)
    const tree = new MemoryOutputTree()
    const inner = await PersistentTreeOutputSession.open({
      identity: IDENTITY,
      tree,
      journal: new MemoryOutputJournal(),
    })
    const output = new OriginPrivateOutputSession(
      inner,
      new ControlledExporter(false),
      admission,
      async () => {},
      false,
    )
    const file = await admittedOutputFile(output, lifecycleFile('dual-abort-failure.bin'))
    const begun = await output.beginFile(file, ACTIVE_SIGNAL)
    const removalFailure = new Error('inner file removal failed')
    const releaseFailure = new Error('quota release failed')
    tree.removeFileError = removalFailure
    authority.failNextUpdate(releaseFailure)

    const failure = await begun.transaction.abort(new Error('transfer failed'))
      .then(() => undefined, (error: unknown) => error)

    expect(failure).toBeInstanceOf(AggregateError)
    const evidence = (failure as AggregateError).errors
    expect(evidence[0]).toBeInstanceOf(AggregateError)
    expect((evidence[0] as AggregateError).errors).toContain(removalFailure)
    expect(evidence[1]).toBe(releaseFailure)
    expect(tree.has(file.path)).toBe(true)
    expect(admission.snapshot().activeReservations).toBe(1)
    await admission.release()
  })
})

class ControlledExporter implements OriginPrivateOutputExporter {
  readonly started = deferred<void>()
  readonly abortReasons: unknown[] = []
  readonly #publication = deferred<void>()
  readonly #ignoreAbort: boolean
  catalog: StagedOutputCatalog | undefined
  outcome: JobOutcome | undefined

  constructor(ignoreAbort: boolean) {
    this.#ignoreAbort = ignoreAbort
  }

  async export(
    catalog: StagedOutputCatalog,
    outcome: JobOutcome,
    signal: AbortSignal,
  ): Promise<OriginPrivateExportResult> {
    this.catalog = catalog
    this.outcome = outcome
    this.started.resolve()
    if (this.#ignoreAbort) {
      await this.#publication.promise
      return ORIGIN_PRIVATE_EXPORT_COMPLETE
    }
    signal.throwIfAborted()
    const blocked = deferred<void>()
    signal.addEventListener('abort', () => blocked.reject(signal.reason), { once: true })
    await blocked.promise
    return ORIGIN_PRIVATE_EXPORT_COMPLETE
  }

  async abort(reason: unknown): Promise<void> {
    this.abortReasons.push(reason)
  }

  releasePublication(): void {
    this.#publication.resolve()
  }
}

async function lifecycleFixture(
  exporter: OriginPrivateOutputExporter,
  retainAfterExport: boolean,
): Promise<{
  readonly output: OriginPrivateOutputSession
  readonly settlements: boolean[]
}> {
  const admission = await openAdmission(new MemoryAdmissionAuthority())
  const settlements: boolean[] = []
  const output = new OriginPrivateOutputSession(
    await openInner(),
    exporter,
    admission,
    async (removeStaging) => {
      settlements.push(removeStaging)
      await admission.release()
    },
    retainAfterExport,
  )
  return { output, settlements }
}

async function openInner(): Promise<PersistentTreeOutputSession> {
  return PersistentTreeOutputSession.open({
    identity: IDENTITY,
    tree: new MemoryOutputTree(),
    journal: new MemoryOutputJournal(),
  })
}

async function openAdmission(
  authority: OriginPrivateAdmissionAuthority,
): Promise<OriginPrivateStagingAdmission> {
  return OriginPrivateStagingAdmission.open(
    'origin-private-staging\0lifecycle-test',
    { logicalBytes: 0n, additionalBytes: 0n },
    {
      authority,
      estimate: async () => ({ quota: 2 * 1024 * 1024 * 1024, usage: 0 }),
    },
  )
}

class MemoryAdmissionAuthority implements OriginPrivateAdmissionAuthority {
  #record: AdmissionLeaseRecord | undefined
  #nextUpdateFailure: unknown
  #updatesUntilHook = 0
  #updateHook: (() => void) | undefined

  async claim(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    this.#validate(record, limits)
    this.#record = record
  }

  async update(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    if (this.#nextUpdateFailure !== undefined) {
      const failure = this.#nextUpdateFailure
      this.#nextUpdateFailure = undefined
      throw failure
    }
    this.#validate(record, limits)
    this.#record = record
    this.#runUpdateHook()
  }

  failNextUpdate(reason: unknown): void {
    this.#nextUpdateFailure = reason
  }

  afterUpdates(count: number, hook: () => void): void {
    this.#updatesUntilHook = count
    this.#updateHook = hook
  }

  async heartbeat(
    id: string,
    token: string,
    expiresAtMilliseconds: number,
    nowMilliseconds: number,
  ): Promise<void> {
    if (this.#record?.id !== id || this.#record.token !== token ||
        expiresAtMilliseconds <= nowMilliseconds) throw new Error('lease changed')
    this.#record = { ...this.#record, expiresAtMilliseconds }
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#record?.id === id && this.#record.token === token) this.#record = undefined
  }

  close(): void {}

  #runUpdateHook(): void {
    if (this.#updateHook === undefined) return
    this.#updatesUntilHook -= 1
    if (this.#updatesUntilHook > 0) return
    const hook = this.#updateHook
    this.#updateHook = undefined
    hook()
  }

  #validate(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): void {
    if (record.logicalBytes > limits.jobLimit || record.logicalBytes > limits.processLimit ||
        limits.usage + record.additionalBytes + limits.reserve > limits.quota) {
      throw new DOMException('quota exceeded', 'QuotaExceededError')
    }
  }
}

function lifecycleFile(name: string) {
  return {
    source: {
      shareInstance: testOutputIdentity(`share:${name}`),
      fileId: testOutputIdentity(`file:${name}`),
      fileRevision: testOutputIdentity(`revision:${name}`),
    },
    path: [name],
    exactSize: 1n,
  }
}

class PublicationHookJournal extends MemoryOutputJournal {
  readonly #published: () => void

  constructor(published: () => void) {
    super()
    this.#published = published
  }

  override async commitCandidate(key: string): Promise<void> {
    await super.commitCandidate(key)
    const record = await super.readCommitted(key)
    if (record?.kind === 'file' && record.committed) this.#published()
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
