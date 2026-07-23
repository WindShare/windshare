export const V2_IMAGE_HEADER_BYTES = 64 * 1024
export const V2_IMAGE_FILE_BYTES = 64 * 1024 * 1024
export const V2_IMAGE_MAXIMUM_DIMENSION = 16_384
export const V2_IMAGE_DECODED_BYTES = 128 * 1024 * 1024

const PNG_SIGNATURE = Uint8Array.of(137, 80, 78, 71, 13, 10, 26, 10)
const JPEG_START = Uint8Array.of(0xff, 0xd8)
const RIFF = 'RIFF'
const WEBP = 'WEBP'

export interface V2ImageHeader {
  readonly mimeType: 'image/png' | 'image/jpeg' | 'image/webp'
  readonly width: number
  readonly height: number
}

export class V2ImagePreviewError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2ImagePreviewError'
  }
}

export function sniffV2ImageHeader(bytes: Uint8Array, exactSize: bigint): V2ImageHeader | undefined {
  if (startsWith(bytes, PNG_SIGNATURE)) return validateDimensions(parsePng(bytes), exactSize)
  if (startsWith(bytes, JPEG_START)) return validateDimensions(parseJpeg(bytes), exactSize)
  if (ascii(bytes, 0, 4) === RIFF && ascii(bytes, 8, 12) === WEBP) {
    return validateDimensions(parseWebp(bytes, exactSize), exactSize)
  }
  return undefined
}

function parsePng(bytes: Uint8Array): V2ImageHeader {
  if (bytes.byteLength < 33 || ascii(bytes, 12, 16) !== 'IHDR' || uint32(bytes, 8) !== 13) {
    throw new V2ImagePreviewError('PNG does not begin with a canonical IHDR chunk')
  }
  const width = uint32(bytes, 16)
  const height = uint32(bytes, 20)
  let offset = 8
  let reachedData = false
  while (offset + 8 <= bytes.byteLength) {
    const length = uint32(bytes, offset)
    const type = ascii(bytes, offset + 4, offset + 8)
    // IDAT payloads are intentionally outside the sniff budget; its chunk header
    // is sufficient to prove bounded metadata has ended.
    if (type === 'acTL') throw new V2ImagePreviewError('Animated PNG preview is not supported')
    if (type === 'IDAT') {
      reachedData = true
      break
    }
    const end = offset + 12 + length
    if (!Number.isSafeInteger(end) || end > bytes.byteLength) break
    if (type === 'IEND') throw new V2ImagePreviewError('PNG ended before image data')
    offset = end
  }
  if (!reachedData) {
    throw new V2ImagePreviewError('PNG metadata exceeds the bounded header sniff')
  }
  return { mimeType: 'image/png', width, height }
}

function parseJpeg(bytes: Uint8Array): V2ImageHeader {
  let offset = 2
  while (offset < bytes.byteLength) {
    const segment = nextJpegSegment(bytes, offset)
    if (segment === undefined) break
    offset = segment.next
    if (isJpegFrameMarker(segment.marker)) {
      if (segment.length < 8) throw new V2ImagePreviewError('JPEG frame header is truncated')
      return {
        mimeType: 'image/jpeg',
        width: uint16(bytes, segment.header + 5),
        height: uint16(bytes, segment.header + 3),
      }
    }
  }
  throw new V2ImagePreviewError('JPEG dimensions exceed the bounded header sniff')
}

interface JpegSegment {
  readonly marker: number
  readonly header: number
  readonly length: number
  readonly next: number
}

function nextJpegSegment(bytes: Uint8Array, start: number): JpegSegment | undefined {
  let offset = start
  while (bytes[offset] === 0xff) offset += 1
  const marker = bytes[offset]
  offset += 1
  if (marker === undefined || marker === 0xd9 || marker === 0xda) return undefined
  if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) {
    return { marker, header: offset, length: 0, next: offset }
  }
  if (offset + 2 > bytes.byteLength) return undefined
  const length = uint16(bytes, offset)
  if (length < 2 || offset + length > bytes.byteLength) return undefined
  return { marker, header: offset, length, next: offset + length }
}

function parseWebp(bytes: Uint8Array, exactSize: bigint): V2ImageHeader {
  if (bytes.byteLength < 30) throw new V2ImagePreviewError('WebP header is truncated')
  const declared = BigInt(uint32LittleEndian(bytes, 4)) + 8n
  if (declared !== exactSize) throw new V2ImagePreviewError('WebP RIFF length does not match the file')
  let offset = 12
  while (offset < bytes.byteLength) {
    const chunk = nextWebpChunk(bytes, offset)
    if (chunk === undefined) break
    if (chunk.type === 'ANIM' || chunk.type === 'ANMF') {
      throw new V2ImagePreviewError('Animated WebP preview is not supported')
    }
    const header = decodeWebpImageChunk(bytes, chunk)
    if (header !== undefined) return header
    offset = chunk.next
  }
  throw new V2ImagePreviewError('WebP dimensions exceed the bounded header sniff')
}

