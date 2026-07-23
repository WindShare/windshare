import type {
  ZipArchiveEntry,
  ZipArchiveFileEntry,
  ZipArchiveMember,
  ZipArchiveWriter,
} from './zip-archive'
import type { ZipCentralDirectorySpool } from './zip-spool'

const ZIP_LOCAL_FILE_HEADER = 0x04034b50
const ZIP_DATA_DESCRIPTOR = 0x08074b50
const ZIP_CENTRAL_DIRECTORY_HEADER = 0x02014b50
const ZIP64_END_OF_CENTRAL_DIRECTORY = 0x06064b50
const ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR = 0x07064b50
const ZIP_END_OF_CENTRAL_DIRECTORY = 0x06054b50
const ZIP64_EXTRA_FIELD = 0x0001
const ZIP64_VERSION = 45
const ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS = 0x0808
const ZIP_STORE_METHOD = 0
const ZIP_DIRECTORY_ATTRIBUTE = 0x10
const ZIP_MAXIMUM_UINT16 = 0xffff
const ZIP_MAXIMUM_UINT32 = 0xffff_ffff
const ZIP_MAXIMUM_UINT64 = (1n << 64n) - 1n
const ZIP_MINIMUM_DATE = new Date(1980, 0, 1)
const ZIP_MAXIMUM_DATE = new Date(2107, 11, 31, 23, 59, 58)
const ZIP_MINIMUM_DATE_MILLISECONDS = BigInt(ZIP_MINIMUM_DATE.getTime())
const ZIP_MAXIMUM_DATE_MILLISECONDS = BigInt(ZIP_MAXIMUM_DATE.getTime())
const ZIP64_END_RECORD_BYTES = 56
const ZIP64_LOCATOR_BYTES = 20
const ZIP_END_RECORD_BYTES = 22

export class StreamingZipArchiveWriter implements ZipArchiveWriter {
  readonly #spool: ZipCentralDirectorySpool
  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  #active: StreamingZipMember | undefined
  #offset = 0n
  #entryCount = 0n
  #state: 'open' | 'closing' | 'committed' | 'aborting' | 'aborted' | 'failed' = 'open'
  #settlementPromise: Promise<void> | undefined
  #abortPromise: Promise<void> | undefined
  #closeController: AbortController | undefined
  #settlementCleanupFailure: unknown
  #spoolCleanupFailure: unknown
  #spoolCleanupPromise: Promise<void> | undefined

  constructor(output: WritableStream<Uint8Array>, spool: ZipCentralDirectorySpool) {
    if (output.locked) throw new TypeError('ZIP output stream is already locked')
    this.#writer = output.getWriter()
    this.#spool = spool
  }

  get cleanupPending(): boolean {
    return this.#spoolCleanupFailure !== undefined
  }

  get cleanupFailure(): unknown {
    return this.#spoolCleanupFailure
  }

  async addDirectory(entry: ZipArchiveEntry): Promise<void> {
    this.#requireIdle()
    const encoded = encodedEntry(entry, true)
    const localOffset = this.#offset
    try {
      await this.#write(localHeader(encoded, 0n))
      await this.#write(dataDescriptor(0, 0n))
      await this.#spool.append(centralDirectoryRecord(encoded, 0, 0n, localOffset))
      this.#entryCount += 1n
    } catch (error) {
      try {
        await this.abort(error)
      } catch (abortError) {
        throw new AggregateError(
          [error, abortError],
          'ZIP directory output and abort failed',
          { cause: abortError },
        )
      }
      throw error
    }
  }

