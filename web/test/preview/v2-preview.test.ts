import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { byteRange, FileGeometry, type ByteRange } from '../../src/content/geometry'
import {
  V2BlockBroker,
  V2LaneSet,
  type V2BlockDemand,
  type V2BlockLane,
  type V2BlockRangeReader,
  type V2BlockRouteEligibility,
} from '../../src/content/v2-broker'
import type { V2BlockRecord, V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import type { V2Mp4Segment, V2PreviewRangeSource } from '../../src/preview/mp4-range'
import {
  V2FilePreview,
  type V2PreviewPorts,
  type V2VideoRangePort,
} from '../../src/preview/v2-preview'

const ALL_ROUTES: V2BlockRouteEligibility = Object.freeze({
  active: true,
  allows: () => true,
  assertActive: () => undefined,
  subscribe: () => () => undefined,
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function fileEntry(size: bigint, name = 'preview.bin'): Extract<V2CatalogEntry, { kind: 'file' }> {
  return Object.freeze({ kind: 'file', id: identity(2), idText: 'file', name, expectedSize: size })
}

class ByteLane implements V2BlockLane {
  readonly id = 1
  readonly calls: bigint[] = []
  readonly #bytes: Uint8Array<ArrayBuffer>

  constructor(bytes: Uint8Array<ArrayBuffer>) {
    this.#bytes = bytes
  }

  async fetchBlock(demand: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
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
  const descriptor: V2FileRevisionDescriptor = Object.freeze({
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
  const lanes = new V2LaneSet()
  lanes.add(lane, 'relay')
  const upstreamBroker = new V2BlockBroker(lanes)
  const broker: V2BlockRangeReader = Object.freeze({
    readRange: (
      revision: V2FileRevisionDescriptor,
      leaseId: Uint8Array,
      range: ByteRange,
      options: NonNullable<Parameters<V2BlockRangeReader['readRange']>[3]> = {},
    ) => upstreamBroker.readRange(
      revision,
      leaseId,
      range,
      { ...options, routes: ALL_ROUTES },
    ),
  })
  let releases = 0
  const revisions = {
    open: async () => ({
      descriptor,
      leaseId: identity(4),
      release: async () => { releases += 1 },
    }),
  } satisfies V2RevisionReader
  return { broker, descriptor, lane, releases: () => releases, revisions }
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

function mp4ShapedFile(size: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(size)
  new DataView(bytes.buffer).setUint32(0, 24, false)
  bytes.set([102, 116, 121, 112], 4)
  return bytes
}

function urlPorts(extra: V2PreviewPorts = {}) {
  const created: Blob[] = []
  const revoked: string[] = []
  let next = 1
  const ports: V2PreviewPorts = {
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
    const preview = await V2FilePreview.open(
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

  it('cancels a superseded video seek without releasing the preview lease', async () => {
    const bytes = mp4ShapedFile(1 << 20)
    const fixture = runtime(bytes, 64n * 1024n)
    const pending: AbortSignal[] = []
    const openVideo = async (source: V2PreviewRangeSource): Promise<V2VideoRangePort> => {
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
        segmentAt: async (seconds, signal): Promise<V2Mp4Segment> => {
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
          return Object.freeze({ bytes: new Blob([sample], { type: 'video/mp4' }), positionSeconds: seconds })
        },
      }
    }
    const urls = urlPorts({ openVideo, supportsVideo: () => true })
    const preview = await V2FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'clip.mp4'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )
    expect(preview.current).toMatchObject({ kind: 'video', url: 'blob:preview-1', positionSeconds: 0 })
    expect(fixture.releases()).toBe(0)

    const superseded = preview.seek(1)
    await Promise.resolve()
    const latest = preview.seek(2)
    await expect(superseded).rejects.toMatchObject({ name: 'AbortError' })
    await expect(latest).resolves.toMatchObject({ url: 'blob:preview-2', positionSeconds: 2 })
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
    await expect(V2FilePreview.open(
      fileEntry(BigInt(bytes.byteLength), 'forged.png'),
      fixture.revisions,
      fixture.broker,
      new AbortController().signal,
      urls.ports,
    )).rejects.toThrow('dimensions disagree')
    expect(urls.created).toHaveLength(0)
    expect(fixture.releases()).toBe(1)
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
          return { bytes: new Blob(), positionSeconds: 0 }
        },
      }),
      supportsVideo: () => false,
    })
    await expect(V2FilePreview.open(
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
