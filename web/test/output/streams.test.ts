import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  planZipLayout,
  ZipLayoutLedgerV1,
  type SealedZipLayoutPlanV1,
} from '../../src/output/zip-layout/layout'
import type {
  ZipEntryPlanV1,
  ZipEntrySpec,
} from '../../src/output/zip-layout/policy'
import {
  StreamingZipArchiveWriter,
  type ZipArchiveTraceEvent,
} from '../../src/output/streams/streaming-zip'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'

const ACTIVE_SIGNAL = new AbortController().signal
const RECEIVE_INTENT_DIGEST = digest(0x11)
const ARTIFACT_DIGEST = digest(0x22)
const PREPARATION_DIGEST = digest(0x33)
const DISCOVERY_DIGEST = digest(0x44)

describe('planned ZIP stream output', () => {
  it('derives the exact classic archive bound from the sealed plan and matches an independent parser', async () => {
    const plan = await preparedLayout([
      { kind: 'directory', path: ['tree'], modifiedTimeMilliseconds: 0n },
      { kind: 'file', path: ['tree', 'file.bin'], exactSize: 3n },
    ])
    const output = recordingStream()
    const spool = new MemoryZipCentralDirectorySpool()
    const events: ZipArchiveTraceEvent[] = []
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      spool,
      { kind: 'sealed', plan },
      (event) => { events.push(event) },
    )

    await archive.addDirectory(entry(plan, 0, 'directory'))
    const member = await archive.beginFile(entry(plan, 1, 'file'))
    await member.write(Uint8Array.of(1, 2, 3))
    await member.close()
    await archive.close(plan, ACTIVE_SIGNAL)

    expect(BigInt(output.bytes().byteLength)).toBe(plan.exactArchiveBytes)
    expect(plan.zip64EndRequired).toBe(false)
    expect(new DataView(output.bytes().buffer).getUint16(4, true)).toBe(20)
    expect(new DataView(output.bytes().buffer).getUint32(output.bytes().byteLength - 22, true))
      .toBe(0x0605_4b50)

    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const entries = await reader.getEntries()
    expect(entries.map((zipEntry) => zipEntry.filename)).toEqual(['tree/', 'tree/file.bin'])
    const fileEntry = entries[1]
    if (fileEntry === undefined || fileEntry.directory) throw new Error('ZIP file entry is missing')
    expect(await fileEntry.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
    expect(spool.cleared).toBe(true)
    expect(events).toEqual([
      {
        kind: 'zip-close-proof-accepted',
        receiveIntentDigest: RECEIVE_INTENT_DIGEST,
        layoutDigest: plan.digest,
        entryCount: 2n,
      },
      {
        kind: 'zip-archive-committed',
        receiveIntentDigest: RECEIVE_INTENT_DIGEST,
        layoutDigest: plan.digest,
        exactArchiveBytes: plan.exactArchiveBytes,
      },
    ])
  })

  it('accepts progressive close only from the completed discovery ledger', async () => {
    const ledger = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    const root = ledger.append({ kind: 'directory', path: ['tree'] })
    const file = ledger.append({ kind: 'file', path: ['tree', 'owned.bin'], exactSize: 3n })
    const output = recordingStream()
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      new MemoryZipCentralDirectorySpool(),
      { kind: 'progressive', ledger },
    )

    await archive.addDirectory(root)
    const member = await archive.beginFile(file)
    const callerBytes = Uint8Array.of(1, 2, 3)
    await member.write(callerBytes)
    callerBytes.fill(9)
    await member.close()
    ledger.completeDiscovery(DISCOVERY_DIGEST)
    const proof = await ledger.seal()
    await archive.close(proof, ACTIVE_SIGNAL)

    expect(BigInt(output.bytes().byteLength)).toBe(proof.exactArchiveBytes)
    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const fileEntry = (await reader.getEntries())[1]
    if (fileEntry === undefined || fileEntry.directory) throw new Error('ZIP file entry is missing')
    expect(await fileEntry.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
  })

  it('invalidates progressive proof and aborts output when a selected member fails', async () => {
    const ledger = new ZipLayoutLedgerV1(RECEIVE_INTENT_DIGEST, ARTIFACT_DIGEST)
    const root = ledger.append({ kind: 'directory', path: ['tree'] })
    const file = ledger.append({ kind: 'file', path: ['tree', 'partial.bin'], exactSize: 2n })
    const output = recordingStream()
    const spool = new MemoryZipCentralDirectorySpool()
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      spool,
      { kind: 'progressive', ledger },
    )

    await archive.addDirectory(root)
    const member = await archive.beginFile(file)
    await member.write(Uint8Array.of(1))
    await expect(member.abort(new Error('selected member failed'))).resolves.toBeUndefined()

    expect(output.aborted).toBe(true)
    expect(output.closed).toBe(false)
    expect(spool.cleared).toBe(true)
    expect(() => ledger.completeDiscovery(DISCOVERY_DIGEST)).toThrow(/cannot be completed/u)
    await expect(ledger.seal()).rejects.toThrow(/successful discovery/u)
  })

  it('rejects a valid but different sealed proof instead of publishing observed bytes', async () => {
    const entries: readonly ZipEntrySpec[] = [
      { kind: 'directory', path: ['tree'] },
      { kind: 'file', path: ['tree', 'file.bin'], exactSize: 1n },
    ]
    const expected = await preparedLayout(entries)
    const changed = await planZipLayout({
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      artifactDigest: digest(0x55),
      preparationManifestDigest: PREPARATION_DIGEST,
      entries,
    })
    const output = recordingStream()
    const spool = new MemoryZipCentralDirectorySpool()
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      spool,
      { kind: 'sealed', plan: expected },
    )

    await archive.addDirectory(entry(expected, 0, 'directory'))
    const member = await archive.beginFile(entry(expected, 1, 'file'))
    await member.write(Uint8Array.of(7))
    await member.close()

    await expect(archive.close(changed, ACTIVE_SIGNAL)).rejects.toThrow(/proof changed/u)
    expect(output.aborted).toBe(true)
    expect(output.closed).toBe(false)
    expect(spool.cleared).toBe(true)
  })

  it('checksums the same owned bytes that survive sink backpressure', async () => {
    const plan = await preparedLayout([
      { kind: 'directory', path: ['tree'] },
      { kind: 'file', path: ['tree', 'owned.bin'], exactSize: 3n },
    ])
    const chunks: Uint8Array[] = []
    const started = deferred<void>()
    const release = deferred<void>()
    const output = new WritableStream<Uint8Array>({
      async write(chunk) {
        if (chunk.byteLength === 3) {
          started.resolve()
          await release.promise
        }
        chunks.push(chunk.slice())
      },
    })
    const archive = new StreamingZipArchiveWriter(
      output,
      new MemoryZipCentralDirectorySpool(),
      { kind: 'sealed', plan },
    )
    await archive.addDirectory(entry(plan, 0, 'directory'))
    const member = await archive.beginFile(entry(plan, 1, 'file'))
    const callerBytes = Uint8Array.of(1, 2, 3)
    const write = member.write(callerBytes)
    await started.promise
    callerBytes.fill(9)
    release.resolve()
    await write
    await member.close()
    await archive.close(plan, ACTIVE_SIGNAL)

    const reader = new ZipReader(new Uint8ArrayReader(concatenate(chunks)))
    const fileEntry = (await reader.getEntries())[1]
    if (fileEntry === undefined || fileEntry.directory) throw new Error('Owned ZIP member is missing')
    expect(await fileEntry.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
  })

  it('keeps a published archive canonical when abort arrives from the sink close', async () => {
    const plan = await preparedLayout([{ kind: 'directory', path: ['tree'] }])
    const closeStarted = deferred<void>()
    const publish = deferred<void>()
    const reason = new DOMException('late cancellation', 'AbortError')
    let abort: Promise<void> | undefined
    let published = false
    const output = new WritableStream<Uint8Array>({
      close: async () => {
        closeStarted.resolve()
        await publish.promise
        published = true
        abort = archive.abort(reason)
      },
    })
    const archive = new StreamingZipArchiveWriter(
      output,
      new MemoryZipCentralDirectorySpool(),
      { kind: 'sealed', plan },
    )
    await archive.addDirectory(entry(plan, 0, 'directory'))

    const close = archive.close(plan, ACTIVE_SIGNAL)
    await closeStarted.promise
    publish.resolve()
    await expect(close).resolves.toBeUndefined()
    if (abort === undefined) throw new Error('Sink did not trigger the ZIP close race')
    await expect(abort).resolves.toBeUndefined()
    expect(published).toBe(true)
  })

  it('refuses publication when durable central-directory bytes change without changing length', async () => {
    const plan = await preparedLayout([{ kind: 'directory', path: ['tree'] }])
    const output = recordingStream()
    const archive = new StreamingZipArchiveWriter(
      output.stream,
      new CorruptingReplaySpool(),
      { kind: 'sealed', plan },
    )
    await archive.addDirectory(entry(plan, 0, 'directory'))

    await expect(archive.close(plan, ACTIVE_SIGNAL)).rejects.toThrow(/spool content changed/u)
    expect(output.aborted).toBe(true)
    expect(output.closed).toBe(false)
  })

  it('reports post-publication spool cleanup as retryable metadata cleanup', async () => {
    const plan = await preparedLayout([{ kind: 'directory', path: ['tree'] }])
    const spool = new FailingClearSpool()
    const archive = new StreamingZipArchiveWriter(
      recordingStream().stream,
      spool,
      { kind: 'sealed', plan },
    )
    await archive.addDirectory(entry(plan, 0, 'directory'))

    await expect(archive.close(plan, ACTIVE_SIGNAL)).resolves.toBeUndefined()
    expect(archive.cleanupPending).toBe(true)
    expect(archive.cleanupFailure).toBe(spool.failure)

    await expect(archive.retryCleanup()).resolves.toBeUndefined()
    expect(archive.cleanupPending).toBe(false)
    expect(spool.clearAttempts).toBe(2)
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
class FailingClearSpool extends MemoryZipCentralDirectorySpool {
  readonly failure = new Error('central-directory cleanup failed')
  clearAttempts = 0

  override async clear(): Promise<void> {
    this.clearAttempts += 1
    if (this.clearAttempts === 1) throw this.failure
    await super.clear()
  }
}

class CorruptingReplaySpool extends MemoryZipCentralDirectorySpool {
  override async readChunk(index: number): Promise<Uint8Array | undefined> {
    const chunk = await super.readChunk(index)
    if (chunk !== undefined && chunk.byteLength > 0) chunk[0] = (chunk[0] ?? 0) ^ 0xff
    return chunk
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
async function preparedLayout(
  entries: readonly ZipEntrySpec[],
): Promise<SealedZipLayoutPlanV1> {
  return planZipLayout({
    receiveIntentDigest: RECEIVE_INTENT_DIGEST,
    artifactDigest: ARTIFACT_DIGEST,
    preparationManifestDigest: PREPARATION_DIGEST,
    entries,
  })
}

function entry(
  plan: SealedZipLayoutPlanV1,
  index: number,
  kind: 'directory' | 'file',
): ZipEntryPlanV1 {
  const candidate = plan.entries[index]
  if (candidate === undefined || candidate.kind !== kind) {
    throw new Error(`ZIP ${kind} plan is missing`)
  }
  return candidate
}

function digest(fill: number): string {
  return encodeBase64Url(new Uint8Array(32).fill(fill))
}
