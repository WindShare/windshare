import { describe, expect, it } from 'vitest'

import {
  continueJpegHeader,
  sniffImageHeader,
  IMAGE_DECODED_BYTES,
  IMAGE_FILE_BYTES,
  IMAGE_HEADER_BYTES,
  ImagePreviewError,
  JPEG_HEADER_WINDOWS,
  JpegHeaderContinuation,
} from '../../src/preview/image-header'

function writeUint32(bytes: Uint8Array, offset: number, value: number): void {
  new DataView(bytes.buffer).setUint32(offset, value, false)
}

function png(width: number, height: number, animated = false): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(animated ? 53 : 41)
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10])
  writeUint32(bytes, 8, 13)
  bytes.set([73, 72, 68, 82], 12)
  writeUint32(bytes, 16, width)
  writeUint32(bytes, 20, height)
  let offset = 33
  if (animated) {
    writeUint32(bytes, offset, 0)
    bytes.set([97, 99, 84, 76], offset + 4)
    offset += 12
  }
  // The IDAT body is intentionally absent: header sniffing must not require it.
  writeUint32(bytes, offset, 1_000_000)
  bytes.set([73, 68, 65, 84], offset + 4)
  return bytes
}

function webpAnimated(): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(30)
  bytes.set([82, 73, 70, 70])
  new DataView(bytes.buffer).setUint32(4, 22, true)
  bytes.set([87, 69, 66, 80, 86, 80, 56, 88], 8)
  new DataView(bytes.buffer).setUint32(16, 10, true)
  bytes[20] = 0x02
  return bytes
}

// Simple-format WebP stores the whole bitstream in its first chunk, so only the
// bounded prefix is materialized, as the preview runtime does.
function simpleWebpPrefix(kind: 'VP8 ' | 'VP8L', exactSize: number, chunkLength = exactSize - 20) {
  const bytes = new Uint8Array(IMAGE_HEADER_BYTES)
  const view = new DataView(bytes.buffer)
  bytes.set([82, 73, 70, 70])
  view.setUint32(4, exactSize - 8, true)
  bytes.set([87, 69, 66, 80, ...Array.from(kind, (char) => char.charCodeAt(0))], 8)
  view.setUint32(16, chunkLength, true)
  if (kind === 'VP8 ') {
    bytes.set([0x9d, 0x01, 0x2a], 23)
    view.setUint16(26, 1024, true)
    view.setUint16(28, 768, true)
  } else {
    bytes[20] = 0x2f
    view.setUint32(21, (1024 - 1) | ((768 - 1) << 14), true)
  }
  return { bytes, exactSize: BigInt(exactSize) }
}

const JPEG_MAXIMUM_SEGMENT = 0xffff

// Camera JPEGs carry EXIF previews, MPF and ICC segments ahead of the frame.
function jpegFile(metadataLengths: readonly number[], fill = 0): Uint8Array<ArrayBuffer> {
  const metadata = metadataLengths.reduce((total, length) => total + 2 + length, 0)
  const bytes = new Uint8Array(2 + metadata + fill + 2 + 17 + 2)
  const view = new DataView(bytes.buffer)
  bytes.set([0xff, 0xd8])
  let offset = 2
  for (const length of metadataLengths) {
    bytes.set([0xff, 0xe1], offset)
    view.setUint16(offset + 2, length, false)
    offset += 2 + length
  }
  bytes.fill(0xff, offset, offset + fill)
  offset += fill
  bytes.set([0xff, 0xc0], offset)
  view.setUint16(offset + 2, 17, false)
  view.setUint16(offset + 5, 768, false)
  view.setUint16(offset + 7, 1024, false)
  bytes.set([0xff, 0xda], offset + 19)
  return bytes
}

// Mirrors the preview runtime: each step reads one bounded window.
function sniffJpeg(file: Uint8Array, exactSize = BigInt(file.byteLength)) {
  let sniffed = sniffImageHeader(file.subarray(0, IMAGE_HEADER_BYTES), exactSize)
  const windows = [0]
  while (sniffed instanceof JpegHeaderContinuation) {
    windows.push(sniffed.offset)
    const window = file.subarray(sniffed.offset, sniffed.offset + IMAGE_HEADER_BYTES)
    sniffed = continueJpegHeader(window, sniffed, exactSize)
  }
  return { header: sniffed, windows }
}

