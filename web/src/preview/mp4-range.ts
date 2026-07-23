import type { ISOFile, Movie, MP4BoxBuffer, Sample, Track } from 'mp4box'

import { bigintToSafeNumber, byteRange, type ByteRange } from '../content/geometry'

export const V2_VIDEO_EDGE_METADATA_BYTES = 256 * 1024
export const V2_VIDEO_METADATA_BYTES = 4 * 1024 * 1024
export const V2_VIDEO_SAMPLE_BYTES = 8 * 1024 * 1024
export const V2_VIDEO_DECODED_BYTES = 128 * 1024 * 1024
export const V2_VIDEO_MAXIMUM_DIMENSION = 8_192
export const V2_VIDEO_MAXIMUM_SECONDS = 24 * 60 * 60
export const V2_VIDEO_MAXIMUM_SAMPLES = 1_000_000

const TOP_LEVEL_BOX_LIMIT = 64
const MP4_HEADER_BYTES = 16
const SUPPORTED_VIDEO_CODEC = /^(?:avc1|avc3|hvc1|hev1|vp09|av01)\./u

export interface V2PreviewRangeSource {
  readonly exactSize: bigint
  read(range: ByteRange, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>>
}

export interface V2Mp4Metadata {
  readonly durationSeconds: number
  readonly width: number
  readonly height: number
  readonly codec: string
  readonly mimeType: string
}

export interface V2Mp4Segment {
  readonly bytes: Blob
  readonly positionSeconds: number
}

interface MetadataPart {
  readonly start: number
  readonly data: Uint8Array<ArrayBuffer>
}

interface BoxLocation {
  readonly start: number
  readonly end: number
}

interface TopLevelBox extends BoxLocation {
  readonly type: string
}

interface ParsedMetadata {
  readonly metadata: V2Mp4Metadata
  readonly trackId: number
  readonly parts: readonly MetadataPart[]
}

export class V2VideoPreviewError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2VideoPreviewError'
  }
}

export class V2Mp4RangePreview {
  readonly metadata: V2Mp4Metadata
  readonly #source: V2PreviewRangeSource
  readonly #trackId: number
  readonly #parts: readonly MetadataPart[]

  private constructor(source: V2PreviewRangeSource, parsed: ParsedMetadata) {
    this.#source = source
    this.metadata = parsed.metadata
    this.#trackId = parsed.trackId
    this.#parts = parsed.parts
  }

  static async open(source: V2PreviewRangeSource, signal: AbortSignal): Promise<V2Mp4RangePreview> {
    signal.throwIfAborted()
    if (source.exactSize < 16n) throw new V2VideoPreviewError('MP4 file is too small')
    const parsed = await parseMetadata(source, signal)
    return new V2Mp4RangePreview(source, parsed)
  }

