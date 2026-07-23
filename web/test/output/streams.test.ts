import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import { fileId } from '../../src/catalog/model'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  jobOutcome,
  summarizeTransferFailures,
} from '../../src/transfer/outcome'
import type { OutputFile } from '../../src/transfer/output-session'
import {
  createBoundedPortableDownloadStream,
  PORTABLE_DOWNLOAD_MAXIMUM_BYTES,
} from '../../src/output/portable/browser-download'
import { SingleFileStreamOutputSession } from '../../src/output/streams/single-file'
import type {
  ZipArchiveEntry,
  ZipArchiveFileEntry,
  ZipArchiveMember,
  ZipArchiveWriter,
} from '../../src/output/streams/zip-archive'
import { StreamingZipArchiveWriter } from '../../src/output/streams/streaming-zip'
import {
  ZipStreamOutputSession,
  type ZipCompletionReport,
} from '../../src/output/streams/zip'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'

const ACTIVE_SIGNAL = new AbortController().signal

describe('single-file stream output', () => {
  it('streams one file in ascending order and never advertises durable ranges', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('single', output.stream)
    const begun = await session.beginFile(outputFile('file', 3n))

    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2))
    expect((await begun.transaction.checkpoint()).ranges).toEqual([])
    await begun.transaction.writeRange(2n, Uint8Array.of(3))
    await begun.transaction.commit()

    expect(output.bytes()).toEqual(Uint8Array.of(1, 2, 3))
    expect(output.closed).toBe(true)
    expect(session.capabilities.durability).toBe('None')
  })

  it('isolates a failure before output but compromises the job after a write starts', async () => {
    const before = new SingleFileStreamOutputSession('before', recordingStream().stream)
    const untouched = await before.beginFile(outputFile('untouched', 1n))
    await expect(untouched.transaction.abort(new Error('source failed')))
      .resolves.toBe('FileIsolated')

    const output = recordingStream()
    const after = new SingleFileStreamOutputSession('after', output.stream)
    const started = await after.beginFile(outputFile('started', 2n))
    await started.transaction.writeRange(0n, Uint8Array.of(1))
    await expect(started.transaction.abort(new Error('later failure')))
      .resolves.toBe('JobOutputCompromised')
    expect(output.bytes()).toEqual(Uint8Array.of(1))
    expect(output.aborted).toBe(true)
  })

  it('commits an empty file without inventing a byte', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('empty', output.stream)
    const begun = await session.beginFile(outputFile('empty', 0n))
    await begun.transaction.commit()
    expect(output.bytes()).toEqual(new Uint8Array())
    expect(output.closed).toBe(true)
  })

  it('keeps a sink publication canonical when abort arrives at the close boundary', async () => {
    const closeStarted = deferred<void>()
    const publish = deferred<void>()
    const reason = new DOMException('late cancellation', 'AbortError')
    const race: {
      session?: SingleFileStreamOutputSession
      abort?: Promise<void>
    } = {}
    let published = false
    const output = new WritableStream<Uint8Array>({
      close: async () => {
        closeStarted.resolve()
        await publish.promise
        published = true
        if (race.session === undefined) throw new Error('Session is unavailable')
        race.abort = race.session.abortJob(reason)
      },
    })
    const session = new SingleFileStreamOutputSession('single-close-winner', output)
    race.session = session
    const begun = await session.beginFile(outputFile('empty', 0n))

    const commit = begun.transaction.commit()
    await closeStarted.promise
    publish.resolve()

    await expect(commit).resolves.toBeUndefined()
    if (race.abort === undefined) throw new Error('Sink did not trigger the close race')
    await expect(race.abort).resolves.toBeUndefined()
    expect(published).toBe(true)
  })

  it('lets abort win when the sink rejects its deferred close', async () => {
    const closeStarted = deferred<void>()
    const close = deferred<void>()
    const reason = new DOMException('cancelled before publication', 'AbortError')
    const output = new WritableStream<Uint8Array>({
      close: async () => {
        closeStarted.resolve()
        await close.promise
      },
    })
    const session = new SingleFileStreamOutputSession('single-abort-winner', output)
    const begun = await session.beginFile(outputFile('empty', 0n))

    const commit = begun.transaction.commit()
    await closeStarted.promise
    const abort = session.abortJob(reason)
    close.reject(reason)

    await expect(commit).rejects.toBe(reason)
    await expect(abort).resolves.toBeUndefined()
  })
})

