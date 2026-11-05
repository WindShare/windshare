export const IMAGE_HEADER_BYTES = 64 * 1024
export const IMAGE_FILE_BYTES = 64 * 1024 * 1024
export const IMAGE_MAXIMUM_DIMENSION = 16_384
export const IMAGE_DECODED_BYTES = 128 * 1024 * 1024
// Bounds the serial JPEG header reads (at most 4 MiB) that a file of many
// tiny segments could force. Real metadata needs far fewer: a Google Cardboard
// photo carrying its right-eye image and audio in XMP puts the frame header
// 2.8 MiB in, 24 windows deep.
export const JPEG_HEADER_WINDOWS = 64

const PNG_SIGNATURE = Uint8Array.of(137, 80, 78, 71, 13, 10, 26, 10)
const JPEG_START = Uint8Array.of(0xff, 0xd8)
const RIFF = 'RIFF'
const WEBP = 'WEBP'

export interface ImageHeader {
  readonly mimeType: 'image/png' | 'image/jpeg' | 'image/webp'
  readonly width: number
  readonly height: number
}

export class ImagePreviewError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'ImagePreviewError'
  }
}

// Camera JPEGs routinely place more than one sniff window of metadata (EXIF
// previews, MPF, ICC, XMP) before the frame header. Segments are
// length-prefixed, so the caller resumes the walk at `offset` with another
// bounded window instead of reading the skipped payloads.
export class JpegHeaderContinuation {
  readonly offset: number
  /** Sniff windows already walked, including the initial header window. */
  readonly windows: number

  constructor(offset: number, windows: number) {
    this.offset = offset
    this.windows = windows
    Object.freeze(this)
  }
}

export function sniffImageHeader(
  bytes: Uint8Array,
  exactSize: bigint,
): ImageHeader | JpegHeaderContinuation | undefined {
  if (startsWith(bytes, PNG_SIGNATURE)) return validateDimensions(parsePng(bytes), exactSize)
  if (startsWith(bytes, JPEG_START)) return walkJpeg(bytes, undefined, JPEG_START.byteLength, exactSize)
  if (ascii(bytes, 0, 4) === RIFF && ascii(bytes, 8, 12) === WEBP) {
    return validateDimensions(parseWebp(bytes, exactSize), exactSize)
  }
  return undefined
}

function parsePng(bytes: Uint8Array): ImageHeader {
  if (bytes.byteLength < 33 || ascii(bytes, 12, 16) !== 'IHDR' || uint32(bytes, 8) !== 13) {
    throw new ImagePreviewError('PNG does not begin with a canonical IHDR chunk')
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
    if (type === 'acTL') throw new ImagePreviewError('Animated PNG preview is not supported')
    if (type === 'IDAT') {
      reachedData = true
      break
    }
    const end = offset + 12 + length
    if (!Number.isSafeInteger(end) || end > bytes.byteLength) break
    if (type === 'IEND') throw new ImagePreviewError('PNG ended before image data')
    offset = end
  }
  if (!reachedData) {
    throw new ImagePreviewError('PNG metadata exceeds the bounded header sniff')
  }
  return { mimeType: 'image/png', width, height }
}

/** Continues a JPEG header walk; `window` holds the file bytes from `continuation.offset`. */
export function continueJpegHeader(
  window: Uint8Array,
  continuation: JpegHeaderContinuation,
  exactSize: bigint,
): ImageHeader | JpegHeaderContinuation {
  return walkJpeg(window, continuation, 0, exactSize)
}

const JPEG_ENDED = 'JPEG ended before its frame header'

// Offsets are relative to `window`, which holds the file from `from.offset`.
function walkJpeg(
  window: Uint8Array,
  from: JpegHeaderContinuation | undefined,
  start: number,
  exactSize: bigint,
): ImageHeader | JpegHeaderContinuation {
  // Walking a file the preview would reject anyway must not cost range reads.
  requirePreviewFileSize(exactSize)
  const fileEnd = Number(exactSize)
  const windowStart = from?.offset ?? 0
  const windowEnd = windowStart + window.byteLength
  const windows = (from?.windows ?? 0) + 1
  let offset = start
  for (;;) {
    const segment = nextJpegSegment(window, offset, fileEnd - windowStart)
    if (segment === undefined) throw new ImagePreviewError(JPEG_ENDED)
    if (segment === 'window-end') {
      return resumeJpeg(windowStart + resumableOffset(window, offset), windowEnd, windows, fileEnd)
    }
    if (isJpegFrameMarker(segment.marker)) return jpegFrameHeader(window, segment, exactSize)
    offset = segment.next
    if (offset >= window.byteLength) return resumeJpeg(windowStart + offset, windowEnd, windows, fileEnd)
  }
}

// A window that reaches the end of the file cannot be resumed.
function resumeJpeg(
  offset: number,
  windowEnd: number,
  windows: number,
  fileEnd: number,
): JpegHeaderContinuation {
  if (windowEnd >= fileEnd || offset >= fileEnd) throw new ImagePreviewError(JPEG_ENDED)
  if (windows >= JPEG_HEADER_WINDOWS) throw new ImagePreviewError('JPEG metadata exceeds the preview limit')
  return new JpegHeaderContinuation(offset, windows)
}

function jpegFrameHeader(window: Uint8Array, segment: JpegSegment, exactSize: bigint): ImageHeader {
  if (segment.length < 8) throw new ImagePreviewError('JPEG frame header is truncated')
  return validateDimensions({
    mimeType: 'image/jpeg',
    width: uint16(window, segment.header + 5),
    height: uint16(window, segment.header + 3),
  }, exactSize)
}

