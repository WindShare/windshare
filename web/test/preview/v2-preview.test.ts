import { readFileSync } from 'node:fs'

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
import { ImagePreviewError } from '../../src/preview/image-header'
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

function ftypFile(major: string, compatible: readonly string[] = [], size = 64): Uint8Array<ArrayBuffer> {
  const boxSize = 16 + compatible.length * 4
  const bytes = new Uint8Array(Math.max(size, boxSize))
  new DataView(bytes.buffer).setUint32(0, boxSize, false)
  bytes.set([102, 116, 121, 112], 4)
  bytes.set([...major.padEnd(4, ' ')].slice(0, 4).map((c) => c.charCodeAt(0)), 8)
  // bytes 12..15 remain minor_version 0.
  compatible.forEach((brand, index) => {
    bytes.set([...brand.padEnd(4, ' ')].slice(0, 4).map((c) => c.charCodeAt(0)), 16 + index * 4)
  })
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
    )).rejects.toMatchObject({ name: 'AutomaticPhotoDeferredError', cause: expect.any(ImagePreviewError) })
    expect(fixture.releases()).toBe(1)
    // Manual Preview still surfaces the format failure so the user can act on it.
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), name),
      fixture.revisions, fixture.broker, new AbortController().signal,
    )).rejects.toThrow(/Animated|ended before its frame header/i)
    expect(fixture.releases()).toBe(2)
  })

  it('retains browser decode evidence when automatic presentation is deferred', async () => {
    const fixture = runtime(pngFile(128, 20, 10), 64n)
    const failure = new DOMException('The image data is invalid', 'InvalidStateError')
    const urls = urlPorts({ decodeImage: async () => { throw failure } })
    const entry = fileEntry(128n, 'photo.png')
    await expect(FilePreview.openAutomaticPhoto(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )).rejects.toMatchObject({
      name: 'AutomaticPhotoDeferredError',
      cause: { name: 'ImagePreviewError', cause: failure },
    })
    await expect(FilePreview.open(
      entry, fixture.revisions, fixture.broker, new AbortController().signal, urls.ports,
    )).rejects.toMatchObject({ name: 'ImagePreviewError', cause: failure })
    expect(fixture.releases()).toBe(2)
    expect(urls.created).toEqual([])
  })

  it.each([new Error('range read failed'), new PreviewRuntimeError('invalid content range')])(
    'preserves source failures during automatic presentation: %s', async (failure) => {
      const fixture = runtime(pngFile(128, 20, 10), 64n)
      const broker: BlockRangeReader = {
        readRange: async function* () { yield await Promise.reject(failure) },
      }
      await expect(FilePreview.openAutomaticPhoto(
        fileEntry(128n, 'photo.png'), fixture.revisions, broker, new AbortController().signal,
      )).rejects.toBe(failure)
      expect(fixture.releases()).toBe(1)
    },
  )

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

  it.each([
    ['photo.heic', 'heic', []],
    ['photo.avif', 'avif', []],
    ['photo.heif', 'mif1', ['heic', 'miaf']],
  ] as const)('rejects %s still-image ftyp before the MP4 pipeline', async (name, major, compatible) => {
    const bytes = ftypFile(major, compatible)
    const fixture = runtime(bytes, BigInt(bytes.byteLength))
    const openVideo = vi.fn()
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), name),
      fixture.revisions, fixture.broker, new AbortController().signal, { openVideo },
    )).rejects.toMatchObject({
      name: 'ImagePreviewError',
      message: expect.stringMatching(/HEIC\/AVIF/),
    })
    expect(openVideo).not.toHaveBeenCalled()
    expect(fixture.releases()).toBe(1)
  })

  it('still opens ordinary MP4 ftyp brands through the video pipeline', async () => {
    const bytes = ftypFile('isom', ['isom', 'mp41'])
    const fixture = runtime(bytes, BigInt(bytes.byteLength))
    const openVideo = vi.fn(async () => {
      throw new VideoPreviewError('MP4 metadata was not found within the bounded box scan')
    })
    await expect(FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'clip.mp4'),
      fixture.revisions, fixture.broker, new AbortController().signal, { openVideo },
    )).rejects.toBeInstanceOf(VideoPreviewError)
    expect(openVideo).toHaveBeenCalledOnce()
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