describe('ZIP stream output', () => {
  it('skips a not-yet-started member and reports it without corrupting later members', async () => {
    const archive = new FakeArchive()
    let report: ZipCompletionReport | undefined
    const session = new ZipStreamOutputSession({
      outputSessionId: 'zip',
      archive,
      reportCompletion: (value) => { report = value },
    })
    const skipped = await session.beginFile(outputFile('skipped', 1n))
    const kept = await session.beginFile(outputFile('kept', 1n))
    await expect(skipped.transaction.abort(new Error('source failed')))
      .resolves.toBe('FileIsolated')
    await kept.transaction.writeRange(0n, Uint8Array.of(7))
    await kept.transaction.commit()
    const failedId = fileId('skipped')
    await session.finishJob(jobOutcome('CompletedWithErrors', summarizeTransferFailures([{
      kind: 'file',
      fileId: failedId,
      reason: new Error('source failed'),
    }])), ACTIVE_SIGNAL)

    expect(archive.files.map((file) => file.path.join('/'))).toEqual(['kept'])
    expect(archive.files[0]?.bytes).toEqual([7])
    expect(report?.outcome.failures
      .filter((failure) => failure.kind === 'file')
      .map((failure) => failure.fileId)).toEqual([failedId])
  })

  it('aborts the archive when a started member fails', async () => {
    const archive = new FakeArchive()
    const session = new ZipStreamOutputSession({ outputSessionId: 'zip', archive })
    const begun = await session.beginFile(outputFile('started', 2n))
    await begun.transaction.writeRange(0n, Uint8Array.of(1))

    await expect(begun.transaction.abort(new Error('failed member')))
      .resolves.toBe('JobOutputCompromised')
    expect(archive.aborted).toBe(true)
  })

  it('serializes concurrently prepared members without buffering the archive', async () => {
    const archive = new FakeArchive()
    const session = new ZipStreamOutputSession({ outputSessionId: 'zip', archive })
    const first = await session.beginFile(outputFile('first', 1n))
    const second = await session.beginFile(outputFile('second', 1n))
    const secondWrite = second.transaction.writeRange(0n, Uint8Array.of(2))
    await Promise.resolve()
    expect(archive.files).toHaveLength(0)

    await first.transaction.writeRange(0n, Uint8Array.of(1))
    await first.transaction.commit()
    await secondWrite
    await second.transaction.commit()

    expect(archive.files.map((file) => file.path.join('/'))).toEqual(['first', 'second'])
  })

  it('preserves empty entries without claiming unsupported browser mtime restoration', async () => {
    const archive = new FakeArchive()
    const session = new ZipStreamOutputSession({ outputSessionId: 'zip', archive })
    await session.ensureDirectory({ path: ['empty-dir'], modifiedTimeMilliseconds: 10n })
    await session.finalizeDirectory(
      { path: ['empty-dir'], modifiedTimeMilliseconds: 10n },
      ACTIVE_SIGNAL,
    )
    const empty = await session.beginFile({
      ...outputFile('empty-file', 0n),
      modifiedTimeMilliseconds: 20n,
    })
    await empty.transaction.commit()
    await session.finishJob(jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY), ACTIVE_SIGNAL)

    expect(session.capabilities.modificationTime).toBe(false)
    expect(archive.directories).toEqual([{ path: ['empty-dir'] }])
    expect(archive.files[0]).toMatchObject({
      path: ['empty-file'],
      bytes: [],
    })
    expect(archive.files[0]).not.toHaveProperty('modifiedTimeMilliseconds')
  })

  it('reports the canonical job outcome without retaining a second FileID authority', async () => {
    const archive = new FakeArchive()
    let report: ZipCompletionReport | undefined
    const session = new ZipStreamOutputSession({
      outputSessionId: 'zip-release',
      archive,
      reportCompletion: (value) => { report = value },
    })
    const committedId = fileId('committed')
    const begun = await session.beginFile(outputFile('committed', 1n))
    await begun.transaction.writeRange(0n, Uint8Array.of(1))
    await begun.transaction.commit()
    const outcome = jobOutcome('CompletedWithErrors', summarizeTransferFailures([{
      kind: 'file',
      fileId: committedId,
      reason: new Error('lease release failed'),
    }]))
    await session.finishJob(outcome, ACTIVE_SIGNAL)

    expect(report?.outcome).toBe(outcome)
    expect(archive.files.map((file) => file.path.join('/'))).toEqual(['committed'])
  })

  it('uses the archive close result as the direct-session race winner', async () => {
    const archive = new DeferredCloseArchive()
    let report: ZipCompletionReport | undefined
    const session = new ZipStreamOutputSession({
      outputSessionId: 'zip-close-winner',
      archive,
      reportCompletion: (value) => { report = value },
    })
    const outcome = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
    const finish = session.finishJob(outcome, ACTIVE_SIGNAL)
    await archive.closeStarted.promise
    const abort = session.abortJob(new DOMException('late cancellation', 'AbortError'))
    archive.releaseClose()

    await expect(finish).resolves.toBeUndefined()
    await expect(abort).resolves.toBeUndefined()
    expect(report?.outcome).toBe(outcome)
  })

  it('does not relabel published output when completion reporting throws', async () => {
    const archive = new FakeArchive()
    const session = new ZipStreamOutputSession({
      outputSessionId: 'zip-report-failure',
      archive,
      reportCompletion: () => { throw new Error('observer failed') },
    })

    await expect(session.finishJob(
      jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
      ACTIVE_SIGNAL,
    )).resolves.toBeUndefined()
    await expect(session.abortJob(new Error('must not revoke publication')))
      .resolves.toBeUndefined()
    expect(archive.closed).toBe(true)
    expect(archive.aborted).toBe(false)
  })

  it('writes a real store-mode Zip64 archive through the browser adapter', async () => {
    const output = recordingStream()
    const spool = new MemoryZipCentralDirectorySpool()
    const archive = new StreamingZipArchiveWriter(output.stream, spool)
    await archive.addDirectory({ path: ['tree'], modifiedTimeMilliseconds: 0n })
    const member = await archive.beginFile({ path: ['tree', 'file.bin'], exactSize: 3n })
    await member.write(Uint8Array.of(1, 2, 3))
    await member.close()
    await archive.close(ACTIVE_SIGNAL)

    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const entries = await reader.getEntries()
    expect(entries.map((entry) => entry.filename)).toEqual(['tree/', 'tree/file.bin'])
    const fileEntry = entries[1]
    if (fileEntry === undefined || fileEntry.directory) throw new Error('ZIP file entry is missing')
    const data = await fileEntry.getData(new Uint8ArrayWriter())
    expect(data).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
    expect(spool.cleared).toBe(true)
  })

  it('keeps a real archive committed when abort arrives from the publishing close', async () => {
    const closeStarted = deferred<void>()
    const publish = deferred<void>()
    const reason = new DOMException('late cancellation', 'AbortError')
    const race: {
      archive?: StreamingZipArchiveWriter
      abort?: Promise<void>
    } = {}
    let published = false
    const output = new WritableStream<Uint8Array>({
      close: async () => {
        closeStarted.resolve()
        await publish.promise
        published = true
        if (race.archive === undefined) throw new Error('Archive is unavailable')
        race.abort = race.archive.abort(reason)
      },
    })
    const archive = new StreamingZipArchiveWriter(output, new MemoryZipCentralDirectorySpool())
    race.archive = archive

    const close = archive.close(ACTIVE_SIGNAL)
    await closeStarted.promise
    publish.resolve()

    await expect(close).resolves.toBeUndefined()
    if (race.abort === undefined) throw new Error('Sink did not trigger the ZIP close race')
    await expect(race.abort).resolves.toBeUndefined()
    expect(published).toBe(true)
  })

  it('reports post-publication spool cleanup as retryable metadata cleanup', async () => {
    const spool = new FailingClearSpool()
    const archive = new StreamingZipArchiveWriter(recordingStream().stream, spool)

    await expect(archive.close(ACTIVE_SIGNAL)).resolves.toBeUndefined()
    expect(archive.cleanupPending).toBe(true)
    expect(archive.cleanupFailure).toBe(spool.failure)

    await expect(archive.retryCleanup()).resolves.toBeUndefined()
    expect(archive.cleanupPending).toBe(false)
    expect(spool.clearAttempts).toBe(2)
  })

  it('checksums the same owned bytes that survive sink backpressure', async () => {
    const chunks: Uint8Array[] = []
    let payloadStarted!: () => void
    let releasePayload!: () => void
    const started = new Promise<void>((resolve) => { payloadStarted = resolve })
    const released = new Promise<void>((resolve) => { releasePayload = resolve })
    const output = new WritableStream<Uint8Array>({
      async write(chunk) {
        if (chunk.byteLength === 3) {
          payloadStarted()
          await released
        }
        chunks.push(chunk.slice())
      },
    })
    const archive = new StreamingZipArchiveWriter(
      output,
      new MemoryZipCentralDirectorySpool(),
    )
    const member = await archive.beginFile({ path: ['owned.bin'], exactSize: 3n })
    const callerBytes = Uint8Array.of(1, 2, 3)
    const write = member.write(callerBytes)
    await started
    callerBytes.fill(9)
    releasePayload()
    await write
    await member.close()
    await archive.close(ACTIVE_SIGNAL)

    const reader = new ZipReader(new Uint8ArrayReader(concatenate(chunks)))
    const entry = (await reader.getEntries())[0]
    if (entry === undefined || entry.directory) throw new Error('Owned ZIP member is missing')
    expect(await entry.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
  })

  it('round-trips a large streamed archive through an independent ZIP parser', async () => {
    const payloadBytes = 8 * 1024 * 1024
    const transferChunkBytes = 64 * 1024
    const output = recordingStream()
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      new MemoryZipCentralDirectorySpool(),
    )
    const member = await archive.beginFile({ path: ['large.bin'], exactSize: BigInt(payloadBytes) })
    for (let offset = 0; offset < payloadBytes; offset += transferChunkBytes) {
      const chunk = new Uint8Array(Math.min(transferChunkBytes, payloadBytes - offset))
      chunk.fill((offset / transferChunkBytes) & 0xff)
      await member.write(chunk)
    }
    await member.close()
    await archive.close(ACTIVE_SIGNAL)

    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const entries = await reader.getEntries()
    const entry = entries[0]
    if (entry === undefined || entry.directory) throw new Error('Large ZIP member is missing')
    const decoded = await entry.getData(new Uint8ArrayWriter())
    expect(decoded.byteLength).toBe(payloadBytes)
    expect(decoded[0]).toBe(0)
    expect(decoded[transferChunkBytes]).toBe(1)
    expect(decoded[payloadBytes - 1]).toBe(127)
    await reader.close()
  })

  it('charges ZIP headers and central-directory bytes to the exact portable bound', async () => {
    expect(PORTABLE_DOWNLOAD_MAXIMUM_BYTES).toBe(64 * 1024 * 1024)
    const exactArchiveBytes = 249
    let publishedSize = -1
    const exact = createBoundedPortableDownloadStream('exact.zip', {
      createBlob: (parts) => new Blob([...parts]),
      publish: (_name, blob) => { publishedSize = blob.size },
    }, exactArchiveBytes)
    const exactArchive = new StreamingZipArchiveWriter(
      exact,
      new MemoryZipCentralDirectorySpool(),
    )
    const exactMember = await exactArchive.beginFile({ path: ['a'], exactSize: 1n })
    await exactMember.write(Uint8Array.of(1))
    await exactMember.close()
    await exactArchive.close(ACTIVE_SIGNAL)
    expect(publishedSize).toBe(exactArchiveBytes)

    const overflowSpool = new MemoryZipCentralDirectorySpool()
    const overflow = new StreamingZipArchiveWriter(
      createBoundedPortableDownloadStream('overflow.zip', {
        createBlob: (parts) => new Blob([...parts]),
        publish: () => { throw new Error('overflow archive must not publish') },
      }, exactArchiveBytes - 1),
      overflowSpool,
    )
    const overflowMember = await overflow.beginFile({ path: ['a'], exactSize: 1n })
    await overflowMember.write(Uint8Array.of(1))
    await overflowMember.close()
    await expect(overflow.close(ACTIVE_SIGNAL)).rejects.toMatchObject({ name: 'QuotaExceededError' })
    expect(overflowSpool.cleared).toBe(true)
  })

  it('clears durable central-directory metadata when cancellation aborts the archive', async () => {
    const output = recordingStream()
    const spool = new MemoryZipCentralDirectorySpool()
    const archive = new StreamingZipArchiveWriter(output.stream, spool)
    const member = await archive.beginFile({ path: ['cancelled'], exactSize: 2n })
    await member.write(Uint8Array.of(1))

    await archive.abort(new DOMException('cancelled', 'AbortError'))

    expect(output.aborted).toBe(true)
    expect(output.closed).toBe(false)
    expect(spool.cleared).toBe(true)
  })
})