// Resuming at the marker keeps a segment whose header straddles the window
// intact. A fill-byte run reaching the window end resumes at its last byte.
// Windows short of the file end are full, so either choice makes progress.
function resumableOffset(window: Uint8Array, offset: number): number {
  let marker = offset
  while (marker < window.byteLength && window[marker] === 0xff) marker += 1
  return Math.max(offset, marker - 1)
}

interface JpegSegment {
  readonly marker: number
  readonly header: number
  readonly length: number
  readonly next: number
}

// Segment payloads are outside the sniff budget, like PNG IDAT and WebP image
// chunks: only the marker, length and a frame header must be in the window.
function nextJpegSegment(
  bytes: Uint8Array,
  start: number,
  fileEnd: number,
): JpegSegment | 'window-end' | undefined {
  let offset = start
  while (bytes[offset] === 0xff) offset += 1
  const marker = bytes[offset]
  offset += 1
  if (marker === undefined) return 'window-end'
  if (marker === 0xd9 || marker === 0xda) return undefined
  if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) {
    return { marker, header: offset, length: 0, next: offset }
  }
  if (offset + 2 > bytes.byteLength) return 'window-end'
  const length = uint16(bytes, offset)
  if (length < 2 || offset + length > fileEnd) return undefined
  if (isJpegFrameMarker(marker) && offset + Math.min(length, 8) > bytes.byteLength) return 'window-end'
  return { marker, header: offset, length, next: offset + length }
}

function parseWebp(bytes: Uint8Array, exactSize: bigint): ImageHeader {
  if (bytes.byteLength < 30) throw new ImagePreviewError('WebP header is truncated')
  const declared = BigInt(uint32LittleEndian(bytes, 4)) + 8n
  if (declared !== exactSize) throw new ImagePreviewError('WebP RIFF length does not match the file')
  let offset = 12
  while (offset < bytes.byteLength) {
    const chunk = nextWebpChunk(bytes, offset, exactSize)
    if (chunk === undefined) break
    if (chunk.type === 'ANIM' || chunk.type === 'ANMF') {
      throw new ImagePreviewError('Animated WebP preview is not supported')
    }
    const header = decodeWebpImageChunk(bytes, chunk)
    if (header !== undefined) return header
    offset = chunk.next
  }
  throw new ImagePreviewError('WebP dimensions exceed the bounded header sniff')
}

interface WebpChunk {
  readonly type: string
  readonly length: number
  readonly data: number
  readonly next: number
}

// Image chunk payloads are intentionally outside the sniff budget, like PNG
// IDAT: a simple-format VP8/VP8L chunk spans the whole bitstream, but its
// dimensions sit in the first payload bytes. Bound chunks by the file instead.
function nextWebpChunk(bytes: Uint8Array, offset: number, exactSize: bigint): WebpChunk | undefined {
  if (offset + 8 > bytes.byteLength) return undefined
  const length = uint32LittleEndian(bytes, offset + 4)
  const data = offset + 8
  const next = data + length + (length & 1)
  if (!Number.isSafeInteger(next) || BigInt(next) > exactSize) return undefined
  return { type: ascii(bytes, offset, offset + 4), length, data, next }
}

function decodeWebpImageChunk(bytes: Uint8Array, chunk: WebpChunk): ImageHeader | undefined {
  if (chunk.type === 'VP8X') return decodeWebpExtended(bytes, chunk)
  if (chunk.type === 'VP8 ') return decodeWebpLossy(bytes, chunk)
  if (chunk.type === 'VP8L') return decodeWebpLossless(bytes, chunk)
  return undefined
}

function decodeWebpExtended(bytes: Uint8Array, chunk: WebpChunk): ImageHeader | undefined {
  if (chunk.length < 10 || chunk.data + 10 > bytes.byteLength) return undefined
  if (((bytes[chunk.data] ?? 0) & 0x02) !== 0) {
    throw new ImagePreviewError('Animated WebP preview is not supported')
  }
  return {
    mimeType: 'image/webp',
    width: uint24LittleEndian(bytes, chunk.data + 4) + 1,
    height: uint24LittleEndian(bytes, chunk.data + 7) + 1,
  }
}

function decodeWebpLossy(bytes: Uint8Array, chunk: WebpChunk): ImageHeader | undefined {
  if (chunk.length < 10 || chunk.data + 10 > bytes.byteLength ||
      bytes[chunk.data + 3] !== 0x9d || bytes[chunk.data + 4] !== 0x01 ||
      bytes[chunk.data + 5] !== 0x2a) return undefined
  return {
    mimeType: 'image/webp',
    width: uint16LittleEndian(bytes, chunk.data + 6) & 0x3fff,
    height: uint16LittleEndian(bytes, chunk.data + 8) & 0x3fff,
  }
}

function decodeWebpLossless(bytes: Uint8Array, chunk: WebpChunk): ImageHeader | undefined {
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

function requirePreviewFileSize(exactSize: bigint): void {
  if (exactSize <= 0n || exactSize > BigInt(IMAGE_FILE_BYTES)) {
    throw new ImagePreviewError('Image encoded size exceeds the preview limit')
  }
}

function validateDimensions(header: ImageHeader, exactSize: bigint): ImageHeader {
  requirePreviewFileSize(exactSize)
  if (
    !Number.isSafeInteger(header.width) || !Number.isSafeInteger(header.height) ||
    header.width <= 0 || header.height <= 0 ||
    header.width > IMAGE_MAXIMUM_DIMENSION || header.height > IMAGE_MAXIMUM_DIMENSION ||
    BigInt(header.width) * BigInt(header.height) * 4n > BigInt(IMAGE_DECODED_BYTES)
  ) {
    throw new ImagePreviewError('Image dimensions exceed the decoded-memory limit')
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
