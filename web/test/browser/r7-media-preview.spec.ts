import { expect, test } from '@playwright/test'
import { createFile } from 'mp4box'

import {
  r8PerformanceSampleCount,
  reportR8Trend,
  summarizeR8Metric,
} from '../performance/r8-trend'

const MP4_FIXTURE_BYTES = 768 * 1024
const MP4_BLOCK_BYTES = 16 * 1024
const MP4_SAMPLE_BYTES = 9
const MP4_TIMESCALE = 1_000
const MP4_DURATION_UNITS = 2_000
const MP4_WIDTH = 16
const MP4_HEIGHT = 16
const HERO_WIDTH = 343
const HERO_HEIGHT = 361
const HERO_BYTES = 13_057

const MP4_FIXTURE_BASE64 = Buffer.from(buildDeterministicMp4Fixture()).toString('base64')

test('runs production image decode and bounded MP4 seek semantics in the active browser', async ({ page }) => {
  await page.goto('/')
  const evidence = await page.evaluate(async ({ mp4Base64, mp4BlockBytes }) => {
    const previewPath = '/src/preview/v2-preview.ts'
    const imageHeaderPath = '/src/preview/image-header.ts'
    const mp4RangePath = '/src/preview/mp4-range.ts'
    const geometryPath = '/src/content/geometry.ts'
    const brokerPath = '/src/content/v2-broker.ts'
    const connectivityPath = '/src/connectivity/v2-receiver-policy.ts'
    const [previewModule, imageHeaderModule, mp4RangeModule, geometryModule, brokerModule,
      connectivityModule] = await Promise.all([
      import(previewPath) as Promise<typeof import('../../src/preview/v2-preview')>,
      import(imageHeaderPath) as Promise<typeof import('../../src/preview/image-header')>,
      import(mp4RangePath) as Promise<typeof import('../../src/preview/mp4-range')>,
      import(geometryPath) as Promise<typeof import('../../src/content/geometry')>,
      import(brokerPath) as Promise<typeof import('../../src/content/v2-broker')>,
      import(connectivityPath) as Promise<typeof import('../../src/connectivity/v2-receiver-policy')>,
    ])

    type CatalogFile = Extract<
    import('../../src/catalog/v2-records').V2CatalogEntry,
    { kind: 'file' }
    >
    type Descriptor = import('../../src/content/v2-records').V2FileRevisionDescriptor
    type RangeReader = import('../../src/content/v2-broker').V2BlockRangeReader
    type BlockLane = import('../../src/content/v2-broker').V2BlockLane
    type RevisionReader = import('../../src/content/v2-session-services').V2RevisionReader

    interface ReadObservation {
      readonly start: number
      readonly end: number
      readonly priority: string
    }

    function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
      return left.byteLength === right.byteLength && left.every((value, index) => value === right[index])
    }

    function identity(first: number): Uint8Array<ArrayBuffer> {
      const value = new Uint8Array(16)
      value[0] = first
      return value
    }

    function createRuntime(
      bytes: Uint8Array<ArrayBuffer>,
      name: string,
      identitySeed: number,
      blockBytes: number,
    ) {
      const descriptor: Descriptor = Object.freeze({
        shareInstance: identity(identitySeed),
        shareInstanceId: `share-${identitySeed}`,
        fileId: identity(identitySeed + 1),
        fileIdText: `file-${identitySeed}`,
        fileRevision: identity(identitySeed + 2),
        fileRevisionText: `revision-${identitySeed}`,
        exactSize: BigInt(bytes.byteLength),
        geometry: new geometryModule.FileGeometry(BigInt(bytes.byteLength), BigInt(blockBytes)),
      })
      const leaseId = identity(identitySeed + 3)
      const ranges: ReadObservation[] = []
      const upstreamBlocks: bigint[] = []
      let releases = 0

      const lane: BlockLane = {
        id: identitySeed,
        async fetchBlock(demand, signal) {
          signal.throwIfAborted()
          if (demand.descriptor !== descriptor || !equalBytes(demand.leaseId, leaseId)) {
            throw new Error('Preview broker received a block outside its opened revision lease')
          }
          const range = descriptor.geometry.blockPlaintext(demand.localBlockIndex)
          upstreamBlocks.push(demand.localBlockIndex)
          return Object.freeze({
            descriptor,
            localBlockIndex: demand.localBlockIndex,
            data: bytes.slice(Number(range.start), Number(range.end)),
          })
        },
      }
      const lanes = new brokerModule.V2LaneSet()
      lanes.add(lane, 'relay')
      const productionBroker = new brokerModule.V2BlockBroker(lanes)
      const routes = new connectivityModule.V2ConnectivityRouteAuthority()
      routes.admitRelay()
      const recordingBroker: RangeReader = {
        readRange(candidate, candidateLease, range, options) {
          options?.signal?.throwIfAborted()
          if (candidate !== descriptor || !equalBytes(candidateLease, leaseId)) {
            throw new Error('Preview range escaped its opened revision lease')
          }
          const checked = descriptor.geometry.requireRange(range)
          ranges.push({
            start: Number(checked.start),
            end: Number(checked.end),
            priority: options?.priority ?? 'download',
          })
          return productionBroker.readRouteAuthorizedRange(candidate, candidateLease, checked, {
            ...options,
            routes,
          })
        },
      }
      const revisions: RevisionReader = {
        async open(fileId, signal) {
          signal?.throwIfAborted()
          if (!equalBytes(fileId, descriptor.fileId)) {
            throw new Error('Preview opened a different file identity')
          }
          return {
            descriptor,
            leaseId: leaseId.slice(),
            release: async () => { releases += 1 },
          }
        },
      }
      const entry: CatalogFile = Object.freeze({
        kind: 'file',
        id: descriptor.fileId,
        idText: descriptor.fileIdText,
        name,
        expectedSize: descriptor.exactSize,
      })

      return {
        broker: recordingBroker,
        entry,
        ranges,
        releases: () => releases,
        revisions,
        upstreamBytes: () => [...new Set(upstreamBlocks)].reduce((total, block) => {
          const range = descriptor.geometry.blockPlaintext(block)
          return total + Number(range.end - range.start)
        }, 0),
        upstreamBlocks: () => [...new Set(upstreamBlocks)].map(Number).sort((left, right) => left - right),
        close: () => {
          productionBroker.close()
          lanes.close()
        },
      }
    }

    async function objectUrlIsRevoked(url: string): Promise<boolean> {
      try {
        await fetch(url)
        return false
      } catch {
        return true
      }
    }

    function previewCurrentIsCleared(preview: { readonly current: unknown }): boolean {
      try {
        return preview.current === undefined
      } catch {
        return true
      }
    }

    const heroResponse = await fetch('/src/assets/hero.png')
    if (!heroResponse.ok) throw new Error(`Repository PNG returned HTTP ${heroResponse.status}`)
    const heroBytes = new Uint8Array(await heroResponse.arrayBuffer())
    const imageRuntime = createRuntime(heroBytes, 'hero.png', 1, 16 * 1024)
    const imagePreview = await previewModule.V2FilePreview.open(
      imageRuntime.entry,
      imageRuntime.revisions,
      imageRuntime.broker,
      new AbortController().signal,
    )
    const image = imagePreview.current
    if (image.kind !== 'image') throw new Error('Repository PNG did not produce an image presentation')
    const imageUrlResponse = await fetch(image.url)
    const imageUrlBlob = await imageUrlResponse.blob()
    const imageUrlBytes = new Uint8Array(await imageUrlBlob.arrayBuffer())
    const element = new Image()
    element.alt = 'R7 repository PNG evidence'
    element.src = image.url
    document.body.append(element)
    await element.decode()
    const imageRendered = {
      complete: element.complete,
      connected: element.isConnected,
      height: element.naturalHeight,
      source: element.currentSrc,
      width: element.naturalWidth,
    }
    element.remove()
    const imageReleaseAtOpen = imageRuntime.releases()
    await Promise.all([imagePreview.close(), imagePreview.close()])
    const imageCurrentCleared = previewCurrentIsCleared(imagePreview)
    const imageEvidence = {
      currentCleared: imageCurrentCleared,
      exactPayload: equalBytes(imageUrlBytes, heroBytes),
      height: image.height,
      mimeType: image.mimeType,
      objectUrl: image.url,
      objectUrlMimeType: imageUrlBlob.type,
      ranges: imageRuntime.ranges,
      releaseAtOpen: imageReleaseAtOpen,
      releaseAfterClose: imageRuntime.releases(),
      rendered: imageRendered,
      revokedAfterClose: await objectUrlIsRevoked(image.url),
      upstreamBlocks: imageRuntime.upstreamBlocks(),
      width: image.width,
    }
    imageRuntime.close()

    const binary = atob(mp4Base64)
    const mp4Bytes = Uint8Array.from(binary, (value) => value.charCodeAt(0))
    const videoRuntime = createRuntime(mp4Bytes, 'bounded-two-sample.mp4', 10, mp4BlockBytes)
    // canPlayType is intentionally outside this oracle: the production container parser,
    // authenticated broker reads, segmentation, Blob construction, and seek lifecycle are portable.
    const videoPreview = await previewModule.V2FilePreview.open(
      videoRuntime.entry,
      videoRuntime.revisions,
      videoRuntime.broker,
      new AbortController().signal,
      { supportsVideo: () => true },
    )
    const initial = videoPreview.current
    if (initial.kind !== 'video') throw new Error('MP4 fixture did not produce a video presentation')
    const initialResponse = await fetch(initial.url)
    const initialBlob = await initialResponse.blob()
    const releaseAtInitialSegment = videoRuntime.releases()

    const superseded = videoPreview.seek(0.25).then(
      () => ({ status: 'resolved', name: '' }),
      (error: unknown) => ({
        status: 'rejected',
        name: error instanceof DOMException ? error.name : 'UnexpectedError',
      }),
    )
    const later = await videoPreview.seek(1.5)
    if (later.kind !== 'video') throw new Error('MP4 seek changed the presentation kind')
    const supersededResult = await superseded
    const laterResponse = await fetch(later.url)
    const laterBlob = await laterResponse.blob()
    const initialRevokedAfterSeek = await objectUrlIsRevoked(initial.url)

    const rangesBeforeClose = videoRuntime.ranges.slice()
    const upstreamBlocks = videoRuntime.upstreamBlocks()
    const upstreamBytes = videoRuntime.upstreamBytes()
    await Promise.all([videoPreview.close(), videoPreview.close()])
    const videoCurrentCleared = previewCurrentIsCleared(videoPreview)
    const videoEvidence = {
      codecBoundary: 'container-range-and-blob-only',
      currentCleared: videoCurrentCleared,
      durationSeconds: initial.durationSeconds,
      fileBytes: mp4Bytes.byteLength,
      height: initial.height,
      initial: {
        blobBytes: initialBlob.size,
        blobMimeType: initialBlob.type,
        objectUrl: initial.url,
        positionSeconds: initial.positionSeconds,
      },
      initialRevokedAfterSeek,
      later: {
        blobBytes: laterBlob.size,
        blobMimeType: laterBlob.type,
        objectUrl: later.url,
        positionSeconds: later.positionSeconds,
      },
      latestRevokedAfterClose: await objectUrlIsRevoked(later.url),
      limits: {
        edgeMetadataBytes: mp4RangeModule.V2_VIDEO_EDGE_METADATA_BYTES,
        headerSniffBytes: imageHeaderModule.V2_IMAGE_HEADER_BYTES,
        sampleBytes: mp4RangeModule.V2_VIDEO_SAMPLE_BYTES,
      },
      mimeType: initial.mimeType,
      ranges: rangesBeforeClose,
      releaseAtInitialSegment,
      releaseAfterClose: videoRuntime.releases(),
      superseded: supersededResult,
      upstreamBlocks,
      upstreamBytes,
      width: initial.width,
    }
    videoRuntime.close()

    return { image: imageEvidence, video: videoEvidence }
  }, { mp4Base64: MP4_FIXTURE_BASE64, mp4BlockBytes: MP4_BLOCK_BYTES })
  expect(evidence.image).toMatchObject({
    currentCleared: true,
    exactPayload: true,
    height: HERO_HEIGHT,
    mimeType: 'image/png',
    objectUrlMimeType: 'image/png',
    releaseAtOpen: 1,
    releaseAfterClose: 1,
    revokedAfterClose: true,
    width: HERO_WIDTH,
  })
  expect(evidence.image.objectUrl).toMatch(/^blob:http:\/\/127\.0\.0\.1:4173\//u)
  expect(evidence.image.rendered).toEqual({
    complete: true,
    connected: true,
    height: HERO_HEIGHT,
    source: evidence.image.objectUrl,
    width: HERO_WIDTH,
  })
  expect(evidence.image.ranges).toEqual([
    { start: 0, end: HERO_BYTES, priority: 'preview' },
    { start: 0, end: HERO_BYTES, priority: 'preview' },
  ])
  expect(evidence.image.upstreamBlocks).toEqual([0])

  expect(evidence.video).toMatchObject({
    codecBoundary: 'container-range-and-blob-only',
    currentCleared: true,
    durationSeconds: 2,
    fileBytes: MP4_FIXTURE_BYTES,
    height: MP4_HEIGHT,
    initial: {
      blobMimeType: 'video/mp4',
      positionSeconds: 0,
    },
    initialRevokedAfterSeek: true,
    later: {
      blobMimeType: 'video/mp4',
      positionSeconds: 1,
    },
    latestRevokedAfterClose: true,
    mimeType: 'video/mp4; codecs="avc1.42001e"',
    releaseAtInitialSegment: 0,
    releaseAfterClose: 1,
    superseded: { status: 'rejected', name: 'AbortError' },
    width: MP4_WIDTH,
  })
  expect(evidence.video.initial.objectUrl).toMatch(/^blob:http:\/\/127\.0\.0\.1:4173\//u)
  expect(evidence.video.later.objectUrl).toMatch(/^blob:http:\/\/127\.0\.0\.1:4173\//u)
  expect(evidence.video.initial.blobBytes).toBeGreaterThan(0)
  expect(evidence.video.later.blobBytes).toBeGreaterThan(0)

  const [sniff, head, tail, ...samples] = evidence.video.ranges
  expect(sniff).toEqual({
    start: 0,
    end: evidence.video.limits.headerSniffBytes,
    priority: 'preview',
  })
  expect(head).toEqual({
    start: 0,
    end: evidence.video.limits.edgeMetadataBytes,
    priority: 'preview',
  })
  expect(tail).toEqual({
    start: MP4_FIXTURE_BYTES - evidence.video.limits.edgeMetadataBytes,
    end: MP4_FIXTURE_BYTES,
    priority: 'preview',
  })
  expect(samples).toHaveLength(2)
  expect(samples.map((range) => range.end - range.start)).toEqual([
    MP4_SAMPLE_BYTES,
    MP4_SAMPLE_BYTES,
  ])
  expect(samples.every((range) => range.end - range.start <= evidence.video.limits.sampleBytes)).toBe(true)
  expect(evidence.video.ranges.some((range) => range.start === 0 && range.end === MP4_FIXTURE_BYTES))
    .toBe(false)
  expect(evidence.video.ranges.reduce((total, range) => total + range.end - range.start, 0))
    .toBeLessThan(MP4_FIXTURE_BYTES)
  expect(evidence.video.upstreamBytes).toBeLessThan(MP4_FIXTURE_BYTES)
  expect(new Set(evidence.video.upstreamBlocks).size).toBe(evidence.video.upstreamBlocks.length)
})

test('records environment-qualified production media preview trends', async ({ browserName, page }) => {
  const trendSampleCount = r8PerformanceSampleCount()
  await page.goto('/')
  const trend = await page.evaluate(measureR8MediaTrend, {
    heroHeight: HERO_HEIGHT,
    heroWidth: HERO_WIDTH,
    mp4Base64: MP4_FIXTURE_BASE64,
    mp4BlockBytes: MP4_BLOCK_BYTES,
    mp4Height: MP4_HEIGHT,
    mp4Width: MP4_WIDTH,
    sampleCount: trendSampleCount,
  })

  expect(trend).toHaveLength(trendSampleCount)
  reportR8Trend({
    browser: browserName,
    scenario: 'media-preview',
    workload: {
      samples: trendSampleCount,
      imageBytes: HERO_BYTES,
      mp4Bytes: MP4_FIXTURE_BYTES,
      mp4BlockBytes: MP4_BLOCK_BYTES,
    },
    capabilities: { imageDecode: true, mp4RangeParser: true },
    unavailable: {},
    metrics: {
      imageOpenMilliseconds: summarizeR8Metric(
        trend.map((sample) => sample.imageOpenMilliseconds),
      ),
      imageFirstFrameMilliseconds: summarizeR8Metric(
        trend.map((sample) => sample.imageFirstFrameMilliseconds),
      ),
      videoMetadataMilliseconds: summarizeR8Metric(
        trend.map((sample) => sample.videoMetadataMilliseconds),
      ),
      videoSeekMilliseconds: summarizeR8Metric(
        trend.map((sample) => sample.videoSeekMilliseconds),
      ),
    },
  })
})

async function measureR8MediaTrend(options: {
  readonly heroHeight: number
  readonly heroWidth: number
  readonly mp4Base64: string
  readonly mp4BlockBytes: number
  readonly mp4Height: number
  readonly mp4Width: number
  readonly sampleCount: number
}) {
  const previewPath = '/src/preview/v2-preview.ts'
  const geometryPath = '/src/content/geometry.ts'
  const brokerPath = '/src/content/v2-broker.ts'
  const connectivityPath = '/src/connectivity/v2-receiver-policy.ts'
  const [previewModule, geometryModule, brokerModule, connectivityModule] = await Promise.all([
    import(previewPath) as Promise<typeof import('../../src/preview/v2-preview')>,
    import(geometryPath) as Promise<typeof import('../../src/content/geometry')>,
    import(brokerPath) as Promise<typeof import('../../src/content/v2-broker')>,
    import(connectivityPath) as Promise<typeof import('../../src/connectivity/v2-receiver-policy')>,
  ])
  type CatalogFile = Extract<
    import('../../src/catalog/v2-records').V2CatalogEntry,
    { kind: 'file' }
  >
  type ByteRange = import('../../src/content/geometry').ByteRange
  type Descriptor = import('../../src/content/v2-records').V2FileRevisionDescriptor
  type BlockLane = import('../../src/content/v2-broker').V2BlockLane
  type RangeReader = import('../../src/content/v2-broker').V2BlockRangeReader
  type RangeReaderOptions = import('../../src/content/v2-broker').V2BlockRangeReaderOptions
  type RevisionReader = import('../../src/content/v2-session-services').V2RevisionReader

  function identity(first: number): Uint8Array<ArrayBuffer> {
    const value = new Uint8Array(16)
    value.set([first])
    return value
  }

  function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
    if (left.byteLength !== right.byteLength) return false
    for (let index = 0; index < left.byteLength; index += 1) {
      if (left[index] !== right[index]) return false
    }
    return true
  }

  function createRuntime(
    bytes: Uint8Array<ArrayBuffer>,
    name: string,
    seed: number,
    blockBytes: number,
  ) {
    const descriptor: Descriptor = Object.freeze({
      shareInstance: identity(seed),
      shareInstanceId: `r8-share-${seed}`,
      fileId: identity(seed + 1),
      fileIdText: `r8-file-${seed}`,
      fileRevision: identity(seed + 2),
      fileRevisionText: `r8-revision-${seed}`,
      exactSize: BigInt(bytes.byteLength),
      geometry: new geometryModule.FileGeometry(BigInt(bytes.byteLength), BigInt(blockBytes)),
    })
    const leaseId = identity(seed + 3)
    const upstream = new Set<bigint>()
    const lane: BlockLane = {
      id: seed,
      async fetchBlock(demand, signal) {
        signal.throwIfAborted()
        if (demand.descriptor !== descriptor || !equalBytes(demand.leaseId, leaseId)) {
          throw new Error('R8 media trend escaped its opened revision')
        }
        upstream.add(demand.localBlockIndex)
        const range = descriptor.geometry.blockPlaintext(demand.localBlockIndex)
        return Object.freeze({
          descriptor,
          localBlockIndex: demand.localBlockIndex,
          data: bytes.slice(Number(range.start), Number(range.end)),
        })
      },
    }
    const lanes = new brokerModule.V2LaneSet()
    lanes.add(lane, 'relay')
    const rawBroker = new brokerModule.V2BlockBroker(lanes)
    const routes = new connectivityModule.V2ConnectivityRouteAuthority()
    routes.admitRelay()
    // Preview stays route-neutral so only this fixture boundary can supply activation authority.
    const broker: RangeReader = Object.freeze({
      readRange: (
        candidate: Descriptor,
        candidateLease: Uint8Array,
        range: ByteRange,
        options: RangeReaderOptions = {},
      ) =>
        rawBroker.readRouteAuthorizedRange(candidate, candidateLease, range, {
          ...options,
          routes,
        }),
    })
    const revisions: RevisionReader = {
      async open(fileId, signal) {
        signal?.throwIfAborted()
        if (!equalBytes(fileId, descriptor.fileId)) {
          throw new Error('R8 media trend opened another file')
        }
        return { descriptor, leaseId: leaseId.slice(), release: async () => undefined }
      },
    }
    const entry: CatalogFile = Object.freeze({
      kind: 'file',
      id: descriptor.fileId,
      idText: descriptor.fileIdText,
      name,
      expectedSize: descriptor.exactSize,
    })
    return {
      broker,
      entry,
      revisions,
      upstreamBytes: () => [...upstream].reduce((total, block) => {
        const range = descriptor.geometry.blockPlaintext(block)
        return total + Number(range.end - range.start)
      }, 0),
      close: () => {
        routes.close()
        rawBroker.close()
        lanes.close()
      },
    }
  }

  const heroResponse = await fetch('/src/assets/hero.png')
  if (!heroResponse.ok) throw new Error(`Repository PNG returned HTTP ${heroResponse.status}`)
  const heroBytes = new Uint8Array(await heroResponse.arrayBuffer())
  const binary = atob(options.mp4Base64)
  const mp4Bytes = Uint8Array.from(binary, (value) => value.charCodeAt(0))
  const trend = []
  for (let sample = 0; sample < options.sampleCount; sample += 1) {
    const imageRuntime = createRuntime(heroBytes, 'hero.png', 40 + sample * 4, 16 * 1024)
    const imageStartedAt = performance.now()
    const imagePreview = await previewModule.V2FilePreview.open(
      imageRuntime.entry,
      imageRuntime.revisions,
      imageRuntime.broker,
      new AbortController().signal,
    )
    const imageOpenedAt = performance.now()
    const image = imagePreview.current
    if (image.kind !== 'image' || image.width !== options.heroWidth ||
        image.height !== options.heroHeight) {
      throw new Error('R8 image trend escaped the production dimension oracle')
    }
    const element = new Image()
    element.src = image.url
    document.body.append(element)
    await element.decode()
    const imageFirstFrameAt = performance.now()
    if (element.naturalWidth !== options.heroWidth || element.naturalHeight !== options.heroHeight) {
      throw new Error('R8 image trend decoded unexpected dimensions')
    }
    element.remove()
    await imagePreview.close()
    imageRuntime.close()

    const videoRuntime = createRuntime(
      mp4Bytes,
      'bounded-two-sample.mp4',
      100 + sample * 4,
      options.mp4BlockBytes,
    )
    const videoStartedAt = performance.now()
    const videoPreview = await previewModule.V2FilePreview.open(
      videoRuntime.entry,
      videoRuntime.revisions,
      videoRuntime.broker,
      new AbortController().signal,
      { supportsVideo: () => true },
    )
    const videoMetadataAt = performance.now()
    const video = videoPreview.current
    if (video.kind !== 'video' || video.width !== options.mp4Width ||
        video.height !== options.mp4Height || video.durationSeconds !== 2) {
      throw new Error('R8 video trend escaped the production metadata oracle')
    }
    const seekStartedAt = performance.now()
    const sought = await videoPreview.seek(1.5)
    const seekCompletedAt = performance.now()
    if (sought.kind !== 'video' || sought.positionSeconds !== 1 ||
        videoRuntime.upstreamBytes() >= mp4Bytes.byteLength) {
      throw new Error('R8 video trend escaped bounded seek semantics')
    }
    await videoPreview.close()
    videoRuntime.close()
    trend.push({
      imageOpenMilliseconds: imageOpenedAt - imageStartedAt,
      imageFirstFrameMilliseconds: imageFirstFrameAt - imageStartedAt,
      videoMetadataMilliseconds: videoMetadataAt - videoStartedAt,
      videoSeekMilliseconds: seekCompletedAt - seekStartedAt,
    })
  }
  return trend
}

function buildDeterministicMp4Fixture(): Uint8Array<ArrayBuffer> {
  const sequenceParameterSet = Uint8Array.from([
    0x67, 0x42, 0x00, 0x1e, 0x95, 0xa8, 0x28, 0x0f, 0x00, 0x44, 0x7a, 0x10, 0x00,
    0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0x20, 0xf1, 0x83, 0x19, 0x60,
  ])
  const pictureParameterSet = Uint8Array.from([0x68, 0xce, 0x3c, 0x80])
  const decoderConfiguration = new Uint8Array(
    11 + sequenceParameterSet.byteLength + pictureParameterSet.byteLength,
  )
  let offset = 0
  decoderConfiguration.set([
    1,
    0x42,
    0,
    0x1e,
    0xff,
    0xe1,
    sequenceParameterSet.byteLength >>> 8,
    sequenceParameterSet.byteLength & 0xff,
  ], offset)
  offset += 8
  decoderConfiguration.set(sequenceParameterSet, offset)
  offset += sequenceParameterSet.byteLength
  decoderConfiguration.set([
    1,
    pictureParameterSet.byteLength >>> 8,
    pictureParameterSet.byteLength & 0xff,
  ], offset)
  offset += 3
  decoderConfiguration.set(pictureParameterSet, offset)

  const file = createFile()
  const trackId = file.addTrack({
    avcDecoderConfigRecord: decoderConfiguration.buffer,
    brands: ['isom', 'iso6', 'avc1'],
    duration: MP4_DURATION_UNITS,
    height: MP4_HEIGHT,
    media_duration: MP4_DURATION_UNITS,
    timescale: MP4_TIMESCALE,
    type: 'avc1',
    width: MP4_WIDTH,
  })
  file.addSample(trackId, avcSample(1), {
    cts: 0,
    dts: 0,
    duration: MP4_TIMESCALE,
    is_sync: true,
  })
  file.addSample(trackId, avcSample(9), {
    cts: MP4_TIMESCALE,
    dts: MP4_TIMESCALE,
    duration: MP4_TIMESCALE,
    is_sync: true,
  })
  const stream = file.getBuffer()
  const core = new Uint8Array(stream.buffer, 0, stream.byteLength)
  if (core.byteLength + 8 >= MP4_FIXTURE_BYTES) {
    throw new Error('Generated MP4 core leaves no room for the bounded-read fixture padding')
  }
  const bytes = new Uint8Array(MP4_FIXTURE_BYTES)
  bytes.set(core)
  const freeBytes = bytes.byteLength - core.byteLength
  new DataView(bytes.buffer).setUint32(core.byteLength, freeBytes, false)
  bytes.set([0x66, 0x72, 0x65, 0x65], core.byteLength + 4)
  return bytes
}

function avcSample(seed: number): Uint8Array<ArrayBuffer> {
  return Uint8Array.from([0, 0, 0, 5, 0x65, seed, seed + 1, seed + 2, seed + 3])
}
