import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { ByteRangeSet, byteRange, type ByteRange } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import {
  V2BlockOperationError,
  V2RevisionLeaseExpiredError,
  type V2OpenedRevision,
  type V2RevisionReader,
} from '../../src/content/v2-session-services'
import { SingleFileStreamOutputSession } from '../../src/output/streams/single-file'
import { ZipStreamOutputSession } from '../../src/output/streams/zip'
import type {
  ZipArchiveFileEntry,
  ZipArchiveMember,
  ZipArchiveWriter,
} from '../../src/output/streams/zip-archive'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import { OutputTransactionContractError } from '../../src/transfer/output-file-transaction'
import {
  OutputBudgetExceededError,
  VerifiedDurableRanges,
  type BeginOutputFileResult,
  type OutputFile,
  type OutputFileOwnership,
  type OutputSession,
} from '../../src/transfer/output-session'
import {
  TransferJob,
  V2OutputPausedError,
  V2OutputSettlementTimeoutError,
  V2RangeReaderContractError,
  type TransferProgress,
} from '../../src/transfer/v2-job'
import { V2FileLeaseSettlementError } from '../../src/transfer/v2-job-failures'
import {
  committedDirectoryFor,
  fileEntry,
  identity,
  identityText,
  openedRevision,
  outputAuthority,
  traversalPage,
  withTimeout,
} from './v2-job-fixture'

