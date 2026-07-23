import type { V2CatalogEntry } from '../catalog/v2-records'
import { bigintToSafeNumber, byteRange } from '../content/geometry'
import type { V2BlockRangeReader, V2BlockSlice } from '../content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../content/v2-session-services'
import type { V2FileRevisionDescriptor } from '../content/v2-records'
import {
  sniffV2ImageHeader,
  V2_IMAGE_HEADER_BYTES,
  V2ImagePreviewError,
  type V2ImageHeader,
} from './image-header'
import {
  V2Mp4RangePreview,
  type V2Mp4Metadata,
  type V2Mp4Segment,
  type V2PreviewRangeSource,
} from './mp4-range'

export interface V2ImageDecodeResult {
  readonly width: number
  readonly height: number
}

export interface V2PreviewPorts {
  readonly decodeImage?: (blob: Blob, signal: AbortSignal) => Promise<V2ImageDecodeResult>
  readonly createObjectUrl?: (blob: Blob) => string
  readonly revokeObjectUrl?: (url: string) => void
  readonly openVideo?: (
    source: V2PreviewRangeSource,
    signal: AbortSignal,
  ) => Promise<V2VideoRangePort>
  readonly supportsVideo?: (mimeType: string) => boolean
}

export interface V2VideoRangePort {
  readonly metadata: V2Mp4Metadata
  segmentAt(seconds: number, signal: AbortSignal): Promise<V2Mp4Segment>
}

export type V2PreviewPresentation =
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
      readonly positionSeconds: number
    }

export class V2PreviewRuntimeError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2PreviewRuntimeError'
  }
}

export class V2FilePreview {
  readonly #entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly #opened: V2OpenedRevision
  readonly #source: BrokerPreviewSource
  readonly #decodeImage: (blob: Blob, signal: AbortSignal) => Promise<V2ImageDecodeResult>
  readonly #createObjectUrl: (blob: Blob) => string
  readonly #revokeObjectUrl: (url: string) => void
  readonly #openVideo: NonNullable<V2PreviewPorts['openVideo']>
  readonly #supportsVideo: NonNullable<V2PreviewPorts['supportsVideo']>
  #video: V2VideoRangePort | undefined
  #url: string | undefined
  #seekController: AbortController | undefined
  #presentation: V2PreviewPresentation | undefined
  #releaseTask: Promise<void> | undefined
  #closeTask: Promise<void> | undefined

  private constructor(
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    opened: V2OpenedRevision,
    broker: V2BlockRangeReader,
    ports: V2PreviewPorts,
  ) {
    this.#entry = entry
    this.#opened = opened
    this.#source = new BrokerPreviewSource(opened.descriptor, opened.leaseId, broker)
    this.#decodeImage = ports.decodeImage ?? decodeBrowserImage
    this.#createObjectUrl = ports.createObjectUrl ?? ((blob) => URL.createObjectURL(blob))
    this.#revokeObjectUrl = ports.revokeObjectUrl ?? ((url) => URL.revokeObjectURL(url))
    this.#openVideo = ports.openVideo ?? ((source, signal) => V2Mp4RangePreview.open(source, signal))
    this.#supportsVideo = ports.supportsVideo ?? supportsBrowserVideo
  }

  static async open(
    entry: V2CatalogEntry,
    revisions: V2RevisionReader,
    broker: V2BlockRangeReader,
    signal: AbortSignal,
    ports: V2PreviewPorts = {},
  ): Promise<V2FilePreview> {
    signal.throwIfAborted()
    if (entry.kind !== 'file') throw new TypeError('Only one explicit file can be previewed')
    const opened = await revisions.open(entry.id, signal)
    if (opened.descriptor.exactSize !== entry.expectedSize) {
      await opened.release().catch(() => undefined)
      throw new V2PreviewRuntimeError('Preview revision changed from its catalog size')
    }
    const preview = new V2FilePreview(entry, opened, broker, ports)
    try {
      await preview.#initialize(signal)
      return preview
    } catch (error) {
      await preview.close().catch(() => undefined)
      throw error
    }
  }

