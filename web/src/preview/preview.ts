import type { CatalogEntry } from '../catalog/records'
import { bigintToSafeNumber, byteRange } from '../content/geometry'
import type { BlockRangeReader, BlockSlice } from '../content/broker'
import type { OpenedRevision, RevisionReader } from '../content/session-services'
import type { FileRevisionDescriptor } from '../content/records'
import {
  continueImageHeader,
  sniffImageHeader,
  IMAGE_HEADER_BYTES,
  ImagePreviewError,
  isImageHeaderContinuation,
  type ImageHeader,
  type ImageHeaderContinuation,
} from './image-header'
import {
  Mp4RangePreview,
  VideoPreviewError,
  type Mp4Metadata,
  type Mp4Segment,
  type PreviewRangeSource,
} from './mp4-range'

import { isAutomaticPhotoCandidate, requireAutomaticPhotoHeader, AutomaticPhotoDeferredError } from './automatic-photo'

// Displayed dimensions, after any orientation metadata, matching the preview <img>.
export interface ImageDecodeResult {
  readonly width: number
  readonly height: number
}

export interface PreviewPorts {
  readonly decodeImage?: (blob: Blob, signal: AbortSignal) => Promise<ImageDecodeResult>
  readonly createObjectUrl?: (blob: Blob) => string
  readonly revokeObjectUrl?: (url: string) => void
  readonly openVideo?: (
    source: PreviewRangeSource,
    signal: AbortSignal,
  ) => Promise<VideoRangePort>
  readonly supportsVideo?: (mimeType: string) => boolean
}

export interface VideoRangePort {
  readonly metadata: Mp4Metadata
  segmentAt(seconds: number, signal: AbortSignal): Promise<Mp4Segment>
}

export type PreviewPresentation =
  | {
      readonly kind: 'image'
      readonly name: string
      readonly url: string
      readonly mimeType: string
      readonly width: number
      readonly height: number
    }
  | {
      readonly kind: 'video'
      readonly name: string
      readonly url: string
      readonly mimeType: string
      readonly width: number
      readonly height: number
      readonly durationSeconds: number
      readonly keyframeSeconds: number
      readonly decodeSeconds: number
    }

// Whether a FilePreview.seek failure is confined to the requested position,
// so the session, its previous frame and the download stay usable and the UI
// may keep the preview open. Only the bounded MP4 parser rejecting that one
// segment (VideoPreviewError) qualifies. Decoder failures and the presentation
// deadline happen after seek resolves and are the UI's to classify. Every
// other seek failure (a broker range read or disconnect, lease or source
// invalidation such as ReceiveSourceInvalidatedError, an integrity check such
// as PreviewRuntimeError, a remote rejection whether retryable or not, or a
// dead peer session) says the session itself cannot serve another seek.
export function isPositionLocalPreviewFailure(error: unknown): boolean {
  return error instanceof VideoPreviewError
}

export class PreviewRuntimeError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PreviewRuntimeError'
  }
}

export class FilePreview {
  readonly #entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly #opened: OpenedRevision
  readonly #source: BrokerPreviewSource
  readonly #decodeImage: (blob: Blob, signal: AbortSignal) => Promise<ImageDecodeResult>
  readonly #createObjectUrl: (blob: Blob) => string
  readonly #revokeObjectUrl: (url: string) => void
  readonly #openVideo: NonNullable<PreviewPorts['openVideo']>
  readonly #supportsVideo: NonNullable<PreviewPorts['supportsVideo']>
  #video: VideoRangePort | undefined
  #url: string | undefined
  #seekController: AbortController | undefined
  #presentation: PreviewPresentation | undefined
  #releaseTask: Promise<void> | undefined
  #closeTask: Promise<void> | undefined

  private constructor(
    entry: Extract<CatalogEntry, { kind: 'file' }>,
    opened: OpenedRevision,
    broker: BlockRangeReader,
    ports: PreviewPorts,
  ) {
    this.#entry = entry
    this.#opened = opened
    this.#source = new BrokerPreviewSource(opened.descriptor, opened.leaseId, broker)
    this.#decodeImage = ports.decodeImage ?? decodeBrowserImage
    this.#createObjectUrl = ports.createObjectUrl ?? ((blob) => URL.createObjectURL(blob))
    this.#revokeObjectUrl = ports.revokeObjectUrl ?? ((url) => URL.revokeObjectURL(url))
    this.#openVideo = ports.openVideo ?? ((source, signal) => Mp4RangePreview.open(source, signal))
    this.#supportsVideo = ports.supportsVideo ?? supportsBrowserVideo
  }

  static async open(
    entry: CatalogEntry,
    revisions: RevisionReader,
    broker: BlockRangeReader,
    signal: AbortSignal,
    ports: PreviewPorts = {},
  ): Promise<FilePreview> {
    return this.#open(entry, revisions, broker, signal, ports, 'manual')
  }