describe('v2 file transfer output semantics', () => {
  it('runs a real single-file transient stream without claiming durable ranges', async () => {
    const bytes: number[] = []
    let closed = false
    const output = new WritableStream<Uint8Array>({
      write: (chunk) => { bytes.push(...chunk) },
      close: () => { closed = true },
    })
    const session = new SingleFileStreamOutputSession(identityText(31), output)

    const result = await runJob(
      [fileEntry(identity(11), 'single.bin', 3n)],
      session,
      contiguousBroker(Uint8Array.of(1, 2, 3)),
      2n,
    )

    expect(result.outcome.failures).toEqual([])
    expect(result.outcome.status).toBe('Succeeded')
    expect(bytes).toEqual([1, 2, 3])
    expect(closed).toBe(true)
  })

  it('runs a real ZIP transient session through member commit and archive publication', async () => {
    const archive = new RecordingArchive()
    const session = new ZipStreamOutputSession({ outputSessionId: identityText(32), archive })

    const result = await runJob(
      [fileEntry(identity(11), 'archive.bin', 3n)],
      session,
      contiguousBroker(Uint8Array.of(4, 5, 6)),
      2n,
    )

    expect(result.outcome.status).toBe('Succeeded')
    expect(archive.closed).toBe(true)
    expect(archive.files).toEqual([{ path: ['archive.bin'], bytes: [4, 5, 6] }])
  })

  it('isolates a ZIP begin rejection before member ownership and transfers the next file', async () => {
    const archive = new RecordingArchive()
    const base = new ZipStreamOutputSession({ outputSessionId: identityText(33), archive })
    const session = delegateOutput(base, async (file, signal) => {
      if (file.path.at(-1) === 'bad.bin') throw new Error('member rejected before acquisition')
      return base.beginFile(file, signal)
    })

    const result = await runJob([
      fileEntry(identity(11), 'bad.bin', 1n),
      fileEntry(identity(12), 'good.bin', 1n),
    ], session, contiguousBroker(Uint8Array.of(7)), 1n)

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(archive.aborted).toBe(false)
    expect(archive.files).toEqual([{ path: ['good.bin'], bytes: [7] }])
  })

  it('pauses a real ZIP session when a later block fails after member emission starts', async () => {
    const archive = new RecordingArchive()
    const session = new ZipStreamOutputSession({ outputSessionId: identityText(34), archive })
    const failure = new V2BlockOperationError('object-auth', 'later block failed', {
      cause: new Error('bad block'),
    })
    const broker = {
      readRange: async function* (
        _descriptor: unknown,
        _lease: Uint8Array,
        range: ByteRange,
      ) {
        if (range.start === 0n) yield { offset: 0n, data: Uint8Array.of(1) }
        else throw failure
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'started.bin', 2n)],
      session,
      broker,
      1n,
    )

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe('Discarded')
    expect(archive.aborted).toBe(true)
    expect(archive.closed).toBe(false)
  })

  it.each([
    ['durable-prefix escape', async function* () {
      yield { offset: 1n, data: Uint8Array.of(9, 9) }
    }],
    ['overlap', async function* () {
      yield { offset: 2n, data: Uint8Array.of(9) }
      yield { offset: 2n, data: Uint8Array.of(9) }
    }],
    ['gap', async function* () {
      yield { offset: 3n, data: Uint8Array.of(9) }
    }],
    ['early end', async function* () {
      yield { offset: 2n, data: Uint8Array.of(9) }
    }],
  ] as const)('atomically rejects %s from a resumed range reader', async (_name, slices) => {
    const output = durableOutput([byteRange(0n, 2n)])
    const broker = {
      readRange: () => slices(),
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'resume.bin', 4n)],
      output.session,
      broker,
      4n,
    )

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBeInstanceOf(V2RangeReaderContractError)
    expect(output.writes).toEqual([])
    expect(output.suspendReasons).toHaveLength(1)
  })

  it('accepts a high-fragment contiguous reader with constant verifier state and one atomic write', async () => {
    const size = 32_768n
    const output = durableOutput([])
    const broker = {
      readRange: async function* (_descriptor: unknown, _lease: Uint8Array, range: ByteRange) {
        for (let offset = range.start; offset < range.end; offset += 1n) {
          yield { offset, data: Uint8Array.of(Number(offset & 0xffn)) }
        }
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'fragmented.bin', size)],
      output.session,
      broker,
      size,
    )

    expect(result.outcome.status).toBe('Succeeded')
    expect(output.writes).toHaveLength(1)
    expect(output.writes[0]).toMatchObject({ offset: 0n })
    expect(output.writes[0]?.data).toHaveLength(Number(size))
  })

  it.each(['begin', 'write', 'checkpoint'] as const)(
    'pauses when the output %s operation exhausts a policy budget',
    async (stage) => {
      const budget = new OutputBudgetExceededError(`${stage}-budget`, 1n, 2n)
      const output = durableOutput([], { stage, error: budget })

      const result = await runJob(
        [fileEntry(identity(11), 'budget.bin', 1n)],
        output.session,
        contiguousBroker(Uint8Array.of(1)),
        1n,
      )

      expect(result.outcome.status).toBe('Paused')
      expect(result.abortReason).toBe(budget)
      expect(output.suspendReasons).toEqual([budget])
    },
  )

  it('keeps acknowledged bytes separate from committed bytes after a partial file failure', async () => {
    const progress: TransferProgress[] = []
    const output = durableOutput([])
    const failure = new V2BlockOperationError('object-auth', 'second block failed', {
      cause: new Error('bad block'),
    })
    const broker = {
      readRange: async function* (_descriptor: unknown, _lease: Uint8Array, range: ByteRange) {
        if (range.start === 0n) yield { offset: 0n, data: Uint8Array.of(1) }
        else throw failure
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'partial.bin', 2n)],
      output.session,
      broker,
      1n,
      undefined,
      undefined,
      { onProgress: (value) => progress.push(value) },
    )

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 1n, completedBytes: 0n, completedFiles: 0, fileErrors: 1 })
  })

  it('counts the exact logical size after a resumed file commits', async () => {
    const progress: TransferProgress[] = []
    const output = durableOutput([byteRange(0n, 2n)])

    const result = await runJob(
      [fileEntry(identity(11), 'resume.bin', 4n)],
      output.session,
      contiguousBroker(Uint8Array.of(1, 2, 3, 4)),
      4n,
      undefined,
      undefined,
      { onProgress: (value) => progress.push(value) },
    )

    expect(result.outcome.status).toBe('Succeeded')
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 2n, completedBytes: 4n, completedFiles: 1, fileErrors: 0 })
  })

  it('releases a raw mismatched revision exactly once and continues with its sibling', async () => {
    const entries = [
      fileEntry(identity(11), 'changed.bin', 1n),
      fileEntry(identity(12), 'good.bin', 1n),
    ]
    const releases = [vi.fn(async () => undefined), vi.fn(async () => undefined)]
    const revisions: V2RevisionReader = {
      open: async (id) => {
        const index = id[0] === entries[0]?.id[0] ? 0 : 1
        const release = releases[index]
        if (release === undefined) throw new Error('test release spy is unavailable')
        const opened = openedRevision(index === 0 ? identity(99) : Uint8Array.from(id), 1n, 1n)
        return Object.freeze({ ...opened, release })
      },
    }
    const output = durableOutput([])

    const result = await runJob(
      entries,
      output.session,
      contiguousBroker(Uint8Array.of(1)),
      1n,
      undefined,
      undefined,
      { revisions },
    )

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(releases[0]).toHaveBeenCalledOnce()
    expect(releases[1]).toHaveBeenCalledOnce()
    expect(output.begunPaths).toEqual(['good.bin'])
  })

  it('records a revision-scoped release failure after commit without losing completion', async () => {
    const entries = [
      fileEntry(identity(11), 'released-late.bin', 1n),
      fileEntry(identity(12), 'good.bin', 1n),
    ]
    const progress: TransferProgress[] = []
    const releaseFailure = new V2RevisionLeaseExpiredError()
    const revisions = revisionReader(entries, 1n, async (index) => {
      if (index === 0) throw releaseFailure
    })
    const output = durableOutput([])

    const result = await runJob(
      entries,
      output.session,
      contiguousBroker(Uint8Array.of(1)),
      1n,
      undefined,
      undefined,
      { revisions, onProgress: (value) => progress.push(value) },
    )

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failures[0]?.reason).toBeInstanceOf(V2FileLeaseSettlementError)
    expect(progress.at(-1)).toMatchObject({ completedFiles: 2, completedBytes: 2n, fileErrors: 1 })
    expect([...output.begunPaths].sort()).toEqual(['good.bin', 'released-late.bin'])
  })

  it('preserves both a file failure and a terminal lease-release failure before pausing siblings', async () => {
    const entries = [
      fileEntry(identity(11), 'bad.bin', 1n),
      fileEntry(identity(12), 'must-not-start.bin', 1n),
    ]
    const primaryFailure = new V2BlockOperationError('object-auth', 'block failed', {
      cause: new Error('bad block'),
    })
    const releaseFailure = new Error('lease release transport failed')
    const revisions = revisionReader(entries, 1n, async (index) => {
      if (index === 0) throw releaseFailure
    })
    const output = durableOutput([])
    const broker = {
      readRange: async function* () {
        await Promise.reject(primaryFailure)
        yield { offset: 0n, data: Uint8Array.of(0) }
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(entries, output.session, broker, 1n, undefined, undefined, { revisions })

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBeInstanceOf(V2OutputPausedError)
    const evidence = (result.abortReason as Error).cause
    expect(evidence).toBeInstanceOf(AggregateError)
    expect((evidence as AggregateError).errors).toEqual([primaryFailure, releaseFailure])
    expect(output.begunPaths).toEqual(['bad.bin'])
  })

  it.each(['foreign-binding', 'transient-ranges'] as const)(
    'settles the raw transaction before pausing a %s BeginFile contract failure',
    async (failure) => {
      const output = bindingFailureOutput(failure)
      const job = runJob([
        fileEntry(identity(11), 'bad.bin', 1n),
        fileEntry(identity(12), 'must-not-start.bin', 1n),
      ], output.session, contiguousBroker(Uint8Array.of(1)), 1n)

      await output.abortStarted
      expect(output.begunPaths).toEqual(['bad.bin'])
      expect(output.events).toEqual(['abort-started'])
      output.releaseAbort()
      const result = await job

      expect(result.outcome.status).toBe('Paused')
      expect(result.abortReason).toBeInstanceOf(OutputTransactionContractError)
      expect(output.begunPaths).toEqual(['bad.bin'])
      expect(output.events).toEqual(['abort-started', 'abort-finished', 'suspended'])
    },
  )

  it('checks cancellation between fragmented range slices before copying or writing more bytes', async () => {
    const controller = new AbortController()
    const cancelled = new DOMException('cancel fragmented read', 'AbortError')
    const output = durableOutput([])
    let yielded = 0
    const broker = {
      readRange: async function* (_descriptor: unknown, _lease: Uint8Array, range: ByteRange) {
        yielded += 1
        yield { offset: range.start, data: Uint8Array.of(1) }
        controller.abort(cancelled)
        for (let offset = range.start + 1n; offset < range.end; offset += 1n) {
          yielded += 1
          yield { offset, data: Uint8Array.of(1) }
        }
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'cancel.bin', 4_096n)],
      output.session,
      broker,
      4_096n,
      controller.signal,
    )

    expect(result.outcome.status).toBe('Aborted')
    expect(result.abortReason).toBe(cancelled)
    expect(yielded).toBe(2)
    expect(output.writes).toEqual([])
  })

  it('bounds a raw transaction abort that never returns and prevents sibling continuation', async () => {
    const output = bindingFailureOutput('foreign-binding')
    const result = await withTimeout(runJob([
      fileEntry(identity(11), 'bad.bin', 1n),
      fileEntry(identity(12), 'must-not-start.bin', 1n),
    ], output.session, contiguousBroker(Uint8Array.of(1)), 1n, undefined, 10), 500,
    'raw transaction abort exceeded its settlement deadline')

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBeInstanceOf(V2OutputPausedError)
    const evidence = (result.abortReason as Error).cause
    expect(evidence).toBeInstanceOf(AggregateError)
    expect((evidence as AggregateError).errors).toEqual([
      expect.any(OutputTransactionContractError),
      expect.any(V2OutputSettlementTimeoutError),
    ])
    expect(output.begunPaths).toEqual(['bad.bin'])
    expect(output.events).toEqual(['abort-started', 'suspended'])
  })
})

