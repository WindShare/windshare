import { describe, expect, it } from 'vitest'

import { V2Mp4RangePreview, V2VideoPreviewError, type V2PreviewRangeSource } from '../../src/preview/mp4-range'
import type { ByteRange } from '../../src/content/geometry'

function box(bytes: Uint8Array, offset: number, size: number, type: string): void {
  new DataView(bytes.buffer).setUint32(offset, size, false)
  bytes.set([...type].map((value) => value.charCodeAt(0)), offset + 4)
}

class RecordedSource implements V2PreviewRangeSource {
  readonly exactSize: bigint
  readonly ranges: ByteRange[] = []
  readonly #bytes: Uint8Array<ArrayBuffer>

  constructor(bytes: Uint8Array<ArrayBuffer>) {
    this.#bytes = bytes
    this.exactSize = BigInt(bytes.byteLength)
  }

  async read(range: ByteRange, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
    signal.throwIfAborted()
    this.ranges.push(range)
    return this.#bytes.slice(Number(range.start), Number(range.end))
  }
}

describe('v2 MP4 bounded range parser', () => {
  it('jumps over mdat without reading the full file and probes both metadata edges', async () => {
    const bytes = new Uint8Array(1 << 20)
    box(bytes, 0, 24, 'ftyp')
    box(bytes, 24, bytes.byteLength - 32, 'mdat')
    box(bytes, bytes.byteLength - 8, 8, 'moov')
    const source = new RecordedSource(bytes)

    await expect(V2Mp4RangePreview.open(source, new AbortController().signal))
      .rejects.toBeInstanceOf(V2VideoPreviewError)
    expect(source.ranges[0]).toEqual({ start: 0n, end: 256n * 1024n })
    expect(source.ranges[1]).toEqual({
      start: BigInt(bytes.byteLength - 256 * 1024),
      end: BigInt(bytes.byteLength),
    })
    expect(source.ranges.every((range) => range.end - range.start <= 256n * 1024n)).toBe(true)
    expect(source.ranges.some((range) => range.start === 0n && range.end === BigInt(bytes.byteLength)))
      .toBe(false)
  })

  it('rejects invalid top-level geometry before media decode', async () => {
    const bytes = new Uint8Array(1_024)
    box(bytes, 0, 24, 'ftyp')
    box(bytes, 24, 2_000, 'mdat')
    const source = new RecordedSource(bytes)
    await expect(V2Mp4RangePreview.open(source, new AbortController().signal))
      .rejects.toThrow('invalid size')
  })
})
