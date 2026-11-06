import { describe, expect, it, vi } from 'vitest'

import type { CatalogEntry } from '../../src/catalog/records'
import { ConnectivityRouteAuthority } from '../../src/connectivity/receiver-policy'
import { byteRange, FileGeometry, type ByteRange } from '../../src/content/geometry'
import {
  BlockBroker,
  LaneSet,
  type BlockDemand,
  type BlockLane,
  type BlockRangeReader,
} from '../../src/content/broker'
import type { BlockRecord, FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import { VideoPreviewError, type Mp4Segment, type PreviewRangeSource } from '../../src/preview/mp4-range'
import { ReceiveSourceInvalidatedError } from '../../src/application/receive/errors'
import {
  FilePreview,
  isPositionLocalPreviewFailure,
  PreviewRuntimeError,
  type PreviewPorts,
  type VideoRangePort,
} from '../../src/preview/preview'

// Lets a test break the image header walk's progress invariant.
const headerWalk = vi.hoisted(() => ({ stall: false }))
vi.mock('../../src/preview/image-header', async (importOriginal) => {
  const original = await importOriginal<typeof import('../../src/preview/image-header')>()
  return {
    ...original,
    continueImageHeader: (...args: Parameters<typeof original.continueImageHeader>) =>
      headerWalk.stall ? args[1] : original.continueImageHeader(...args),
  }
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function fileEntry(size: bigint, name = 'preview.bin'): Extract<CatalogEntry, { kind: 'file' }> {
  return Object.freeze({ kind: 'file', id: identity(2), idText: 'file', name, expectedSize: size })
}

class ByteLane implements BlockLane {
  readonly id = 1
  readonly calls: bigint[] = []
  readonly #bytes: Uint8Array<ArrayBuffer>

  constructor(bytes: Uint8Array<ArrayBuffer>) {
    this.#bytes = bytes
  }

  async fetchBlock(demand: BlockDemand, signal: AbortSignal): Promise<BlockRecord> {
    signal.throwIfAborted()
    this.calls.push(demand.localBlockIndex)
    const range = demand.descriptor.geometry.blockPlaintext(demand.localBlockIndex)
    return Object.freeze({
      descriptor: demand.descriptor,
      localBlockIndex: demand.localBlockIndex,
      data: this.#bytes.slice(Number(range.start), Number(range.end)),
    })
  }
}

function runtime(bytes: Uint8Array<ArrayBuffer>, blockSize: bigint) {
  const descriptor: FileRevisionDescriptor = Object.freeze({
    shareInstance: identity(1),
    shareInstanceId: 'share',
    fileId: identity(2),
    fileIdText: 'file',
    fileRevision: identity(3),
    fileRevisionText: 'revision',
    exactSize: BigInt(bytes.byteLength),
    geometry: new FileGeometry(BigInt(bytes.byteLength), blockSize),
  })
  const lane = new ByteLane(bytes)
  const lanes = new LaneSet()
  lanes.add(lane, 'application-relay')
  const upstreamBroker = new BlockBroker(lanes)
  const routes = new ConnectivityRouteAuthority()
  const broker: BlockRangeReader = Object.freeze({
    readRange: (
      revision: FileRevisionDescriptor,
      leaseId: Uint8Array,
      range: ByteRange,
      options: NonNullable<Parameters<BlockRangeReader['readRange']>[3]> = {},
    ) => upstreamBroker.readRouteAuthorizedRange(
      revision,
      leaseId,
      range,
      { ...options, routes },
    ),
  })
  let releases = 0
  const revisions = {
    open: async () => ({
      descriptor,
      leaseId: identity(4),
      release: async () => { releases += 1 },
    }),
  } satisfies RevisionReader
  return { broker, descriptor, lane, releases: () => releases, revisions }
}

function animatedWebp(): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(30)
  bytes.set([82, 73, 70, 70])
  new DataView(bytes.buffer).setUint32(4, 22, true)
  bytes.set([87, 69, 66, 80, 86, 80, 56, 88], 8)
  new DataView(bytes.buffer).setUint32(16, 10, true)
  bytes[20] = 0x02
  return bytes
}

function animatedPng(): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(53)
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10])
  const view = new DataView(bytes.buffer)
  view.setUint32(8, 13, false)
  bytes.set([73, 72, 68, 82], 12)
  view.setUint32(16, 64, false)
  view.setUint32(20, 48, false)
  view.setUint32(33, 0, false)
  bytes.set([97, 99, 84, 76], 37)
  view.setUint32(45, 1_000_000, false)
  bytes.set([73, 68, 65, 84], 49)
  return bytes
}