async function runJob(
  entries: readonly Extract<V2CatalogEntry, { kind: 'file' }>[],
  output: OutputSession,
  broker: V2BlockRangeReader,
  blockSize: bigint,
  signal?: AbortSignal,
  outputSettlementTimeoutMilliseconds?: number,
  overrides: {
    readonly revisions?: V2RevisionReader
    readonly onProgress?: (progress: TransferProgress) => void
  } = {},
) {
  const root = identity(2)
  const catalog = {
    loadDirectory: async () => committedDirectoryFor(root, identityText(2), entries.length),
    pages: async function* () { yield traversalPage(root, entries) },
  } as unknown as V2CatalogClient
  return new TransferJob({
    descriptor: {
      shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2),
      chunkSize: Number(blockSize),
    } as never,
    catalog,
    selection: new V2SelectionPolicy(true),
    revisions: overrides.revisions ?? {
      open: async (id: Uint8Array<ArrayBuffer>) => {
        const entry = entries.find((candidate) => candidate.id[0] === id[0])
        if (entry === undefined) throw new Error('test revision identity was not catalogued')
        return openedRevision(id, entry.expectedSize, blockSize)
      },
    } as V2RevisionReader,
    broker,
    lanes: { size: 1 },
    output: outputAuthority(output),
    maximumConcurrentFiles: 1,
    ...(overrides.onProgress === undefined ? {} : { onProgress: overrides.onProgress }),
    ...(outputSettlementTimeoutMilliseconds === undefined ? {} : { outputSettlementTimeoutMilliseconds }),
  }).run(signal)
}