  async segmentAt(seconds: number, signal: AbortSignal): Promise<V2Mp4Segment> {
    signal.throwIfAborted()
    if (!Number.isFinite(seconds) || seconds < 0 || seconds > this.metadata.durationSeconds) {
      throw new V2VideoPreviewError('Video seek is outside the authenticated duration')
    }
    const file = await metadataFile(this.#parts)
    const info = requireMovie(file)
    const track = requireTrack(info, this.#trackId)
    file.setSegmentOptions(track.id, track.id, {
      nbSamples: 1,
      nbSamplesPerFragment: 1,
      rapAlignement: false,
    })
    const initialization = file.initializeSegmentation('per-track')
      .find((candidate) => candidate.id === track.id)
    if (initialization === undefined || initialization.buffer.byteLength > V2_VIDEO_METADATA_BYTES) {
      throw new V2VideoPreviewError('MP4 initialization segment exceeds the preview limit')
    }
    file.seek(seconds, true)
    const fragmented = file.fragmentedTracks.find((candidate) => candidate.id === track.id)
    const samples = file.getTrackSamplesInfo(track.id)
    const sampleIndex = fragmented?.state.lastFragmentSampleNumber ?? -1
    const sample = samples[sampleIndex]
    requireSample(sample, this.#source.exactSize)
    const sampleData = await this.#source.read(
      byteRange(BigInt(sample.offset), BigInt(sample.offset + sample.size)),
      signal,
    )
    if (sampleData.byteLength !== sample.size) {
      throw new V2VideoPreviewError('Video sample range was not read exactly')
    }
    const segmentController = new AbortController()
    const unlink = forwardAbort(signal, segmentController)
    const segmentPromise = nextSegment(file, track.id, segmentController.signal)
    try {
      file.start()
      appendPart(file, { start: sample.offset, data: sampleData })
      file.flush()
      const segment = await segmentPromise
      const total = initialization.buffer.byteLength + segment.byteLength
      if (total > V2_VIDEO_METADATA_BYTES + V2_VIDEO_SAMPLE_BYTES) {
        throw new V2VideoPreviewError('Video preview segment exceeds its memory limit')
      }
      return Object.freeze({
        bytes: new Blob([initialization.buffer, segment], { type: 'video/mp4' }),
        positionSeconds: sample.cts / sample.timescale,
      })
    } finally {
      unlink()
      segmentController.abort(new DOMException('Video segment operation settled', 'AbortError'))
      file.stop()
      await segmentPromise.catch(() => undefined)
    }
  }
}

async function parseMetadata(
  source: V2PreviewRangeSource,
  signal: AbortSignal,
): Promise<ParsedMetadata> {
  const size = bigintToSafeNumber(source.exactSize, 'video file size')
  const headEnd = Math.min(size, V2_VIDEO_EDGE_METADATA_BYTES)
  const tailStart = Math.max(0, size - V2_VIDEO_EDGE_METADATA_BYTES)
  const head = await source.read(byteRange(0n, BigInt(headEnd)), signal)
  const tail = tailStart === 0
    ? head
    : await source.read(byteRange(BigInt(tailStart), BigInt(size)), signal)
  const windows = [
    { start: 0, data: head },
    ...(tailStart === 0 ? [] : [{ start: tailStart, data: tail }]),
  ]
  const moov = await locateMoov(source, windows, signal)
  if (moov.end - moov.start > V2_VIDEO_METADATA_BYTES) {
    throw new V2VideoPreviewError('MP4 metadata exceeds the preview limit')
  }
  const moovData = contains(windows[0]!, moov)
    ? windows[0]!.data.slice(moov.start, moov.end)
    : await source.read(byteRange(BigInt(moov.start), BigInt(moov.end)), signal)
  const prefixEnd = Math.min(head.byteLength, moov.start)
  const parts: MetadataPart[] = contains(windows[0]!, moov)
    ? [{ start: 0, data: head }]
    : [
        ...(prefixEnd === 0 ? [] : [{ start: 0, data: head.slice(0, prefixEnd) }]),
        { start: moov.start, data: moovData },
      ]
  const file = await metadataFile(parts)
  const info = requireMovie(file)
  const track = requireVideoTrack(info)
  const samples = file.getTrackSamplesInfo(track.id)
  validateTrack(track, samples, source.exactSize)
  const durationSeconds = track.movie_duration / track.movie_timescale
  return Object.freeze({
    trackId: track.id,
    parts: Object.freeze(parts.map((part) => Object.freeze({
      start: part.start,
      data: part.data.slice(),
    }))),
    metadata: Object.freeze({
      durationSeconds,
      width: track.video!.width,
      height: track.video!.height,
      codec: track.codec,
      mimeType: `video/mp4; codecs="${track.codec}"`,
    }),
  })
}

async function locateMoov(
  source: V2PreviewRangeSource,
  windows: readonly MetadataPart[],
  signal: AbortSignal,
): Promise<BoxLocation> {
  const size = bigintToSafeNumber(source.exactSize, 'video file size')
  let offset = 0
  let sawFtyp = false
  for (let count = 0; count < TOP_LEVEL_BOX_LIMIT && offset < size; count += 1) {
    const header = await bytesAt(source, windows, offset, Math.min(size, offset + MP4_HEADER_BYTES), signal)
    const box = decodeTopLevelBox(header, offset, size)
    if (count === 0 && (box.type !== 'ftyp' || box.end - box.start < 16)) {
      throw new V2VideoPreviewError('MP4 does not begin with a valid file type box')
    }
    if (box.type === 'ftyp') sawFtyp = true
    if (box.type === 'moov') {
      if (!sawFtyp) throw new V2VideoPreviewError('MP4 metadata precedes its file type box')
      return { start: box.start, end: box.end }
    }
    offset = box.end
  }
  throw new V2VideoPreviewError('MP4 metadata was not found within the bounded box scan')
}

function decodeTopLevelBox(header: Uint8Array, offset: number, fileSize: number): TopLevelBox {
  if (header.byteLength < 8) throw new V2VideoPreviewError('MP4 top-level box header is truncated')
  const size32 = uint32(header, 0)
  let headerSize = 8
  let boxSize = size32 === 0 ? fileSize - offset : size32
  if (size32 === 1) {
    if (header.byteLength < 16) throw new V2VideoPreviewError('MP4 extended box header is truncated')
    headerSize = 16
    boxSize = uint64Safe(header, 8)
  }
  if (!Number.isSafeInteger(boxSize) || boxSize < headerSize || offset + boxSize > fileSize) {
    throw new V2VideoPreviewError('MP4 top-level box has an invalid size')
  }
  return { type: ascii(header, 4, 8), start: offset, end: offset + boxSize }
}

async function metadataFile(parts: readonly MetadataPart[]): Promise<ISOFile<number, unknown>> {
  const { createFile } = await import('mp4box')
  const file = createFile(true) as ISOFile<number, unknown>
  // MP4Box only marks metadata ready while dispatching onReady; registering the
  // callback is therefore part of parser initialization, not an observer detail.
  file.onReady = () => undefined
  for (const part of parts) appendPart(file, part)
  return file
}

function appendPart(file: ISOFile<number, unknown>, part: MetadataPart): void {
  const buffer = part.data.slice().buffer as MP4BoxBuffer
  buffer.fileStart = part.start
  try {
    file.appendBuffer(buffer)
  } catch (cause) {
    throw new V2VideoPreviewError('MP4 parser rejected a bounded byte range', { cause })
  }
}

function requireMovie(file: ISOFile<number, unknown>): Movie {
  if (!file.readySent) throw new V2VideoPreviewError('MP4 metadata is incomplete')
  return file.getInfo()
}

function requireVideoTrack(info: Movie): Track {
  if (info.videoTracks.length !== 1) {
    throw new V2VideoPreviewError('MP4 preview requires exactly one video track')
  }
  return info.videoTracks[0]!
}

function requireTrack(info: Movie, trackId: number): Track {
  const track = info.videoTracks.find((candidate) => candidate.id === trackId)
  if (track === undefined) throw new V2VideoPreviewError('MP4 video track identity changed')
  return track
}

function validateTrack(track: Track, samples: readonly Sample[], exactSize: bigint): void {
  const width = track.video?.width ?? 0
  const height = track.video?.height ?? 0
  const duration = track.movie_duration / track.movie_timescale
  if (!SUPPORTED_VIDEO_CODEC.test(track.codec) || !Number.isFinite(duration) ||
      duration <= 0 || duration > V2_VIDEO_MAXIMUM_SECONDS) {
    throw new V2VideoPreviewError('MP4 codec or duration is outside the preview policy')
  }
  if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height) || width <= 0 || height <= 0 ||
      width > V2_VIDEO_MAXIMUM_DIMENSION || height > V2_VIDEO_MAXIMUM_DIMENSION ||
      BigInt(width) * BigInt(height) * 4n > BigInt(V2_VIDEO_DECODED_BYTES)) {
    throw new V2VideoPreviewError('Video dimensions exceed the decoded-frame limit')
  }
  if (samples.length === 0 || samples.length > V2_VIDEO_MAXIMUM_SAMPLES) {
    throw new V2VideoPreviewError('MP4 sample count exceeds the preview limit')
  }
  for (const sample of samples) requireSample(sample, exactSize)
}

