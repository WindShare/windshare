import { describe, expect, expectTypeOf, it } from 'vitest'

import {
  CIPHER_SUITE_V1,
  MANIFEST_FINGERPRINT_BYTES,
  MANIFEST_VERSION,
  MAX_CHUNK_BYTES,
  MAX_FRAME_BYTES,
  MAX_MTIME_MILLISECONDS,
  MAX_SEALED_MANIFEST_BYTES,
  MAX_STREAM_BYTES,
  MIN_CHUNK_BYTES,
  MIN_FRAME_BYTES,
  MIN_MTIME_MILLISECONDS,
  PATH_POLICY_VERSION,
  PLAN_ID_BYTES,
  PLAN_ID_DOMAIN,
  READ_SECRET_BYTES,
  SHARE_ID_BASE64URL_CHARACTERS,
  SHARE_ID_BYTES,
  type BlockSink,
  type ByteLength,
  type CanonicalPath,
  type CapabilityLink,
  type ChannelState,
  type ChunkSize,
  type CipherSuite,
  type FrameChannel,
  type ManifestEntry,
  type OrderedBlockSink,
  type PlanId,
  type RandomWriteBlockSink,
  type ReadSecret,
  type RelayHint,
  type ShareId,
  type UnixMilliseconds,
  type ValidatedManifestV1,
} from '../../src/contracts'

describe('shared domain constants', () => {
  it('pins the accepted cross-language boundaries', () => {
    expect(CIPHER_SUITE_V1).toBe(0x01)
    expect(SHARE_ID_BYTES).toBe(9)
    expect(SHARE_ID_BASE64URL_CHARACTERS).toBe(12)
    expect(READ_SECRET_BYTES).toBe(16)
    expect(MANIFEST_VERSION).toBe(1)
    expect(PATH_POLICY_VERSION).toBe('windshare/path/v1-unicode-15.0.0')
    expect(MIN_CHUNK_BYTES).toBe(1_024)
    expect(MAX_CHUNK_BYTES).toBe(4_194_304)
    expect(MAX_STREAM_BYTES).toBe(281_474_976_710_656)
    expect(MAX_SEALED_MANIFEST_BYTES).toBe(16_777_216)
    expect(MANIFEST_FINGERPRINT_BYTES).toBe(16)
    expect(MIN_MTIME_MILLISECONDS).toBe(-9_007_199_254_740_991)
    expect(MAX_MTIME_MILLISECONDS).toBe(9_007_199_254_740_991)
    expect(PLAN_ID_BYTES).toBe(32)
    expect(PLAN_ID_DOMAIN).toBe('windshare/v1 transfer-plan\0')
    expect(MIN_FRAME_BYTES).toBe(1)
    expect(MAX_FRAME_BYTES).toBe(65_536)
  })
})

describe('validated type boundary', () => {
  it('keeps raw data out of trusted link and manifest values', () => {
    const path = 'tree/file.txt' as CanonicalPath
    const size = 4 as ByteLength
    const mtime = 0 as UnixMilliseconds
    const entry: ManifestEntry = { kind: 'file', path, size, mtime }

    const link: CapabilityLink = {
      suite: CIPHER_SUITE_V1,
      shareId: 'AAAAAAAAAAAA' as ShareId,
      readSecret: new Uint8Array(READ_SECRET_BYTES) as ReadSecret,
      relayHints: ['https://relay.example'] as unknown as readonly RelayHint[],
    }

    expectTypeOf(link.suite).toEqualTypeOf<CipherSuite>()
    expectTypeOf(entry).toMatchTypeOf<ManifestEntry>()
    expect(link.relayHints).toHaveLength(1)

    // @ts-expect-error A parser must validate the base64url length before branding.
    const rawShareId: ShareId = 'AAAAAAAAAAAA'
    // @ts-expect-error A mutable byte array is not a validated 32-byte plan identity.
    const rawPlanId: PlanId = new Uint8Array(PLAN_ID_BYTES)

    const unvalidatedShape = {
      version: MANIFEST_VERSION,
      chunkSize: MIN_CHUNK_BYTES as ChunkSize,
      entries: [entry] as readonly ManifestEntry[],
    }
    // @ts-expect-error Canonical CBOR/schema validation owns the private manifest brand.
    const rawManifest: ValidatedManifestV1 = unvalidatedShape

    expectTypeOf(rawShareId).toEqualTypeOf<ShareId>()
    expect(rawPlanId).toHaveLength(PLAN_ID_BYTES)
    expect(rawManifest.entries).toHaveLength(1)
  })

  it('models channel lifecycle without transport-specific concepts', () => {
    const channel: FrameChannel = {
      state: 'open',
      frames: new ReadableStream<Uint8Array>(),
      send: () => Promise.resolve(),
      sendTerminal: () => Promise.resolve(),
      close: () => Promise.resolve(),
    }

    expectTypeOf(channel.state).toEqualTypeOf<ChannelState>()
    expectTypeOf(channel.frames).toEqualTypeOf<ReadableStream<Uint8Array>>()
  })

  it('makes delivery order a sink-owned literal capability', () => {
    const createSink = <O extends 'any' | 'ascending'>(deliveryOrder: O): BlockSink<O> => ({
      deliveryOrder,
      has: () => false,
      writeBlock: () => Promise.resolve(),
      finalize: () => Promise.resolve(),
      abort: () => Promise.resolve(),
    })

    const random: RandomWriteBlockSink = createSink('any')
    const ordered: OrderedBlockSink = createSink('ascending')

    expectTypeOf(random.deliveryOrder).toEqualTypeOf<'any'>()
    expectTypeOf(ordered.deliveryOrder).toEqualTypeOf<'ascending'>()

    // @ts-expect-error A random-write sink cannot satisfy ascending delivery.
    const sequential: OrderedBlockSink = random
    expect(sequential.deliveryOrder).toBe('any')
  })
})
