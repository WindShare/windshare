import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { ByteRangeSet, byteRange, type ByteRange } from '../../src/content/geometry'
import { V2BlockLaneAttemptsError, type V2BlockRangeReader } from '../../src/content/v2-broker'
import {
  V2BlockOperationError,
  V2RemoteRevisionError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
  type V2OpenedRevision,
  type V2RevisionReader,
} from '../../src/content/v2-session-services'
import {
  SINGLE_FILE_STREAM_BACKEND,
  SingleFileStreamOutputSession,
} from '../../src/output/streams/single-file'
import { ZIP_STREAM_BACKEND, ZipStreamOutputSession } from '../../src/output/streams/zip'
import type {
  ZipArchiveFileEntry,
  ZipArchiveMember,
  ZipArchiveWriter,
} from '../../src/output/streams/zip-archive'
import {
  COMPLETED_JOB_SETTLEMENT,
  OutputBudgetExceededError,
  VerifiedDurableRanges,
  pausedJobSettlement,
  type BeginOutputFileResult,
  type OutputFile,
  type OutputFileOwnership,
  type OutputSession,
} from '../../src/transfer/output-session'
import {
  BoundaryFaultError,
  FaultDomain,
  FaultRetirement,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
  SourceFaultCode,
  type FileRetirementAuthorization,
} from '../../src/transfer/fault'
import {
  TransferJob,
  V2OutputSettlementTimeoutError,
  V2RangeReaderContractError,
  type TransferProgress,
} from '../../src/transfer/v2-job'
import {
  committedDirectoryFor,
  BIND_TEST_DIRECTORY_ADMISSION_SCOPE,
  fileEntry,
  identity,
  identityText,
  openedRevision,
  outputAuthority,
  testDirectoryAdmissionLedgerBinding,
  traversalPage,
  type ScopeBoundTestOutputSession,
  type TestOutputSessionFactory,
  type TestOutputSessionSource,
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
    const session: TestOutputSessionFactory = {
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
      open: (scope) => new SingleFileStreamOutputSession(identityText(31), scope, output),
    }

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
    const session = zipOutputFactory(identityText(32), archive)

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

  it('pauses on an unknown ZIP begin rejection without granting retirement authority', async () => {
    const archive = new RecordingArchive()
    const session: TestOutputSessionFactory = {
      backend: ZIP_STREAM_BACKEND,
      format: 'zip',
      open: (scope) => {
        const base = new ZipStreamOutputSession({
          outputSessionId: identityText(33),
          directoryAdmissionScope: scope,
          archive,
        })
        return delegateOutput(base, async (file, signal) => {
          if (file.path.at(-1) === 'bad.bin') throw new Error('member rejected before acquisition')
          return base.beginFile(file, signal)
        })
      },
    }

    const result = await runJob([
      fileEntry(identity(11), 'bad.bin', 1n),
      fileEntry(identity(12), 'good.bin', 1n),
    ], session, contiguousBroker(Uint8Array.of(7)), 1n)

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Output,
        scope: FaultScope.OutputPause,
        code: OutputFaultCode.StateIO,
      },
    })
    expect(archive.aborted).toBe(true)
    expect(archive.files).toEqual([])
  })

  it('pauses a real ZIP session when a later block fails after member emission starts', async () => {
    const archive = new RecordingArchive()
    const session = zipOutputFactory(identityText(34), archive)
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
    expect(result.settlement.kind).toBe('Paused')
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
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Session,
        scope: FaultScope.OutputPause,
        code: SessionFaultCode.DependencyContract,
      },
      cause: expect.any(V2RangeReaderContractError),
    })
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
      expect(result.abortReason).toMatchObject({
        fault: {
          domain: FaultDomain.Output,
          scope: FaultScope.OutputPause,
          code: OutputFaultCode.ResourceBudget,
        },
        cause: budget,
      })
      expect(output.suspendReasons).toEqual([result.abortReason])
    },
  )

  it('preserves a verified prefix and pauses after a protocol-level block failure', async () => {
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

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Session,
        scope: FaultScope.SessionTerminal,
        code: SessionFaultCode.Protocol,
      },
    })
    expect(output.checkpoints).toEqual([[byteRange(0n, 1n)]])
    expect(output.retirementReasons).toEqual([])
    expect(progress.at(-1)).toMatchObject({ writtenBytes: 1n, completedBytes: 0n, completedFiles: 0, fileErrors: 0 })
  })

  it.each([
    [
      'transient all-lane outage',
      () => new V2BlockLaneAttemptsError([new Error('relay and peer lanes changed')]),
      SessionFaultCode.Transport,
    ],
    [
      'unknown collaborator failure',
      () => new Error('untyped broker failure'),
      SessionFaultCode.DependencyContract,
    ],
    [
      'unknown wrapper around a permanent source cause',
      () => new Error('untyped broker wrapper', {
        cause: new V2RemoteRevisionError({ code: 0x3002, retryable: false }),
      }),
      SessionFaultCode.DependencyContract,
    ],
  ] as const)('keeps durable ranges on %s', async (_name, failure, code) => {
    const output = durableOutput([])
    const broker = {
      readRange: async function* (_descriptor: unknown, _lease: Uint8Array, range: ByteRange) {
        if (range.start === 0n) yield { offset: 0n, data: Uint8Array.of(1) }
        else throw failure()
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(
      [fileEntry(identity(11), 'preserved.bin', 2n)],
      output.session,
      broker,
      1n,
    )

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Session,
        scope: FaultScope.OutputPause,
        code,
      },
    })
    expect(output.checkpoints).toEqual([[byteRange(0n, 1n)]])
    expect(output.retirementReasons).toEqual([])
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
})