  async beginFile(entry: ZipArchiveFileEntry): Promise<ZipArchiveMember> {
    this.#requireIdle()
    requireUint64(entry.exactSize, 'ZIP member size')
    const encoded = encodedEntry(entry, false)
    const localOffset = this.#offset
    // The local header is the protocol-defined no-return boundary for a member.
    try {
      await this.#write(localHeader(encoded, entry.exactSize))
    } catch (error) {
      try {
        await this.abort(error)
      } catch (abortError) {
        throw new AggregateError(
          [error, abortError],
          'ZIP member start and abort failed',
          { cause: abortError },
        )
      }
      throw error
    }
    const member = new StreamingZipMember(
      entry.exactSize,
      encoded,
      localOffset,
      (chunk) => this.#write(chunk),
      (record): Promise<void> => this.#commitMember(record),
      () => { this.#active = undefined },
    )
    this.#active = member
    return member
  }

  close(signal: AbortSignal): Promise<void> {
    this.#requireIdle()
    signal.throwIfAborted()
    this.#state = 'closing'
    const controller = new AbortController()
    this.#closeController = controller
    const detach = forwardAbort(signal, controller)
    const operation = this.#performClose(controller.signal).then(
      () => { this.#state = 'committed' },
      (error: unknown) => {
        this.#state = this.#settlementCleanupFailure === undefined ? 'aborted' : 'failed'
        throw error
      },
    ).finally(() => {
      detach()
      this.#closeController = undefined
    })
    this.#settlementPromise = operation
    return operation
  }

  abort(reason: unknown): Promise<void> {
    if (this.#abortPromise !== undefined) return this.#abortPromise
    let operation: Promise<void>
    if (this.#state === 'closing') {
      this.#closeController?.abort(reason)
      operation = this.#normalizedCloseAbort()
    } else if (this.#state === 'committed' || this.#state === 'aborted') {
      operation = Promise.resolve()
    } else if (this.#state === 'failed') {
      operation = Promise.reject(this.#settlementCleanupFailure)
    } else {
      this.#state = 'aborting'
      operation = this.#performAbort(reason).then(
        () => { this.#state = 'aborted' },
        (error: unknown) => {
          this.#state = 'failed'
          throw error
        },
      )
      this.#settlementPromise = operation
    }
    this.#abortPromise = operation
    // A signal listener may become the first abort owner. Observing rejection here
    // prevents a detached unhandled promise while public callers still receive it.
    operation.catch(() => undefined)
    return operation
  }

  retryCleanup(): Promise<void> {
    if (this.#spoolCleanupFailure === undefined) return Promise.resolve()
    if (this.#spoolCleanupPromise !== undefined) return this.#spoolCleanupPromise
    const operation = this.#spool.clear().then(
      () => { this.#spoolCleanupFailure = undefined },
      (error: unknown) => {
        this.#spoolCleanupFailure = error
        throw error
      },
    ).finally(() => { this.#spoolCleanupPromise = undefined })
    this.#spoolCleanupPromise = operation
    return operation
  }

  async #performClose(signal: AbortSignal): Promise<void> {
    let failure: unknown
    let published = false
    let outputAbort: Promise<void> | undefined
    const abortOutput = () => {
      outputAbort ??= writerAbort(this.#writer, abortReason(signal))
      outputAbort.catch(() => undefined)
    }
    signal.addEventListener('abort', abortOutput, { once: true })
    try {
      await this.#writeCentralDirectory(signal)
      await this.#writer.close()
      published = true
      // If close won after an abort request, the stream is already terminal and
      // published. Awaiting the losing interrupt prevents detached work without
      // allowing it to rewrite the committed result.
      await outputAbort?.catch(() => undefined)
    } catch (error) {
      failure = await this.#settleFailedClose(error, outputAbort)
    } finally {
      signal.removeEventListener('abort', abortOutput)
      this.#writer.releaseLock()
      failure = await this.#clearSpoolAfterSettlement(published, failure)
    }
    if (failure !== undefined) throw failure
  }

  async #writeCentralDirectory(signal: AbortSignal): Promise<void> {
    const centralOffset = this.#offset
    const manifest = await this.#spool.seal()
    signal.throwIfAborted()
    if (manifest.recordCount !== this.#entryCount) {
      throw new Error('ZIP central-directory spool record count changed')
    }
    for (let index = 0; index < manifest.chunkCount; index += 1) {
      signal.throwIfAborted()
      const chunk = await this.#spool.readChunk(index)
      if (chunk === undefined) throw new Error('ZIP central-directory spool ended early')
      await this.#write(chunk)
    }
    if (this.#offset - centralOffset !== manifest.byteLength) {
      throw new Error('ZIP central-directory spool byte count changed')
    }
    const zip64Offset = this.#offset
    signal.throwIfAborted()
    await this.#write(zip64EndRecord(this.#entryCount, manifest.byteLength, centralOffset))
    await this.#write(zip64Locator(zip64Offset))
    await this.#write(classicEndRecord())
    signal.throwIfAborted()
  }

  async #settleFailedClose(
    closeFailure: unknown,
    requestedAbort: Promise<void> | undefined,
  ): Promise<unknown> {
    try {
      await (requestedAbort ?? writerAbort(this.#writer, closeFailure))
      return closeFailure
    } catch (abortFailure) {
      this.#settlementCleanupFailure = abortFailure
      return new AggregateError(
        [closeFailure, abortFailure],
        'ZIP close and output abort failed',
      )
    }
  }

  async #clearSpoolAfterSettlement(published: boolean, failure: unknown): Promise<unknown> {
    try {
      await this.#spool.clear()
      return failure
    } catch (clearFailure) {
      if (published) {
        this.#spoolCleanupFailure = clearFailure
        return failure
      }
      this.#settlementCleanupFailure = this.#settlementCleanupFailure === undefined
        ? clearFailure
        : new AggregateError(
            [this.#settlementCleanupFailure, clearFailure],
            'ZIP abort cleanup failed',
          )
      return failure === undefined
        ? clearFailure
        : new AggregateError(
            [failure, clearFailure],
            'ZIP output and metadata cleanup failed',
          )
    }
  }

  async #performAbort(reason: unknown): Promise<void> {
    const failures: unknown[] = []
    try {
      await this.#active?.abort()
    } catch (error) {
      failures.push(error)
    }
    try {
      await this.#writer.abort(reason)
    } catch (error) {
      failures.push(error)
    } finally {
      this.#writer.releaseLock()
    }
    try {
      await this.#spool.clear()
    } catch (error) {
      failures.push(error)
    }
    if (failures.length > 0) {
      this.#settlementCleanupFailure = new AggregateError(failures, 'ZIP stream abort failed')
      throw this.#settlementCleanupFailure
    }
  }

  async #normalizedCloseAbort(): Promise<void> {
    const settlement = this.#settlementPromise
    if (settlement === undefined) throw new Error('ZIP close settlement is missing')
    try {
      await settlement
    } catch (error) {
      if (this.#state === 'aborted') return
      throw this.#settlementCleanupFailure ?? error
    }
  }

  async #commitMember(record: Uint8Array): Promise<void> {
    if (this.#active === undefined) throw new Error('ZIP member is not active')
    await this.#spool.append(record)
    this.#entryCount += 1n
  }

  async #write(chunk: Uint8Array): Promise<void> {
    await this.#writer.write(chunk)
    this.#offset += BigInt(chunk.byteLength)
    requireUint64(this.#offset, 'ZIP output offset')
  }

  #requireIdle(): void {
    if (this.#state !== 'open') throw new Error('ZIP archive is settled')
    if (this.#active !== undefined) throw new Error('ZIP archive already has an active member')
  }
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('ZIP output aborted', 'AbortError')
}

function writerAbort(
  writer: WritableStreamDefaultWriter<Uint8Array>,
  reason: unknown,
): Promise<void> {
  try {
    return writer.abort(reason)
  } catch (error) {
    return Promise.reject(error)
  }
}

function forwardAbort(source: AbortSignal, target: AbortController): () => void {
  const abort = () => target.abort(abortReason(source))
  if (source.aborted) {
    abort()
    return () => {}
  }
  source.addEventListener('abort', abort, { once: true })
  return () => source.removeEventListener('abort', abort)
}

class StreamingZipMember implements ZipArchiveMember {
  readonly #exactSize: bigint
  readonly #entry: EncodedZipEntry
  readonly #localOffset: bigint
  readonly #writeOutput: (chunk: Uint8Array) => Promise<void>
  readonly #commitRecord: (record: Uint8Array) => Promise<void>
  readonly #settled: () => void
  #written = 0n
  #crc = 0xffff_ffff
  #closed = false

  constructor(
    exactSize: bigint,
    entry: EncodedZipEntry,
    localOffset: bigint,
    writeOutput: (chunk: Uint8Array) => Promise<void>,
    commitRecord: (record: Uint8Array) => Promise<void>,
    settled: () => void,
  ) {
    this.#exactSize = exactSize
    this.#entry = entry
    this.#localOffset = localOffset
    this.#writeOutput = writeOutput
    this.#commitRecord = commitRecord
    this.#settled = once(settled)
  }

  async write(data: Uint8Array): Promise<void> {
    this.#requireOpen()
    if (this.#written + BigInt(data.byteLength) > this.#exactSize) {
      throw new RangeError('ZIP member received more bytes than declared')
    }
    if (data.byteLength === 0) return
    const snapshot = data.slice()
    // Backpressure gives the caller time to mutate its buffer. The checksum must
    // authenticate the owned bytes handed to the sink, not later shared state.
    const nextCrc = updateCrc32(this.#crc, snapshot)
    await this.#writeOutput(snapshot)
    this.#crc = nextCrc
    this.#written += BigInt(snapshot.byteLength)
  }

  async close(): Promise<void> {
    this.#requireOpen()
    this.#closed = true
    try {
      if (this.#written !== this.#exactSize) throw new Error('ZIP member size does not match its declaration')
      const crc = (this.#crc ^ 0xffff_ffff) >>> 0
      await this.#writeOutput(dataDescriptor(crc, this.#exactSize))
      await this.#commitRecord(centralDirectoryRecord(
        this.#entry,
        crc,
        this.#exactSize,
        this.#localOffset,
      ))
    } finally {
      this.#settled()
    }
  }

  async abort(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    this.#settled()
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('ZIP member is settled')
  }
}

interface EncodedZipEntry {
  readonly name: Uint8Array
  readonly time: number
  readonly date: number
  readonly directory: boolean
}

function encodedEntry(entry: ZipArchiveEntry, directory: boolean): EncodedZipEntry {
  const path = archivePath(entry.path)
  const name = new TextEncoder().encode(directory ? `${path}/` : path)
  if (name.byteLength > ZIP_MAXIMUM_UINT16) throw new RangeError('ZIP member name is too long')
  const { time, date } = dosDateTime(entry.modifiedTimeMilliseconds)
  return { name, time, date, directory }
}

function localHeader(entry: EncodedZipEntry, exactSize: bigint): Uint8Array {
  const extra = zip64Extra([exactSize, exactSize])
  const output = new Uint8Array(30 + entry.name.byteLength + extra.byteLength)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_LOCAL_FILE_HEADER, true)
  view.setUint16(4, ZIP64_VERSION, true)
  view.setUint16(6, ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS, true)
  view.setUint16(8, ZIP_STORE_METHOD, true)
  view.setUint16(10, entry.time, true)
  view.setUint16(12, entry.date, true)
  view.setUint32(14, 0, true)
  view.setUint32(18, ZIP_MAXIMUM_UINT32, true)
  view.setUint32(22, ZIP_MAXIMUM_UINT32, true)
  view.setUint16(26, entry.name.byteLength, true)
  view.setUint16(28, extra.byteLength, true)
  output.set(entry.name, 30)
  output.set(extra, 30 + entry.name.byteLength)
  return output
}

function dataDescriptor(crc: number, exactSize: bigint): Uint8Array {
  const output = new Uint8Array(24)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_DATA_DESCRIPTOR, true)
  view.setUint32(4, crc, true)
  view.setBigUint64(8, exactSize, true)
  view.setBigUint64(16, exactSize, true)
  return output
}

function centralDirectoryRecord(
  entry: EncodedZipEntry,
  crc: number,
  exactSize: bigint,
  localOffset: bigint,
): Uint8Array {
  requireUint64(localOffset, 'ZIP local header offset')
  const extra = zip64Extra([exactSize, exactSize, localOffset])
  const output = new Uint8Array(46 + entry.name.byteLength + extra.byteLength)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_CENTRAL_DIRECTORY_HEADER, true)
  view.setUint16(4, ZIP64_VERSION, true)
  view.setUint16(6, ZIP64_VERSION, true)
  view.setUint16(8, ZIP_UTF8_AND_DATA_DESCRIPTOR_FLAGS, true)
  view.setUint16(10, ZIP_STORE_METHOD, true)
  view.setUint16(12, entry.time, true)
  view.setUint16(14, entry.date, true)
  view.setUint32(16, crc, true)
  view.setUint32(20, ZIP_MAXIMUM_UINT32, true)
  view.setUint32(24, ZIP_MAXIMUM_UINT32, true)
  view.setUint16(28, entry.name.byteLength, true)
  view.setUint16(30, extra.byteLength, true)
  view.setUint16(32, 0, true)
  view.setUint16(34, 0, true)
  view.setUint16(36, 0, true)
  view.setUint32(38, entry.directory ? ZIP_DIRECTORY_ATTRIBUTE : 0, true)
  view.setUint32(42, ZIP_MAXIMUM_UINT32, true)
  output.set(entry.name, 46)
  output.set(extra, 46 + entry.name.byteLength)
  return output
}

function zip64Extra(values: readonly bigint[]): Uint8Array {
  const dataBytes = values.length * 8
  const output = new Uint8Array(4 + dataBytes)
  const view = new DataView(output.buffer)
  view.setUint16(0, ZIP64_EXTRA_FIELD, true)
  view.setUint16(2, dataBytes, true)
  values.forEach((value, index) => {
    requireUint64(value, 'ZIP64 extra value')
    view.setBigUint64(4 + index * 8, value, true)
  })
  return output
}

function zip64EndRecord(entries: bigint, centralBytes: bigint, centralOffset: bigint): Uint8Array {
  const output = new Uint8Array(ZIP64_END_RECORD_BYTES)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP64_END_OF_CENTRAL_DIRECTORY, true)
  view.setBigUint64(4, 44n, true)
  view.setUint16(12, ZIP64_VERSION, true)
  view.setUint16(14, ZIP64_VERSION, true)
  view.setUint32(16, 0, true)
  view.setUint32(20, 0, true)
  view.setBigUint64(24, entries, true)
  view.setBigUint64(32, entries, true)
  view.setBigUint64(40, centralBytes, true)
  view.setBigUint64(48, centralOffset, true)
  return output
}

function zip64Locator(zip64Offset: bigint): Uint8Array {
  const output = new Uint8Array(ZIP64_LOCATOR_BYTES)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP64_END_OF_CENTRAL_DIRECTORY_LOCATOR, true)
  view.setUint32(4, 0, true)
  view.setBigUint64(8, zip64Offset, true)
  view.setUint32(16, 1, true)
  return output
}

function classicEndRecord(): Uint8Array {
  const output = new Uint8Array(ZIP_END_RECORD_BYTES)
  const view = new DataView(output.buffer)
  view.setUint32(0, ZIP_END_OF_CENTRAL_DIRECTORY, true)
  view.setUint16(4, 0, true)
  view.setUint16(6, 0, true)
  view.setUint16(8, ZIP_MAXIMUM_UINT16, true)
  view.setUint16(10, ZIP_MAXIMUM_UINT16, true)
  view.setUint32(12, ZIP_MAXIMUM_UINT32, true)
  view.setUint32(16, ZIP_MAXIMUM_UINT32, true)
  view.setUint16(20, 0, true)
  return output
}

function dosDateTime(milliseconds: bigint | undefined): { readonly time: number; readonly date: number } {
  let clamped = milliseconds ?? ZIP_MINIMUM_DATE_MILLISECONDS
  if (clamped < ZIP_MINIMUM_DATE_MILLISECONDS) clamped = ZIP_MINIMUM_DATE_MILLISECONDS
  if (clamped > ZIP_MAXIMUM_DATE_MILLISECONDS) clamped = ZIP_MAXIMUM_DATE_MILLISECONDS
  const value = new Date(Number(clamped))
  return {
    time: (value.getHours() << 11) | (value.getMinutes() << 5) | (value.getSeconds() >>> 1),
    date: ((value.getFullYear() - 1980) << 9) | ((value.getMonth() + 1) << 5) | value.getDate(),
  }
}

function archivePath(path: readonly string[]): string {
  if (path.length === 0 || path.some((segment) =>
    segment.length === 0 || segment === '.' || segment === '..' ||
    segment.includes('/') || segment.includes('\\') || segment.includes('\0'))) {
    throw new TypeError('ZIP path is not canonical')
  }
  return path.join('/')
}

const CRC32_TABLE = crc32Table()

function updateCrc32(crc: number, bytes: Uint8Array): number {
  let current = crc >>> 0
  for (const byte of bytes) current = CRC32_TABLE[(current ^ byte) & 0xff]! ^ (current >>> 8)
  return current >>> 0
}

function crc32Table(): Uint32Array {
  const table = new Uint32Array(256)
  for (let index = 0; index < table.length; index += 1) {
    let value = index
    for (let bit = 0; bit < 8; bit += 1) {
      value = (value & 1) === 0 ? value >>> 1 : 0xedb8_8320 ^ (value >>> 1)
    }
    table[index] = value >>> 0
  }
  return table
}

function requireUint64(value: bigint, label: string): void {
  if (value < 0n || value > ZIP_MAXIMUM_UINT64) throw new RangeError(`${label} exceeds ZIP64`)
}

function once(action: () => void): () => void {
  let called = false
  return () => {
    if (called) return
    called = true
    action()
  }
}