  static async openAutomaticPhoto(
    entry: CatalogEntry,
    revisions: RevisionReader,
    broker: BlockRangeReader,
    signal: AbortSignal,
    ports: PreviewPorts = {},
  ): Promise<FilePreview> {
    signal.throwIfAborted()
    if (!isAutomaticPhotoCandidate(entry)) {
      throw new AutomaticPhotoDeferredError('Use Preview for this file')
    }
    return this.#open(entry, revisions, broker, signal, ports, 'automatic-photo')
  }

  static async #open(
    entry: CatalogEntry,
    revisions: RevisionReader,
    broker: BlockRangeReader,
    signal: AbortSignal,
    ports: PreviewPorts,
    intent: 'manual' | 'automatic-photo',
  ): Promise<FilePreview> {
    signal.throwIfAborted()
    if (entry.kind !== 'file') throw new TypeError('Only one explicit file can be previewed')
    const opened = await revisions.open(entry.id, signal)
    if (opened.descriptor.exactSize !== entry.expectedSize) {
      await opened.release().catch(() => undefined)
      throw new PreviewRuntimeError('Preview revision changed from its catalog size')
    }
    const preview = new FilePreview(entry, opened, broker, ports)
    try {
      await preview.#initialize(signal, intent)
      return preview
    } catch (error) {
      await preview.close().catch(() => undefined)
      // Speculative gallery presentation must not turn format rejection into a
      // browse error panel; manual Preview still surfaces ImagePreviewError.
      if (intent === 'automatic-photo' && error instanceof ImagePreviewError) {
        throw new AutomaticPhotoDeferredError('Use Preview to inspect this file', { cause: error })
      }
      throw error
    }
  }

  get current(): PreviewPresentation {
    if (this.#presentation === undefined) throw new PreviewRuntimeError('Preview is not initialized')
    return this.#presentation
  }

  async seek(seconds: number, signal?: AbortSignal): Promise<PreviewPresentation> {
    const video = this.#video
    if (video === undefined) throw new PreviewRuntimeError('Only video previews can seek')
    this.#seekController?.abort(new DOMException('A newer video seek superseded this one', 'AbortError'))
    const controller = new AbortController()
    this.#seekController = controller
    const unlink = forwardAbort(signal, controller)
    try {
      const segment = await video.segmentAt(seconds, controller.signal)
      controller.signal.throwIfAborted()
      const url = this.#replaceUrl(segment.bytes)
      this.#presentation = videoPresentation(this.#entry.name, url, video.metadata, segment)
      return this.#presentation
    } finally {
      unlink()
      if (this.#seekController === controller) this.#seekController = undefined
    }
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #initialize(signal: AbortSignal, intent: 'manual' | 'automatic-photo'): Promise<void> {
    signal.throwIfAborted()
    const headerEnd = this.#source.exactSize < BigInt(IMAGE_HEADER_BYTES)
      ? this.#source.exactSize
      : BigInt(IMAGE_HEADER_BYTES)
    const header = await this.#source.read(byteRange(0n, headerEnd), signal)
    const sniffed = sniffImageHeader(header, this.#source.exactSize)
    if (intent === 'automatic-photo' && isImageHeaderContinuation(sniffed) && sniffed.format === 'png') {
      // IHDR fixes decoded size; metadata reads cannot make an oversized image eligible.
      requireAutomaticPhotoHeader(sniffed.header)
    }
    const image = await this.#completeImageHeader(sniffed, signal)
    signal.throwIfAborted()
    if (intent === 'automatic-photo') requireAutomaticPhotoHeader(image)
    if (image !== undefined) {
      await this.#initializeImage(image, signal)
      return
    }
    if (!looksLikeMp4(header)) throw new PreviewRuntimeError('File is not a supported preview image or MP4 video')
    const video = await this.#openVideo(this.#source, signal)
    if (!this.#supportsVideo(video.metadata.mimeType)) {
      throw new PreviewRuntimeError('Browser does not support this bounded MP4 video codec')
    }
    this.#video = video
    await this.seek(0, signal)
  }

  async #completeImageHeader(
    sniffed: ImageHeader | ImageHeaderContinuation | undefined,
    signal: AbortSignal,
  ): Promise<ImageHeader | undefined> {
    const exactSize = this.#source.exactSize
    let walked = 0
    while (isImageHeaderContinuation(sniffed)) {
      // A walk that stops advancing would reread the same window forever.
      if (sniffed.offset <= walked) throw new PreviewRuntimeError('Image header walk did not advance')
      walked = sniffed.offset
      const start = BigInt(sniffed.offset)
      const end = start + BigInt(IMAGE_HEADER_BYTES) < exactSize
        ? start + BigInt(IMAGE_HEADER_BYTES)
        : exactSize
      const window = await this.#source.read(byteRange(start, end), signal)
      sniffed = continueImageHeader(window, sniffed, exactSize)
    }
    return sniffed
  }

  async #initializeImage(header: ImageHeader, signal: AbortSignal): Promise<void> {
    const bytes = await this.#source.read(
      byteRange(0n, this.#source.exactSize),
      signal,
    )
    const blob = new Blob([bytes], { type: header.mimeType })
    let decoded: ImageDecodeResult
    try {
      decoded = await this.#decodeImage(blob, signal)
    } catch (cause) {
      throw new ImagePreviewError('Browser rejected the image payload', { cause })
    }
    signal.throwIfAborted()
    if (!matchesOrientedHeader(decoded, header)) {
      throw new ImagePreviewError('Decoded image dimensions disagree with its bounded header')
    }
    const url = this.#replaceUrl(blob)
    this.#presentation = Object.freeze({
      kind: 'image',
      name: this.#entry.name,
      url,
      mimeType: header.mimeType,
      width: decoded.width,
      height: decoded.height,
    })
    await this.#release()
  }

  #replaceUrl(blob: Blob): string {
    const next = this.#createObjectUrl(blob)
    const previous = this.#url
    this.#url = next
    if (previous !== undefined) this.#revokeObjectUrl(previous)
    return next
  }

  #release(): Promise<void> {
    this.#releaseTask ??= this.#opened.release()
    return this.#releaseTask
  }

  async #close(): Promise<void> {
    this.#seekController?.abort(new DOMException('Preview closed', 'AbortError'))
    this.#seekController = undefined
    const url = this.#url
    this.#url = undefined
    this.#presentation = undefined
    if (url !== undefined) this.#revokeObjectUrl(url)
    await this.#release()
  }
}