function revisionReader(
  entries: readonly Extract<V2CatalogEntry, { kind: 'file' }>[],
  blockSize: bigint,
  release: (index: number) => Promise<void>,
): V2RevisionReader {
  return {
    open: async (id) => {
      const index = entries.findIndex((entry) => entry.id[0] === id[0])
      if (index < 0) throw new Error('test revision identity was not catalogued')
      const opened = openedRevision(id as Uint8Array<ArrayBuffer>, entries[index]!.expectedSize, blockSize)
      return Object.freeze({ ...opened, release: () => release(index) }) as V2OpenedRevision
    },
  }
}

function contiguousBroker(bytes: Uint8Array): V2BlockRangeReader {
  return {
    readRange: async function* (_descriptor, _lease, range) {
      const start = Number(range.start)
      const end = Number(range.end)
      yield { offset: range.start, data: bytes.slice(start, end) }
    },
  }
}

function delegateOutput(
  base: OutputSession,
  beginFile: (file: OutputFile, signal: AbortSignal) => Promise<BeginOutputFileResult>,
): OutputSession {
  return {
    identity: base.identity,
    format: base.format,
    capabilities: base.capabilities,
    admitDirectory: (directory, signal) => base.admitDirectory(directory, signal),
    finalizeDirectory: (directory, signal) => base.finalizeDirectory(directory, signal),
    beginFile,
    finishJob: (outcome, signal) => base.finishJob(outcome, signal),
    abortJob: (reason) => base.abortJob(reason),
  }
}