describe('v2 file transfer fault retirement authority', () => {
  it('retires only an explicitly permanent source failure and continues its sibling', async () => {
    const entries = [
      fileEntry(identity(11), 'permanent.bin', 1n),
      fileEntry(identity(12), 'good.bin', 1n),
    ]
    const output = durableOutput([])
    const broker = {
      readRange: async function* (
        descriptor: { readonly fileId: Uint8Array },
        _lease: Uint8Array,
        range: ByteRange,
      ) {
        if (descriptor.fileId[0] === entries[0]?.id[0]) {
          throw new V2RemoteRevisionError({ code: 0x3002, retryable: false })
        }
        yield { offset: range.start, data: Uint8Array.of(9) }
      },
    } as unknown as V2BlockRangeReader

    const result = await runJob(entries, output.session, broker, 1n)

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect([...output.begunPaths].sort()).toEqual(['good.bin', 'permanent.bin'])
    expect(output.retirementReasons).toHaveLength(1)
    expect((output.retirementReasons[0] as FileRetirementAuthorization).retirement)
      .toBe(FaultRetirement.PermanentSource)
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
    expect(result.outcome.failures[0]?.reason).toMatchObject({
      fault: {
        domain: FaultDomain.Source,
        scope: FaultScope.FileLocal,
        code: SourceFaultCode.Unavailable,
      },
      cause: releaseFailure,
    })
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
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Session,
        scope: FaultScope.SessionTerminal,
        code: SessionFaultCode.Protocol,
      },
    })
    const evidence = (result.abortReason as BoundaryFaultError).cause
    expect(evidence).toBeInstanceOf(AggregateError)
    expect((evidence as AggregateError).errors).toEqual([
      expect.objectContaining({ cause: primaryFailure }),
      expect.objectContaining({ cause: releaseFailure }),
    ])
    expect(output.begunPaths).toEqual(['bad.bin'])
  })

  it.each(['foreign-binding', 'transient-ranges'] as const)(
    'pauses a %s BeginFile contract failure without invoking raw retirement',
    async (failure) => {
      const output = bindingFailureOutput(failure)
      const result = await runJob([
        fileEntry(identity(11), 'bad.bin', 1n),
        fileEntry(identity(12), 'must-not-start.bin', 1n),
      ], output.session, contiguousBroker(Uint8Array.of(1)), 1n)

      expect(result.outcome.status).toBe('Paused')
      expect(result.abortReason).toMatchObject({
        fault: {
          domain: FaultDomain.Output,
          scope: FaultScope.OutputPause,
          code: OutputFaultCode.Contract,
        },
      })
      expect(output.begunPaths).toEqual(['bad.bin'])
      expect(output.events).toEqual(['paused'])
    },
  )

  it('keeps fragmented-read cancellation distinct from a concurrent lease settlement fault', async () => {
    const controller = new AbortController()
    const cancelled = new DOMException('cancel fragmented read', 'AbortError')
    const entries = [fileEntry(identity(11), 'cancel.bin', 4_096n)]
    const releaseFailure = new Error('concurrent lease release failure')
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
      entries,
      output.session,
      broker,
      4_096n,
      controller.signal,
      undefined,
      { revisions: revisionReader(entries, 4_096n, async () => { throw releaseFailure }) },
    )

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBe(cancelled)
    expect(yielded).toBe(2)
    expect(output.writes).toEqual([])
    expect(output.retirementReasons).toEqual([])
  })

  it('bounds an authorized retirement that never returns and preserves job-wide ambiguity', async () => {
    const output = durableOutput([], undefined, async () => new Promise(() => undefined))
    const broker = {
      readRange: async function* () {
        await Promise.reject(new V2RevisionChangedDuringRecoveryError())
        yield { offset: 0n, data: Uint8Array.of(0) }
      },
    } as unknown as V2BlockRangeReader
    const result = await withTimeout(runJob([
      fileEntry(identity(11), 'bad.bin', 1n),
      fileEntry(identity(12), 'must-not-start.bin', 1n),
    ], output.session, broker, 1n, undefined, 10), 500,
    'authorized transaction retirement exceeded its settlement deadline')

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Output,
        scope: FaultScope.OutputPause,
        code: OutputFaultCode.MutationAmbiguous,
      },
    })
    const evidence = (result.abortReason as BoundaryFaultError).cause
    expect(evidence).toBeInstanceOf(AggregateError)
    expect((evidence as AggregateError).errors).toEqual([
      expect.objectContaining({
        fault: {
          domain: FaultDomain.Source,
          scope: FaultScope.FileLocal,
          code: SourceFaultCode.RevisionInvalidated,
        },
      }),
      expect.any(V2OutputSettlementTimeoutError),
    ])
    expect(output.begunPaths).toEqual(['bad.bin'])
    expect(output.retirementReasons).toHaveLength(1)
    expect((output.retirementReasons[0] as FileRetirementAuthorization).retirement)
      .toBe(FaultRetirement.InvalidatedRevision)
  })
})