function requireSample(sample: Sample | undefined, exactSize: bigint): asserts sample is Sample {
  if (sample === undefined || !Number.isSafeInteger(sample.offset) || !Number.isSafeInteger(sample.size) ||
      sample.offset < 0 || sample.size <= 0 || sample.size > V2_VIDEO_SAMPLE_BYTES ||
      BigInt(sample.offset) + BigInt(sample.size) > exactSize ||
      !Number.isSafeInteger(sample.cts) || sample.cts < 0 ||
      !Number.isSafeInteger(sample.dts) || sample.dts < 0 ||
      !Number.isSafeInteger(sample.duration) || sample.duration <= 0 ||
      !Number.isSafeInteger(sample.timescale) || sample.timescale <= 0) {
    throw new V2VideoPreviewError('MP4 sample geometry exceeds the preview limits')
  }
  const sampleEnd = BigInt(sample.cts) + BigInt(sample.duration)
  if (sampleEnd > BigInt(V2_VIDEO_MAXIMUM_SECONDS) * BigInt(sample.timescale)) {
    throw new V2VideoPreviewError('MP4 sample timing exceeds the preview limits')
  }
}

function nextSegment(
  file: ISOFile<number, unknown>,
  trackId: number,
  signal: AbortSignal,
): Promise<ArrayBuffer> {
  signal.throwIfAborted()
  return new Promise<ArrayBuffer>((resolve, reject) => {
    const aborted = () => {
      file.stop()
      reject(signal.reason ?? new DOMException('Video seek aborted', 'AbortError'))
    }
    signal.addEventListener('abort', aborted, { once: true })
    file.onError = (module, message) => {
      signal.removeEventListener('abort', aborted)
      reject(new V2VideoPreviewError(`MP4 ${module} error: ${message}`))
    }
    file.onSegment = (id, _user, buffer) => {
      if (id !== trackId) return
      signal.removeEventListener('abort', aborted)
      resolve(buffer)
    }
  })
}