function durableOutput(
  initialRanges: readonly ByteRange[],
  fault?: { readonly stage: 'begin' | 'write' | 'checkpoint'; readonly error: Error },
): {
  readonly session: OutputSession
  readonly writes: Array<{ readonly offset: bigint; readonly data: Uint8Array<ArrayBuffer> }>
  readonly suspendReasons: unknown[]
  readonly begunPaths: string[]
} {
  const identity = Object.freeze({ backend: 'test-durable', outputSessionId: identityText(41) })
  const admissions = new DirectoryAdmissionLedger()
  const writes: Array<{ offset: bigint; data: Uint8Array<ArrayBuffer> }> = []
  const suspendReasons: unknown[] = []
  const begunPaths: string[] = []
  const session: OutputSession = {
    identity,
    format: 'directory',
    capabilities: {
      durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false,
    },
    admitDirectory: (directory, signal) => admissions.admitDirectory(directory, signal),
    finalizeDirectory: async (directory) => { admissions.validateDirectoryFinalization(directory) },
    beginFile: async (input) => {
      if (fault?.stage === 'begin') throw fault.error
      const file = admissions.validateFileParent(input)
      begunPaths.push(file.path.at(-1) ?? '')
      const ownership: OutputFileOwnership = Object.freeze({
        ...identity,
        canonicalPath: file.path,
        ownedFileIdentity: `${identity.outputSessionId}:${file.path.join('/')}`,
      })
      let durable = new ByteRangeSet(file.exactSize, initialRanges)
      let pending = new ByteRangeSet(file.exactSize, [])
      return Object.freeze({
        durableRanges: ranges(ownership, file, durable),
        transaction: Object.freeze({
          writeRange: async (offset: bigint, data: Uint8Array) => {
            if (fault?.stage === 'write') throw fault.error
            const snapshot = Uint8Array.from(data)
            writes.push({ offset, data: snapshot })
            pending = pending.union(new ByteRangeSet(file.exactSize, [
              byteRange(offset, offset + BigInt(snapshot.byteLength)),
            ]))
          },
          checkpoint: async () => {
            if (fault?.stage === 'checkpoint') throw fault.error
            durable = durable.union(pending)
            pending = new ByteRangeSet(file.exactSize, [])
            return ranges(ownership, file, durable)
          },
          commit: vi.fn(async () => undefined),
          abort: vi.fn(async () => 'FileIsolated' as const),
        }),
      })
    },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
    suspendJob: async (reason) => { suspendReasons.push(reason) },
  }
  return { session, writes, suspendReasons, begunPaths }
}