  get current(): V2PreviewPresentation {
    if (this.#presentation === undefined) throw new V2PreviewRuntimeError('Preview is not initialized')
    return this.#presentation
  }

  async seek(seconds: number, signal?: AbortSignal): Promise<V2PreviewPresentation> {
    const video = this.#video
    if (video === undefined) throw new V2PreviewRuntimeError('Only video previews can seek')
    this.#seekController?.abort(new DOMException('A newer video seek superseded this one', 'AbortError'))
    const controller = new AbortController()
    this.#seekController = controller
    const unlink = forwardAbort(signal, controller)
    try {
      const segment = await video.segmentAt(seconds, controller.signal)
      controller.signal.throwIfAborted()
      const url = this.#replaceUrl(segment.bytes)
      this.#presentation = videoPresentation(this.#entry.name, url, video.metadata, segment.positionSeconds)
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

  async #initialize(signal: AbortSignal): Promise<void> {
    const headerEnd = this.#source.exactSize < BigInt(V2_IMAGE_HEADER_BYTES)
      ? this.#source.exactSize
      : BigInt(V2_IMAGE_HEADER_BYTES)
    const header = await this.#source.read(byteRange(0n, headerEnd), signal)
    const image = sniffV2ImageHeader(header, this.#source.exactSize)
    if (image !== undefined) {
      await this.#initializeImage(image, signal)
      return
    }
    if (!looksLikeMp4(header)) throw new V2PreviewRuntimeError('File is not a supported preview image or MP4 video')
    const video = await this.#openVideo(this.#source, signal)
    if (!this.#supportsVideo(video.metadata.mimeType)) {
      throw new V2PreviewRuntimeError('Browser does not support this bounded MP4 video codec')
    }
    this.#video = video
    await this.seek(0, signal)
  }

  async #initializeImage(header: V2ImageHeader, signal: AbortSignal): Promise<void> {
    const bytes = await this.#source.read(
      byteRange(0n, this.#source.exactSize),
      signal,
    )
    const blob = new Blob([bytes], { type: header.mimeType })
    let decoded: V2ImageDecodeResult
    try {
      decoded = await this.#decodeImage(blob, signal)
    } catch (cause) {
      throw new V2ImagePreviewError('Browser rejected the image payload', { cause })
    }
    signal.throwIfAborted()
    if (decoded.width !== header.width || decoded.height !== header.height) {
      throw new V2ImagePreviewError('Decoded image dimensions disagree with its bounded header')
    }
    const url = this.#replaceUrl(blob)
    this.#presentation = Object.freeze({
      kind: 'image',
      name: this.#entry.name,
      url,
      mimeType: header.mimeType,
      width: header.width,
      height: header.height,
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

class BrokerPreviewSource implements V2PreviewRangeSource {
  readonly exactSize: bigint
  readonly #descriptor: V2FileRevisionDescriptor
  readonly #leaseId: Uint8Array<ArrayBuffer>
  readonly #broker: V2BlockRangeReader

  constructor(
    descriptor: V2FileRevisionDescriptor,
    leaseId: Uint8Array,
    broker: V2BlockRangeReader,
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
    if (written !== length) throw new V2PreviewRuntimeError('Preview range ended before its exact length')
    return output
  }

}

async function decodeBrowserImage(blob: Blob, signal: AbortSignal): Promise<V2ImageDecodeResult> {
  signal.throwIfAborted()
  const bitmap = await createImageBitmap(blob, { imageOrientation: 'none' })
  try {
    signal.throwIfAborted()
    return Object.freeze({ width: bitmap.width, height: bitmap.height })
  } finally {
    bitmap.close()
  }
}

function looksLikeMp4(bytes: Uint8Array): boolean {
  return bytes.byteLength >= 12 && String.fromCharCode(...bytes.subarray(4, 8)) === 'ftyp'
}

function requireNextSlice(slice: V2BlockSlice, expectedOffset: bigint): void {
  if (slice.offset !== expectedOffset || slice.data.byteLength === 0) {
    throw new V2PreviewRuntimeError('Preview range returned a gap or empty slice')
  }
}

function videoPresentation(
  name: string,
  url: string,
  metadata: V2Mp4Metadata,
  positionSeconds: number,
): V2PreviewPresentation {
  return Object.freeze({
    kind: 'video',
    name,
    url,
    mimeType: metadata.mimeType,
    width: metadata.width,
    height: metadata.height,
    durationSeconds: metadata.durationSeconds,
    positionSeconds,
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