async function runJob(
  entries: readonly Extract<V2CatalogEntry, { kind: 'file' }>[],
  output: TestOutputSessionSource,
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

function zipOutputFactory(
  outputSessionId: string,
  archive: ZipArchiveWriter,
): TestOutputSessionFactory {
  return {
    backend: ZIP_STREAM_BACKEND,
    format: 'zip',
    open: (scope) => new ZipStreamOutputSession({
      outputSessionId,
      directoryAdmissionScope: scope,
      archive,
    }),
  }
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
    completeJob: (outcome, signal) => base.completeJob(outcome, signal),
    pauseJob: (reason) => base.pauseJob(reason),
  }
}

function durableOutput(
  initialRanges: readonly ByteRange[],
  fault?: { readonly stage: 'begin' | 'write' | 'checkpoint'; readonly error: Error },
  retirement?: (reason: unknown) => Promise<'FileIsolated' | 'JobOutputCompromised'>,
): {
  readonly session: OutputSession
  readonly writes: Array<{ readonly offset: bigint; readonly data: Uint8Array<ArrayBuffer> }>
  readonly suspendReasons: unknown[]
  readonly begunPaths: string[]
  readonly retirementReasons: unknown[]
  readonly checkpoints: readonly (readonly ByteRange[])[]
} {
  const identity = Object.freeze({ backend: 'test-durable', outputSessionId: identityText(41) })
  const admissions = testDirectoryAdmissionLedgerBinding()
  const writes: Array<{ offset: bigint; data: Uint8Array<ArrayBuffer> }> = []
  const suspendReasons: unknown[] = []
  const begunPaths: string[] = []
  const retirementReasons: unknown[] = []
  const checkpoints: Array<readonly ByteRange[]> = []
  const session: ScopeBoundTestOutputSession = {
    [BIND_TEST_DIRECTORY_ADMISSION_SCOPE]: admissions.bind,
    identity,
    format: 'directory',
    capabilities: {
      durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false,
    },
    admitDirectory: (directory, signal) => admissions.get().admitDirectory(directory, signal),
    finalizeDirectory: (admission, signal) => admissions.get().finalizeDirectory(admission, signal),
    beginFile: async (input) => {
      if (fault?.stage === 'begin') throw fault.error
      const file = admissions.get().validateFileParent(input)
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
            checkpoints.push(durable.ranges)
            return ranges(ownership, file, durable)
          },
          commit: vi.fn(async () => undefined),
          retire: vi.fn(async (reason: unknown) => {
            retirementReasons.push(reason)
            return retirement?.(reason) ?? 'FileIsolated' as const
          }),
          pause: vi.fn(async () => undefined),
        }),
      })
    },
    completeJob: async () => COMPLETED_JOB_SETTLEMENT,
    pauseJob: async (reason) => {
      suspendReasons.push(reason)
      return pausedJobSettlement('ProcessRestart')
    },
  }
  return { session, writes, suspendReasons, begunPaths, retirementReasons, checkpoints }
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
} {
  const identity = Object.freeze({ backend: 'binding-failure', outputSessionId: identityText(51) })
  const admissions = testDirectoryAdmissionLedgerBinding()
  const begunPaths: string[] = []
  const events: string[] = []
  const session: ScopeBoundTestOutputSession = {
    [BIND_TEST_DIRECTORY_ADMISSION_SCOPE]: admissions.bind,
    identity,
    format: 'directory',
    capabilities: failure === 'foreign-binding'
      ? { durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false }
      : { durability: 'None', randomWrite: false, fileFailureIsolation: true, modificationTime: false },
    admitDirectory: (directory, signal) => admissions.get().admitDirectory(directory, signal),
    finalizeDirectory: (admission, signal) => admissions.get().finalizeDirectory(admission, signal),
    beginFile: async (input) => {
      const file = admissions.get().validateFileParent(input)
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
          retire: async () => {
            events.push('retirement-called')
            return 'FileIsolated' as const
          },
          pause: async () => undefined,
        }),
      })
    },
    completeJob: async () => COMPLETED_JOB_SETTLEMENT,
    pauseJob: async () => {
      events.push('paused')
      return pausedJobSettlement(session.capabilities.durability)
    },
  }
  return {
    session,
    begunPaths,
    events,
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