interface WebpChunk {
  readonly type: string
  readonly length: number
  readonly data: number
  readonly next: number
}

function nextWebpChunk(bytes: Uint8Array, offset: number): WebpChunk | undefined {
  if (offset + 8 > bytes.byteLength) return undefined
  const length = uint32LittleEndian(bytes, offset + 4)
  const data = offset + 8
  const next = data + length + (length & 1)
  if (!Number.isSafeInteger(next) || next > bytes.byteLength) return undefined
  return { type: ascii(bytes, offset, offset + 4), length, data, next }
}

function decodeWebpImageChunk(bytes: Uint8Array, chunk: WebpChunk): V2ImageHeader | undefined {
  if (chunk.type === 'VP8X') return decodeWebpExtended(bytes, chunk)
  if (chunk.type === 'VP8 ') return decodeWebpLossy(bytes, chunk)
  if (chunk.type === 'VP8L') return decodeWebpLossless(bytes, chunk)
  return undefined
}

function decodeWebpExtended(bytes: Uint8Array, chunk: WebpChunk): V2ImageHeader | undefined {
  if (chunk.length < 10 || chunk.data + 10 > bytes.byteLength) return undefined
  if (((bytes[chunk.data] ?? 0) & 0x02) !== 0) {
    throw new V2ImagePreviewError('Animated WebP preview is not supported')
  }
  return {
    mimeType: 'image/webp',
    width: uint24LittleEndian(bytes, chunk.data + 4) + 1,
    height: uint24LittleEndian(bytes, chunk.data + 7) + 1,
  }
}

function decodeWebpLossy(bytes: Uint8Array, chunk: WebpChunk): V2ImageHeader | undefined {
  if (chunk.length < 10 || chunk.data + 10 > bytes.byteLength ||
      bytes[chunk.data + 3] !== 0x9d || bytes[chunk.data + 4] !== 0x01 ||
      bytes[chunk.data + 5] !== 0x2a) return undefined
  return {
    mimeType: 'image/webp',
    width: uint16LittleEndian(bytes, chunk.data + 6) & 0x3fff,
    height: uint16LittleEndian(bytes, chunk.data + 8) & 0x3fff,
  }
}

function decodeWebpLossless(bytes: Uint8Array, chunk: WebpChunk): V2ImageHeader | undefined {
  if (chunk.length < 5 || chunk.data + 5 > bytes.byteLength || bytes[chunk.data] !== 0x2f) {
    return undefined
  }
  const bits = uint32LittleEndian(bytes, chunk.data + 1)
  return {
    mimeType: 'image/webp',
    width: (bits & 0x3fff) + 1,
    height: ((bits >>> 14) & 0x3fff) + 1,
  }
}

function validateDimensions(header: V2ImageHeader, exactSize: bigint): V2ImageHeader {
  if (exactSize <= 0n || exactSize > BigInt(V2_IMAGE_FILE_BYTES)) {
    throw new V2ImagePreviewError('Image encoded size exceeds the preview limit')
  }
  if (
    !Number.isSafeInteger(header.width) || !Number.isSafeInteger(header.height) ||
    header.width <= 0 || header.height <= 0 ||
    header.width > V2_IMAGE_MAXIMUM_DIMENSION || header.height > V2_IMAGE_MAXIMUM_DIMENSION ||
    BigInt(header.width) * BigInt(header.height) * 4n > BigInt(V2_IMAGE_DECODED_BYTES)
  ) {
    throw new V2ImagePreviewError('Image dimensions exceed the decoded-memory limit')
  }
  return Object.freeze(header)
}

function isJpegFrameMarker(marker: number): boolean {
  return (marker >= 0xc0 && marker <= 0xc3) ||
    (marker >= 0xc5 && marker <= 0xc7) ||
    (marker >= 0xc9 && marker <= 0xcb) ||
    (marker >= 0xcd && marker <= 0xcf)
}

function startsWith(bytes: Uint8Array, prefix: Uint8Array): boolean {
  return prefix.every((value, index) => bytes[index] === value)
}

function ascii(bytes: Uint8Array, start: number, end: number): string {
  return String.fromCharCode(...bytes.subarray(start, end))
}

function uint16(bytes: Uint8Array, offset: number): number {
  return ((bytes[offset] ?? 0) << 8) | (bytes[offset + 1] ?? 0)
}

function uint16LittleEndian(bytes: Uint8Array, offset: number): number {
  return (bytes[offset] ?? 0) | ((bytes[offset + 1] ?? 0) << 8)
}

function uint24LittleEndian(bytes: Uint8Array, offset: number): number {
  return (bytes[offset] ?? 0) |
    ((bytes[offset + 1] ?? 0) << 8) |
    ((bytes[offset + 2] ?? 0) << 16)
}

function uint32(bytes: Uint8Array, offset: number): number {
  return new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0, false)
}

function uint32LittleEndian(bytes: Uint8Array, offset: number): number {
  return new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0, true)
}