describe('v2 file preview ftyp classification', () => {
  const realFixture = (name: string) => new Uint8Array(readFileSync(new URL(`../fixtures/preview/${name}`, import.meta.url)))
  const extendedFtyp = (major: string, minor: string, compatible: readonly string[], trailing: number[] = []) => {
    const enc = (b: string) => [...b].map((c) => c.charCodeAt(0))
    const boxSize = 24 + compatible.length * 4
    const bytes = new Uint8Array(Math.max(64, boxSize + trailing.length))
    const view = new DataView(bytes.buffer)
    view.setUint32(0, 1, false)
    bytes.set(enc('ftyp'), 4)
    view.setBigUint64(8, BigInt(boxSize), false)
    bytes.set(enc(major), 16)
    bytes.set(enc(minor), 20)
    compatible.forEach((brand, index) => bytes.set(enc(brand), 24 + index * 4))
    bytes.set(trailing, boxSize)
    return bytes
  }
  const openWithStubVideo = async (bytes: Uint8Array<ArrayBuffer>, name: string) => {
    const fixture = runtime(bytes, BigInt(bytes.byteLength))
    const openVideo = vi.fn(async () => {
      throw new VideoPreviewError('stub video pipeline')
    })
    const result = FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), name),
      fixture.revisions, fixture.broker, new AbortController().signal, { openVideo },
    )
    return { result, openVideo, fixture }
  }

  it('rejects a real ffmpeg/libaom still AVIF before the MP4 pipeline', async () => {
    const { result, openVideo } = await openWithStubVideo(realFixture('still-ffmpeg-libaom.avif'), 'still.avif')
    await expect(result).rejects.toMatchObject({ name: 'ImagePreviewError' })
    expect(openVideo).not.toHaveBeenCalled()
  })

  it.each([
    ['real ffmpeg AVIF image sequence (avis/msf1)', realFixture('sequence-ffmpeg-libaom.avif'), 'seq.avif'],
    ['real ffmpeg H.264 MP4', realFixture('h264-no-edit-list.mp4'), 'clip.mp4'],
    ['extended ftyp isom with minor_version heic', extendedFtyp('isom', 'heic', ['isom', 'mp41']), 'clip.mp4'],
    ['extended ftyp isom followed by free payload mif1',
      extendedFtyp('isom', '\0\0\0\0', ['isom'], [0, 0, 0, 12, 102, 114, 101, 101, 109, 105, 102, 49]), 'clip.mp4'],
  ] as const)('keeps %s on the video pipeline', async (_label, bytes, name) => {
    const { result, openVideo } = await openWithStubVideo(new Uint8Array(bytes), name)
    await expect(result).rejects.toBeInstanceOf(VideoPreviewError)
    expect(openVideo).toHaveBeenCalledOnce()
  })

  // Real H.264 fixture re-branded to major mif1 with its video brands pushed past
  // the 64 KiB sniff window (stco offsets shifted so the file stays decodable).
  const longFtypVideo = (extended: boolean) => {
    const source = realFixture('h264-no-edit-list.mp4')
    const sourceView = new DataView(source.buffer, source.byteOffset, source.byteLength)
    const oldSize = sourceView.getUint32(0, false)
    const header = extended ? 24 : 16
    const brands = new Uint8Array(16384 * 4 + (oldSize - 16))
    brands.fill(122, 0, 16384 * 4)
    brands.set(source.subarray(16, oldSize), 16384 * 4)
    const newSize = header + brands.byteLength
    const out = new Uint8Array(newSize + source.byteLength - oldSize)
    const view = new DataView(out.buffer)
    view.setUint32(0, extended ? 1 : newSize, false)
    out.set([102, 116, 121, 112], 4)
    if (extended) view.setBigUint64(8, BigInt(newSize), false)
    out.set([109, 105, 102, 49], header - 8)
    out.set(brands, header)
    out.set(source.subarray(oldSize), newSize)
    const delta = newSize - oldSize
    for (let at = newSize; at + 4 <= out.byteLength; at++) {
      if (out[at] !== 115 || out[at + 1] !== 116 || out[at + 2] !== 99 || out[at + 3] !== 111) continue
      const count = view.getUint32(at + 8, false)
      for (let entry = 0; entry < count; entry++) {
        const offset = at + 12 + entry * 4
        view.setUint32(offset, view.getUint32(offset, false) + delta, false)
      }
    }
    return out
  }

  it.each([false, true])('keeps a mif1-major MP4 whose video brands lie past the sniff window on the video path (extended=%s)', async (extended) => {
    const { result, openVideo } = await openWithStubVideo(longFtypVideo(extended), 'clip.mp4')
    await expect(result).rejects.toBeInstanceOf(VideoPreviewError)
    expect(openVideo).toHaveBeenCalledOnce()
  })

  it('keeps a real H.264 MP4 re-branded to major iso7 with only mif1 compatible brands on the video path', async () => {
    const bytes = realFixture('h264-no-edit-list.mp4').slice()
    bytes.set([105, 115, 111, 55], 8)
    for (let at = 16; at < 32; at += 4) bytes.set([109, 105, 102, 49], at)
    const { result, openVideo } = await openWithStubVideo(bytes, 'clip.mp4')
    await expect(result).rejects.toBeInstanceOf(VideoPreviewError)
    expect(openVideo).toHaveBeenCalledOnce()
  })

  it('rejects an extended-size ftyp with a genuine HEIC major brand', async () => {
    const { result, openVideo } = await openWithStubVideo(extendedFtyp('heic', '\0\0\0\0', ['mif1', 'heic']), 'photo.heic')
    await expect(result).rejects.toMatchObject({ name: 'ImagePreviewError' })
    expect(openVideo).not.toHaveBeenCalled()
  })
})