function truncatedJpeg(): Uint8Array<ArrayBuffer> {
  // SOI + one EXIF segment and EOI: no frame header for the sniff to find.
  const bytes = new Uint8Array(2 + 2 + 16 + 2)
  bytes.set([0xff, 0xd8, 0xff, 0xe1, 0x00, 0x10], 0)
  bytes.set([0xff, 0xd9], bytes.byteLength - 2)
  return bytes
}

function pngFile(size: number, width: number, height: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(size)
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10])
  new DataView(bytes.buffer).setUint32(8, 13, false)
  bytes.set([73, 72, 68, 82], 12)
  new DataView(bytes.buffer).setUint32(16, width, false)
  new DataView(bytes.buffer).setUint32(20, height, false)
  new DataView(bytes.buffer).setUint32(33, size - 41, false)
  bytes.set([73, 68, 65, 84], 37)
  return bytes
}

function pngWithMetadata(width: number, height: number): Uint8Array<ArrayBuffer> {
  // C2PA manifests can push IDAT beyond the first sniff window.
  const metadata = 150 * 1024
  const bytes = new Uint8Array(33 + 12 + metadata + 12 + 1024)
  const view = new DataView(bytes.buffer)
  bytes.set(pngFile(41, width, height).subarray(0, 33))
  view.setUint32(33, metadata, false)
  bytes.set([99, 97, 66, 88], 37)
  view.setUint32(33 + 12 + metadata, 1024, false)
  bytes.set([73, 68, 65, 84], 33 + 12 + metadata + 4)
  return bytes
}

// Camera EXIF previews and MPF segments put the frame header past one sniff.
function cameraJpegFile(metadataSegments: number, width: number, height: number): Uint8Array<ArrayBuffer> {
  const segment = 2 + 0xffff
  const frame = 2 + metadataSegments * segment
  const bytes = new Uint8Array(frame + 2 + 17 + 2)
  const view = new DataView(bytes.buffer)
  bytes.set([0xff, 0xd8])
  for (let index = 0; index < metadataSegments; index += 1) {
    bytes.set([0xff, 0xe1, 0xff, 0xff], 2 + index * segment)
  }
  bytes.set([0xff, 0xc0], frame)
  view.setUint16(frame + 2, 17, false)
  view.setUint16(frame + 5, height, false)
  view.setUint16(frame + 7, width, false)
  bytes.set([0xff, 0xda], frame + 19)
  return bytes
}

function mp4ShapedFile(size: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(size)
  new DataView(bytes.buffer).setUint32(0, 24, false)
  bytes.set([102, 116, 121, 112], 4)
  return bytes
}

function urlPorts(extra: PreviewPorts = {}) {
  const created: Blob[] = []
  const revoked: string[] = []
  let next = 1
  const ports: PreviewPorts = {
    ...extra,
    createObjectUrl: (blob) => {
      created.push(blob)
      return `blob:preview-${next++}`
    },
    revokeObjectUrl: (url) => revoked.push(url),
  }
  return { created, ports, revoked }
}