interface RecordedStream {
  stream: WritableStream<Uint8Array>
  readonly chunks: Uint8Array[]
  closed: boolean
  aborted: boolean
  bytes(): Uint8Array
}

function recordingStream(): RecordedStream {
  const result: RecordedStream = {
    chunks: [],
    closed: false,
    aborted: false,
    stream: undefined as unknown as WritableStream<Uint8Array>,
    bytes: () => concatenate(result.chunks),
  }
  result.stream = new WritableStream<Uint8Array>({
    write: (chunk) => { result.chunks.push(chunk.slice()) },
    close: () => { result.closed = true },
    abort: () => { result.aborted = true },
  })
  return result
}

class FakeArchive implements ZipArchiveWriter {
  readonly cleanupPending = false
  readonly cleanupFailure = undefined
  readonly files: Array<{
    readonly path: readonly string[]
    readonly modifiedTimeMilliseconds?: bigint
    readonly bytes: number[]
  }> = []
  readonly directories: ZipArchiveEntry[] = []
  aborted = false
  closed = false
  #active = false

  async addDirectory(entry: ZipArchiveEntry): Promise<void> {
    this.directories.push({ ...entry, path: [...entry.path] })
  }

  async beginFile(entry: ZipArchiveFileEntry): Promise<ZipArchiveMember> {
    if (this.#active) throw new Error('fake archive has concurrent members')
    this.#active = true
    const file = {
      path: [...entry.path],
      ...(entry.modifiedTimeMilliseconds === undefined
        ? {}
        : { modifiedTimeMilliseconds: entry.modifiedTimeMilliseconds }),
      bytes: [] as number[],
    }
    this.files.push(file)
    let settled = false
    const settle = () => {
      if (settled) return
      settled = true
      this.#active = false
    }
    return {
      write: async (data) => { file.bytes.push(...data) },
      close: async () => { settle() },
      abort: async () => { settle() },
    }
  }

  async close(): Promise<void> {
    this.closed = true
  }

  async abort(): Promise<void> {
    this.aborted = true
    this.#active = false
  }

  async retryCleanup(): Promise<void> {}
}

class DeferredCloseArchive extends FakeArchive {
  readonly closeStarted = deferred<void>()
  readonly #close = deferred<void>()

  override async close(): Promise<void> {
    this.closeStarted.resolve()
    await this.#close.promise
    this.closed = true
  }

  releaseClose(): void {
    this.#close.resolve()
  }
}

class FailingClearSpool extends MemoryZipCentralDirectorySpool {
  readonly failure = new Error('central-directory cleanup failed')
  clearAttempts = 0

  override async clear(): Promise<void> {
    this.clearAttempts += 1
    if (this.clearAttempts === 1) throw this.failure
    await super.clear()
  }
}

function outputFile(name: string, exactSize: bigint): OutputFile {
  return {
    source: { shareInstance: 'share', fileId: name, fileRevision: `revision-${name}` },
    path: [name],
    exactSize,
  }
}

function concatenate(chunks: readonly Uint8Array[]): Uint8Array {
  const output = new Uint8Array(chunks.reduce((total, chunk) => total + chunk.byteLength, 0))
  let offset = 0
  for (const chunk of chunks) {
    output.set(chunk, offset)
    offset += chunk.byteLength
  }
  return output
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
