import { describe, expect, it } from 'vitest'

import {
  ZIP_OUTPUT_METADATA_BUFFER_BYTES,
  ZipOutputSink,
} from '../../src/output/streams/zip-output-sink'

describe('bounded ZIP output sink', () => {
  it('coalesces framing metadata while preserving the payload backpressure boundary', async () => {
    const writes: Uint8Array[] = []
    const sink = new ZipOutputSink(new WritableStream<Uint8Array>({
      write(chunk) { writes.push(chunk.slice()) },
    }))

    await sink.appendMetadata(Uint8Array.of(1, 2))
    await sink.appendMetadata(Uint8Array.of(3))
    expect(writes).toEqual([])

    await sink.writePayload(Uint8Array.of(4, 5))
    await sink.close()
    sink.releaseLock()
    expect(writes).toEqual([
      Uint8Array.of(1, 2, 3),
      Uint8Array.of(4, 5),
    ])
  })

  it('never combines metadata beyond the bounded stream write', async () => {
    const writeLengths: number[] = []
    const sink = new ZipOutputSink(new WritableStream<Uint8Array>({
      write(chunk) { writeLengths.push(chunk.byteLength) },
    }))

    await sink.appendMetadata(new Uint8Array(ZIP_OUTPUT_METADATA_BUFFER_BYTES - 1))
    await sink.appendMetadata(Uint8Array.of(1, 2))
    await expect(sink.appendMetadata(
      new Uint8Array(ZIP_OUTPUT_METADATA_BUFFER_BYTES + 1),
    )).rejects.toThrow(/bounded stream write/u)
    await sink.close()
    sink.releaseLock()

    expect(writeLengths).toEqual([ZIP_OUTPUT_METADATA_BUFFER_BYTES - 1, 2])
  })

  it('discards unpublished metadata when the archive aborts', async () => {
    const writes: Uint8Array[] = []
    let observedReason: unknown
    const sink = new ZipOutputSink(new WritableStream<Uint8Array>({
      write(chunk) { writes.push(chunk.slice()) },
      abort(reason) { observedReason = reason },
    }))
    const reason = new Error('cancel output')

    await sink.appendMetadata(Uint8Array.of(1, 2, 3))
    await sink.abort(reason)
    sink.releaseLock()

    expect(writes).toEqual([])
    expect(observedReason).toBe(reason)
  })
})