describe('v2 file preview runtime', () => {
  it('rejects oversized automatic candidates before opening a revision', async () => {
    const fixture = runtime(new Uint8Array(1), 1n)
    const revisions = { open: vi.fn(fixture.revisions.open) }
    await expect(FilePreview.openAutomaticPhoto(
      fileEntry(4n * 1024n * 1024n + 1n, 'large.png'),
      revisions, fixture.broker, new AbortController().signal,
    )).rejects.toThrow('Use Preview')
    expect(revisions.open).not.toHaveBeenCalled()
    expect(fixture.lane.calls).toEqual([])
  })

  it('checks automatic decoded size before a full read, then reuses the header for manual preview', async () => {
    const bytes = pngFile(100 * 1024, 4000, 3000)
    const fixture = runtime(bytes, 32n * 1024n)
    const decodeImage = vi.fn(async () => ({ width: 4000, height: 3000 }))
    const urls = urlPorts({ decodeImage })
    const entry = fileEntry(BigInt(bytes.byteLength), 'portrait.png')
    await expect(FilePreview.openAutomaticPhoto(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )).rejects.toThrow('larger image')
    expect(fixture.lane.calls).toEqual([0n, 1n])
    expect(decodeImage).not.toHaveBeenCalled()
    expect(fixture.releases()).toBe(1)
    const manual = await FilePreview.open(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )
    expect(manual.current.kind).toBe('image')
    expect(fixture.lane.calls).toEqual([0n, 1n, 2n, 3n])
    await manual.close()
  })

  it.each([
    ['animated.webp', animatedWebp()],
    ['animated.png', animatedPng()],
    ['truncated.jpg', truncatedJpeg()],
  ] as const)('defers automatic photos that fail image-format inspection (%s)', async (name, bytes) => {
    const fixture = runtime(bytes, BigInt(bytes.byteLength))
    await expect(FilePreview.openAutomaticPhoto(
      fileEntry(BigInt(bytes.byteLength), name),
      fixture.revisions, fixture.broker, new AbortController().signal,
    )).rejects.toBeInstanceOf(AutomaticPhotoDeferredError)
    expect(fixture.releases()).toBe(1)
    // Manual Preview still surfaces the format failure so the user can act on it.
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), name),
      fixture.revisions, fixture.broker, new AbortController().signal,
    )).rejects.toThrow(/Animated|ended before its frame header/i)
    expect(fixture.releases()).toBe(2)
  })

  it('never promotes an image extension to automatic MP4 decoding', async () => {

    const bytes = mp4ShapedFile(100 * 1024)
    const fixture = runtime(bytes, 64n * 1024n)
    const openVideo = vi.fn()
    await expect(FilePreview.openAutomaticPhoto(
      fileEntry(BigInt(bytes.byteLength), 'disguised.png'),
      fixture.revisions, fixture.broker, new AbortController().signal, { openVideo },
    )).rejects.toThrow('Use Preview')
    expect(openVideo).not.toHaveBeenCalled()
    expect(fixture.lane.calls).toEqual([0n])
    expect(fixture.releases()).toBe(1)
  })

  it('aborts speculative decoding without publishing or leaking an object URL', async () => {
    const bytes = pngFile(128, 20, 10)
    const fixture = runtime(bytes, 64n)
    const controller = new AbortController()
    const urls = urlPorts({ decodeImage: async () => {
      controller.abort(new DOMException('Download admitted', 'AbortError'))
      return { width: 20, height: 10 }
    } })
    await expect(FilePreview.openAutomaticPhoto(
      fileEntry(128n, 'photo.png'), fixture.revisions, fixture.broker, controller.signal, urls.ports,
    )).rejects.toMatchObject({ name: 'AbortError' })
    expect(urls.created).toEqual([])
    expect(fixture.releases()).toBe(1)
  })

  it('sniffs before decode, reuses cached header blocks, and revokes its image URL', async () => {
    const bytes = pngFile(70 * 1024, 320, 200)
    const fixture = runtime(bytes, 32n * 1024n)
    const urls = urlPorts({
      decodeImage: async (blob, signal) => {
        signal.throwIfAborted()
        expect(blob.size).toBe(bytes.byteLength)
        return { width: 320, height: 200 }
      },
    })
    const preview = await FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'still.png'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )

    expect(preview.current).toMatchObject({
      kind: 'image',
      mimeType: 'image/png',
      url: 'blob:preview-1',
      width: 320,
      height: 200,
    })
    expect(fixture.lane.calls).toEqual([0n, 1n, 2n])
    expect(fixture.releases()).toBe(1)
    await preview.close()
    expect(urls.revoked).toEqual(['blob:preview-1'])
    expect(fixture.releases()).toBe(1)
  })

  it('previews a camera JPEG whose metadata pushes the frame header past the sniff', async () => {
    const bytes = cameraJpegFile(3, 6000, 4000)
    const fixture = runtime(bytes, 64n * 1024n)
    const urls = urlPorts({ decodeImage: async () => ({ width: 6000, height: 4000 }) })
    const preview = await FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'DSC_0001.JPG'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )
    expect(preview.current).toMatchObject({ kind: 'image', mimeType: 'image/jpeg', width: 6000, height: 4000 })
    // Each resumed header window is cached and reused by the full read.
    expect(fixture.lane.calls).toEqual([0n, 1n, 2n, 3n])
    await preview.close()
  })

  it('defers oversized automatic PNGs before metadata reads while preserving manual preview', async () => {
    const bytes = pngWithMetadata(4000, 3000)
    const fixture = runtime(bytes, 64n * 1024n)
    const decodeImage = vi.fn(async () => ({ width: 4000, height: 3000 }))
    const urls = urlPorts({ decodeImage })
    const entry = fileEntry(BigInt(bytes.byteLength), 'large.png')
    await expect(FilePreview.openAutomaticPhoto(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )).rejects.toBeInstanceOf(AutomaticPhotoDeferredError)
    expect(fixture.lane.calls).toEqual([0n])
    expect(decodeImage).not.toHaveBeenCalled()
    expect(urls.created).toEqual([])
    expect(fixture.releases()).toBe(1)

    const manual = await FilePreview.open(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )
    expect(manual.current).toMatchObject({ kind: 'image', width: 4000, height: 3000 })
    expect(new Set(fixture.lane.calls)).toEqual(new Set([0n, 1n, 2n]))
    expect(fixture.lane.calls).toHaveLength(3)
    await manual.close()
    expect(fixture.releases()).toBe(2)
  })

  it.each(['open', 'openAutomaticPhoto'] as const)('%s previews a PNG with large metadata', async (operation) => {
    const bytes = pngWithMetadata(1024, 1024)
    const fixture = runtime(bytes, 64n * 1024n)
    const urls = urlPorts({ decodeImage: async () => ({ width: 1024, height: 1024 }) })
    const preview = await FilePreview[operation](
      fileEntry(BigInt(bytes.byteLength), 'generated.png'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )
    expect(preview.current).toMatchObject({ kind: 'image', mimeType: 'image/png', width: 1024, height: 1024 })
    await preview.close()
  })

  it('fails an image header walk that stops advancing instead of looping', async () => {
    const bytes = cameraJpegFile(3, 6000, 4000)
    const fixture = runtime(bytes, 64n * 1024n)
    headerWalk.stall = true
    try {
      await expect(FilePreview.open(
        fileEntry(BigInt(bytes.byteLength), 'DSC_0001.JPG'),
        fixture.revisions,
        fixture.broker,
        new AbortController().signal,
        urlPorts().ports,
      )).rejects.toThrow('Image header walk did not advance')
    } finally {
      headerWalk.stall = false
    }
    // The stalled continuation is rejected before its window is read again.
    expect(fixture.lane.calls).toEqual([0n, 1n, 2n])
    expect(fixture.releases()).toBe(1)
  })

  it('cancels a superseded video seek without releasing the preview lease', async () => {
    const bytes = mp4ShapedFile(1 << 20)
    const fixture = runtime(bytes, 64n * 1024n)
    const pending: AbortSignal[] = []
    const openVideo = async (source: PreviewRangeSource): Promise<VideoRangePort> => {
      await source.read(byteRange(0n, 16n), new AbortController().signal)
      await source.read(byteRange(source.exactSize - 16n, source.exactSize), new AbortController().signal)
      return {
        metadata: {
          durationSeconds: 10,
          width: 640,
          height: 360,
          codec: 'avc1.42001e',
          mimeType: 'video/mp4; codecs="avc1.42001e"',
        },
        segmentAt: async (seconds, signal): Promise<Mp4Segment> => {
          if (seconds === 1) {
            pending.push(signal)
            return await new Promise((_resolve, reject) => {
              const abort = () => reject(signal.reason)
              signal.addEventListener('abort', abort, { once: true })
              if (signal.aborted) abort()
            })
          }
          const start = seconds === 0 ? 100_000n : 200_000n
          const sample = await source.read(byteRange(start, start + 4n), signal)
          return Object.freeze({
            bytes: new Blob([sample], { type: 'video/mp4' }),
            keyframeSeconds: seconds,
            decodeSeconds: seconds + 0.2,
          })
        },
      }
    }
    const urls = urlPorts({ openVideo, supportsVideo: () => true })
    const preview = await FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'clip.mp4'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )
    expect(preview.current).toMatchObject({ kind: 'video', url: 'blob:preview-1', keyframeSeconds: 0, decodeSeconds: 0.2 })
    expect(fixture.releases()).toBe(0)

    const superseded = preview.seek(1)
    await Promise.resolve()
    const latest = preview.seek(2)
    await expect(superseded).rejects.toMatchObject({ name: 'AbortError' })
    await expect(latest).resolves.toMatchObject({ url: 'blob:preview-2', keyframeSeconds: 2, decodeSeconds: 2.2 })
    expect(pending[0]?.aborted).toBe(true)
    expect(urls.revoked).toEqual(['blob:preview-1'])
    expect(fixture.releases()).toBe(0)
    expect(new Set(fixture.lane.calls).size).toBeLessThanOrEqual(4)

    await preview.close()
    expect(urls.revoked).toEqual(['blob:preview-1', 'blob:preview-2'])
    expect(fixture.releases()).toBe(1)
  })

  it('rejects a browser decode whose dimensions disagree with the bounded header', async () => {
    const bytes = pngFile(128, 20, 10)
    const fixture = runtime(bytes, 64n)
    const urls = urlPorts({ decodeImage: async () => ({ width: 21, height: 10 }) })
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'forged.png'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )).rejects.toThrow('dimensions disagree')
    expect(urls.created).toHaveLength(0)
    expect(fixture.releases()).toBe(1)
  })

  it('presents the oriented dimensions when the browser applies a quarter-turn orientation', async () => {
    const bytes = pngFile(128, 20, 10)
    const fixture = runtime(bytes, 64n)
    const urls = urlPorts({ decodeImage: async () => ({ width: 10, height: 20 }) })
    const preview = await FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'portrait.png'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )
    expect(preview.current).toMatchObject({ kind: 'image', width: 10, height: 20 })
    await preview.close()
  })

  it('rejects an MP4 codec the browser cannot decode before reading a sample', async () => {
    const bytes = mp4ShapedFile(128)
    const fixture = runtime(bytes, 64n)
    let sampleReads = 0
    const urls = urlPorts({
      openVideo: async () => ({
        metadata: {
          durationSeconds: 1,
          width: 20,
          height: 10,
          codec: 'avc1.42001e',
          mimeType: 'video/mp4; codecs="avc1.42001e"',
        },
        segmentAt: async () => {
          sampleReads += 1
          return { bytes: new Blob(), keyframeSeconds: 0, decodeSeconds: 0 }
        },
      }),
      supportsVideo: () => false,
    })
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'unsupported.mp4'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )).rejects.toThrow('does not support')
    expect(sampleReads).toBe(0)
    expect(fixture.releases()).toBe(1)
  })
})

describe('position-local preview failure classification', () => {
  it('treats only a bounded MP4 parser rejection as local to the requested position', () => {
    expect(isPositionLocalPreviewFailure(new VideoPreviewError('MP4 sample geometry exceeds the preview limits'))).toBe(true)
    expect(isPositionLocalPreviewFailure(new PreviewRuntimeError('preview integrity failed'))).toBe(false)
    expect(isPositionLocalPreviewFailure(new ReceiveSourceInvalidatedError(new Error('revision changed')))).toBe(false)
    expect(isPositionLocalPreviewFailure(new DOMException('aborted', 'AbortError'))).toBe(false)
    expect(isPositionLocalPreviewFailure(new Error('range read failed'))).toBe(false)
    expect(isPositionLocalPreviewFailure(undefined)).toBe(false)
  })
})
