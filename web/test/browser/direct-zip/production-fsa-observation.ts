interface WritableObservation {
  readonly id: number
  readonly existingBytes: number
  readonly preservedBytes: number
  readonly writes: { readonly position: number; readonly bytes: number; readonly prefixBytes: number }[]
  closes: number
}

/** Observe native staging costs without replacing browser file or journal semantics. */
export function observeProductionDirectZipFileSystem() {
  const filePrototype = FileSystemFileHandle.prototype
  const streamPrototype = FileSystemWritableFileStream.prototype
  const originalOpen = filePrototype.createWritable
  const originalWrite = streamPrototype.write
  const originalClose = streamPrototype.close
  const records: WritableObservation[] = []
  const streams = new WeakMap<FileSystemWritableFileStream, WritableObservation>()

  filePrototype.createWritable = async function (options) {
    const existingBytes = (await this.getFile()).size
    const writable = await originalOpen.call(this, options)
    const record: WritableObservation = {
      id: records.length, existingBytes,
      preservedBytes: options?.keepExistingData === true ? existingBytes : 0,
      writes: [], closes: 0,
    }
    records.push(record)
    streams.set(writable, record)
    return writable
  }
  streamPrototype.write = async function (chunk) {
    const record = streams.get(this)
    await originalWrite.call(this, chunk)
    if (record !== undefined && isPositionedWrite(chunk)) {
      const bytes = dataBytes(chunk.data)
      const position = chunk.position
      record.writes.push({ position, bytes,
        prefixBytes: Math.max(0, Math.min(bytes, record.existingBytes - position)) })
    }
  }
  streamPrototype.close = async function () {
    await originalClose.call(this)
    const record = streams.get(this)
    if (record !== undefined) record.closes += 1
  }

  return {
    snapshot: () => ({
      opens: records.length,
      prefixBytes: records.reduce((sum, record) => sum + record.preservedBytes +
        record.writes.reduce((bytes, write) => bytes + write.prefixBytes, 0), 0),
      writes: records.reduce((sum, record) => sum + record.writes.length, 0),
      writtenBytes: records.reduce((sum, record) => sum +
        record.writes.reduce((bytes, write) => bytes + write.bytes, 0), 0),
      closes: records.reduce((sum, record) => sum + record.closes, 0),
      lastWritable: records.at(-1)?.id,
    }),
    restore: () => {
      filePrototype.createWritable = originalOpen
      streamPrototype.write = originalWrite
      streamPrototype.close = originalClose
    },
  }
}

function isPositionedWrite(chunk: FileSystemWriteChunkType): chunk is WriteParams & {
  readonly type: 'write'; readonly position: number; readonly data: NonNullable<WriteParams['data']>
} {
  return typeof chunk === 'object' && chunk !== null && 'type' in chunk && chunk.type === 'write' &&
    'position' in chunk && typeof chunk.position === 'number' &&
    'data' in chunk && chunk.data !== undefined && chunk.data !== null
}

function dataBytes(data: NonNullable<WriteParams['data']>): number {
  if (typeof data === 'string') return new TextEncoder().encode(data).byteLength
  if (data instanceof Blob) return data.size
  return data.byteLength
}