describe('bounded image header policy', () => {
  it('accepts a static PNG without reading its encoded payload', () => {
    expect(sniffImageHeader(png(320, 200), 1_000_041n)).toEqual({
      mimeType: 'image/png',
      width: 320,
      height: 200,
    })
  })

  it('rejects animation and decoded-memory bombs before browser decode', () => {
    expect(() => sniffImageHeader(png(10, 10, true), 53n))
      .toThrowError(new ImagePreviewError('Animated PNG preview is not supported'))
    const side = Math.floor(Math.sqrt(IMAGE_DECODED_BYTES / 4)) + 1
    expect(() => sniffImageHeader(png(side, side), 41n))
      .toThrow('decoded-memory limit')
    expect(() => sniffImageHeader(webpAnimated(), 30n))
      .toThrow('Animated WebP')
  })

  it('rejects a WebP whose authenticated file size disagrees with RIFF', () => {
    expect(() => sniffImageHeader(webpAnimated(), 31n)).toThrow('RIFF length')
  })

  it('reads simple-format WebP dimensions when the bitstream exceeds the sniff', () => {
    const exactSize = 4 * IMAGE_HEADER_BYTES
    for (const kind of ['VP8 ', 'VP8L'] as const) {
      // An odd chunk length ends with a RIFF pad byte at the end of the file.
      for (const chunkLength of [exactSize - 20, exactSize - 21]) {
        const { bytes } = simpleWebpPrefix(kind, exactSize, chunkLength)
        expect(sniffImageHeader(bytes, BigInt(exactSize)))
          .toEqual({ mimeType: 'image/webp', width: 1024, height: 768 })
      }
    }
  })

  it('rejects a simple-format WebP without a bitstream signature', () => {
    for (const [kind, signatureOffset] of [['VP8 ', 23], ['VP8L', 20]] as const) {
      const { bytes, exactSize } = simpleWebpPrefix(kind, 4 * IMAGE_HEADER_BYTES)
      bytes[signatureOffset] = 0
      expect(() => sniffImageHeader(bytes, exactSize)).toThrow(ImagePreviewError)
    }
  })

  it('rejects a WebP chunk that claims bytes beyond the file', () => {
    const exactSize = 4 * IMAGE_HEADER_BYTES
    const { bytes } = simpleWebpPrefix('VP8 ', exactSize, exactSize)
    expect(() => sniffImageHeader(bytes, BigInt(exactSize))).toThrow('bounded header sniff')
  })

  it('resumes a JPEG walk past metadata segments larger than the sniff', () => {
    const { header, windows } = sniffJpeg(jpegFile([JPEG_MAXIMUM_SEGMENT, JPEG_MAXIMUM_SEGMENT]))
    expect(header).toEqual({ mimeType: 'image/jpeg', width: 1024, height: 768 })
    // Segment payloads are skipped by length, never read.
    expect(windows).toEqual([0, 2 + 2 + JPEG_MAXIMUM_SEGMENT, 2 + 2 * (2 + JPEG_MAXIMUM_SEGMENT)])
  })

  it('reads a JPEG frame header that straddles a sniff window boundary', () => {
    const frameStart = IMAGE_HEADER_BYTES - 12
    for (let shift = 0; shift <= 12; shift += 1) {
      const { header } = sniffJpeg(jpegFile([frameStart + shift - 4]))
      expect(header).toEqual({ mimeType: 'image/jpeg', width: 1024, height: 768 })
    }
    // Fill bytes before a marker may also cross the boundary.
    for (const fill of [1, 12, 13, 14]) {
      const { header } = sniffJpeg(jpegFile([frameStart - 4], fill))
      expect(header).toEqual({ mimeType: 'image/jpeg', width: 1024, height: 768 })
    }
  })

  it('rejects a JPEG segment that claims bytes beyond the file', () => {
    const file = jpegFile([JPEG_MAXIMUM_SEGMENT])
    const truncated = file.subarray(0, JPEG_MAXIMUM_SEGMENT)
    expect(() => sniffJpeg(truncated)).toThrow('JPEG ended before its frame header')
    expect(() => sniffJpeg(file.subarray(0, file.byteLength - 21)))
      .toThrow('JPEG ended before its frame header')
  })

  it('caps the windows a JPEG header walk may read', () => {
    // Each maximum-size segment ends past its window, so it costs one read.
    const atLimit = jpegFile(Array<number>(JPEG_HEADER_WINDOWS - 1).fill(JPEG_MAXIMUM_SEGMENT))
    const { header, windows } = sniffJpeg(atLimit)
    expect(header).toEqual({ mimeType: 'image/jpeg', width: 1024, height: 768 })
    expect(windows).toHaveLength(JPEG_HEADER_WINDOWS)
    const overLimit = jpegFile(Array<number>(JPEG_HEADER_WINDOWS).fill(JPEG_MAXIMUM_SEGMENT))
    expect(() => sniffJpeg(overLimit)).toThrow('JPEG metadata exceeds the preview limit')
    // Tiny segments cannot force a serial read of every window in the file.
    const tiny = jpegFile(Array<number>(((JPEG_HEADER_WINDOWS + 1) * IMAGE_HEADER_BYTES) / 4).fill(2))
    expect(tiny.byteLength).toBeLessThan(IMAGE_FILE_BYTES)
    expect(() => sniffJpeg(tiny)).toThrow('JPEG metadata exceeds the preview limit')
  })

  it('rejects an oversized JPEG before resuming its header walk', () => {
    const file = jpegFile([JPEG_MAXIMUM_SEGMENT])
    expect(() => sniffImageHeader(file.subarray(0, IMAGE_HEADER_BYTES), BigInt(IMAGE_FILE_BYTES + 1)))
      .toThrow('encoded size exceeds')
  })
})