function ranges(
  ownership: OutputFileOwnership,
  file: OutputFile,
  value: ByteRangeSet,
): VerifiedDurableRanges {
  return new VerifiedDurableRanges(ownership, file.source, file.exactSize, value.ranges)
}

function bindingFailureOutput(failure: 'foreign-binding' | 'transient-ranges'): {
  readonly session: OutputSession
  readonly begunPaths: string[]
  readonly events: string[]
  readonly abortStarted: Promise<void>
  readonly releaseAbort: () => void
} {
  const identity = Object.freeze({ backend: 'binding-failure', outputSessionId: identityText(51) })
  const admissions = new DirectoryAdmissionLedger()
  const begunPaths: string[] = []
  const events: string[] = []
  let signalAbortStarted: (() => void) | undefined
  const abortStarted = new Promise<void>((resolve) => { signalAbortStarted = resolve })
  let signalAbortRelease: (() => void) | undefined
  const abortRelease = new Promise<void>((resolve) => { signalAbortRelease = resolve })
  const session: OutputSession = {
    identity,
    format: 'directory',
    capabilities: failure === 'foreign-binding'
      ? { durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false }
      : { durability: 'None', randomWrite: false, fileFailureIsolation: true, modificationTime: false },
    admitDirectory: (directory, signal) => admissions.admitDirectory(directory, signal),
    finalizeDirectory: async (directory) => { admissions.validateDirectoryFinalization(directory) },
    beginFile: async (input) => {
      const file = admissions.validateFileParent(input)
      begunPaths.push(file.path.at(-1) ?? '')
      const ownership: OutputFileOwnership = Object.freeze({
        ...identity,
        canonicalPath: file.path,
        ownedFileIdentity: `${identity.outputSessionId}:${file.path.join('/')}`,
      })
      const rangesOwnership = failure === 'foreign-binding'
        ? Object.freeze({ ...ownership, outputSessionId: identityText(52) })
        : ownership
      return Object.freeze({
        durableRanges: new VerifiedDurableRanges(
          rangesOwnership,
          file.source,
          file.exactSize,
          failure === 'transient-ranges' ? [byteRange(0n, 1n)] : [],
        ),
        transaction: Object.freeze({
          writeRange: async () => undefined,
          checkpoint: async () => new VerifiedDurableRanges(ownership, file.source, file.exactSize, []),
          commit: async () => undefined,
          abort: async () => {
            events.push('abort-started')
            signalAbortStarted?.()
            await abortRelease
            events.push('abort-finished')
            return 'FileIsolated' as const
          },
        }),
      })
    },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
    suspendJob: async () => { events.push('suspended') },
  }
  return {
    session,
    begunPaths,
    events,
    abortStarted,
    releaseAbort: () => signalAbortRelease?.(),
  }
}

class RecordingArchive implements ZipArchiveWriter {
  readonly files: Array<{ readonly path: readonly string[]; readonly bytes: number[] }> = []
  readonly cleanupPending = false
  readonly cleanupFailure: unknown = undefined
  closed = false
  aborted = false

  async addDirectory(): Promise<void> {}

  async beginFile(entry: ZipArchiveFileEntry): Promise<ZipArchiveMember> {
    const file = { path: [...entry.path], bytes: [] as number[] }
    this.files.push(file)
    return {
      write: async (data) => { file.bytes.push(...data) },
      close: async () => undefined,
      abort: async () => undefined,
    }
  }

  async close(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.closed = true
  }

  async abort(): Promise<void> { this.aborted = true }
  async retryCleanup(): Promise<void> {}
}