async function bytesAt(
  source: V2PreviewRangeSource,
  windows: readonly MetadataPart[],
  start: number,
  end: number,
  signal: AbortSignal,
): Promise<Uint8Array<ArrayBuffer>> {
  const wanted = { start, end }
  const window = windows.find((candidate) => contains(candidate, wanted))
  if (window !== undefined) return window.data.slice(start - window.start, end - window.start)
  return source.read(byteRange(BigInt(start), BigInt(end)), signal)
}

function contains(part: MetadataPart, range: BoxLocation): boolean {
  return part.start <= range.start && part.start + part.data.byteLength >= range.end
}

function ascii(bytes: Uint8Array, start: number, end: number): string {
  return String.fromCharCode(...bytes.subarray(start, end))
}

function uint32(bytes: Uint8Array, offset: number): number {
  return new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0, false)
}

function uint64Safe(bytes: Uint8Array, offset: number): number {
  const value = new DataView(bytes.buffer, bytes.byteOffset + offset, 8).getBigUint64(0, false)
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new V2VideoPreviewError('MP4 box size exceeds browser integer precision')
  }
  return Number(value)
}

function forwardAbort(signal: AbortSignal, controller: AbortController): () => void {
  const abort = () => controller.abort(signal.reason ?? new DOMException('Video seek aborted', 'AbortError'))
  signal.addEventListener('abort', abort, { once: true })
  if (signal.aborted) abort()
  return () => signal.removeEventListener('abort', abort)
}