class BrokerPreviewSource implements PreviewRangeSource {
  readonly exactSize: bigint
  readonly #descriptor: FileRevisionDescriptor
  readonly #leaseId: Uint8Array<ArrayBuffer>
  readonly #broker: BlockRangeReader

  constructor(
    descriptor: FileRevisionDescriptor,
    leaseId: Uint8Array,
    broker: BlockRangeReader,
  ) {
    this.exactSize = descriptor.exactSize
    this.#descriptor = descriptor
    this.#leaseId = leaseId.slice()
    this.#broker = broker
  }

  async read(range: ReturnType<typeof byteRange>, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
    const checked = this.#descriptor.geometry.requireRange(range)
    const length = bigintToSafeNumber(checked.end - checked.start, 'preview range length')
    const output = new Uint8Array(length)
    let written = 0
    for await (const slice of this.#broker.readRange(
      this.#descriptor,
      this.#leaseId,
      checked,
      { signal, priority: 'preview' },
    )) {
      requireNextSlice(slice, checked.start + BigInt(written))
      output.set(slice.data, written)
      written += slice.data.byteLength
    }
    if (written !== length) throw new PreviewRuntimeError('Preview range ended before its exact length')
    return output
  }

}

async function decodeBrowserImage(blob: Blob, signal: AbortSignal): Promise<ImageDecodeResult> {
  signal.throwIfAborted()
  // Decode the way the preview <img> displays the file. Current browsers treat
  // the legacy 'none' orientation as the default 'from-image', so no option
  // value reliably exposes the stored, unrotated dimensions.
  const bitmap = await createImageBitmap(blob)
  try {
    signal.throwIfAborted()
    return Object.freeze({ width: bitmap.width, height: bitmap.height })
  } finally {
    bitmap.close()
  }
}

// Orientation metadata (EXIF orientations 5-8) displays stored pixels with a
// quarter turn. The pixel count, and thus the header's decoded-memory bound, is
// unchanged, so a transposed decode still proves the bounded header honest.
function matchesOrientedHeader(decoded: ImageDecodeResult, header: ImageHeader): boolean {
  return (decoded.width === header.width && decoded.height === header.height) ||
    (decoded.width === header.height && decoded.height === header.width)
}

function looksLikeMp4(bytes: Uint8Array): boolean {
  return bytes.byteLength >= 12 && String.fromCharCode(...bytes.subarray(4, 8)) === 'ftyp'
}

function requireNextSlice(slice: BlockSlice, expectedOffset: bigint): void {
  if (slice.offset !== expectedOffset || slice.data.byteLength === 0) {
    throw new PreviewRuntimeError('Preview range returned a gap or empty slice')
  }
}

function videoPresentation(
  name: string,
  url: string,
  metadata: Mp4Metadata,
  segment: Mp4Segment,
): PreviewPresentation {
  return Object.freeze({
    kind: 'video',
    name,
    url,
    mimeType: metadata.mimeType,
    width: metadata.width,
    height: metadata.height,
    durationSeconds: metadata.durationSeconds,
    keyframeSeconds: segment.keyframeSeconds,
    decodeSeconds: segment.decodeSeconds,
  })
}

function forwardAbort(signal: AbortSignal | undefined, controller: AbortController): () => void {
  if (signal === undefined) return () => undefined
  const abort = () => controller.abort(signal.reason ?? new DOMException('Preview operation aborted', 'AbortError'))
  signal.addEventListener('abort', abort, { once: true })
  if (signal.aborted) abort()
  return () => signal.removeEventListener('abort', abort)
}

function supportsBrowserVideo(mimeType: string): boolean {
  return document.createElement('video').canPlayType(mimeType) !== ''
}
