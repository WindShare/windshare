import {
  CIPHER_SUITE_V1,
  MAX_CHUNK_BYTES,
  MAX_CHUNK_COUNT,
  MIN_CHUNK_BYTES,
  type ChunkIndex,
} from '../contracts'
import { copyBytes, encodeUint64 } from './bytes'
import { CryptoError } from './errors'
import {
  DERIVED_KEY_BYTES,
  deriveSegmentKey,
  deriveStreamKey,
} from './key-derivation'
import {
  type CryptoRuntime,
  defaultCryptoRuntime,
  importAesGcmKey,
} from './webcrypto'

export const SEGMENT_BYTES = 16 * 1_024 * 1_024 * 1_024
export const GCM_NONCE_BYTES = 12
export const GCM_TAG_BYTES = 16

function suiteTrailerBytes(suite: number): number {
  if (suite === CIPHER_SUITE_V1) {
    return 0
  }
  throw new CryptoError(
    'unsupported-suite',
    'Block uses an unsupported cipher suite; upgrade required',
  )
}

function requireChunkSize(chunkSize: number): void {
  if (
    !Number.isSafeInteger(chunkSize) ||
    chunkSize < MIN_CHUNK_BYTES ||
    chunkSize > MAX_CHUNK_BYTES ||
    !Number.isInteger(Math.log2(chunkSize))
  ) {
    throw new CryptoError(
      'invalid-chunk-size',
      `Chunk size must be a power of two in [${MIN_CHUNK_BYTES}, ${MAX_CHUNK_BYTES}]`,
    )
  }
}

export function sealedBlockSize(suite: number, plaintextBytes: number): number {
  if (!Number.isSafeInteger(plaintextBytes) || plaintextBytes < 0) {
    throw new CryptoError(
      'invalid-chunk-size',
      'Plaintext size must be a non-negative safe integer',
    )
  }
  const size = plaintextBytes + GCM_NONCE_BYTES + GCM_TAG_BYTES + suiteTrailerBytes(suite)
  if (!Number.isSafeInteger(size)) {
    throw new CryptoError('invalid-chunk-size', 'Sealed block size exceeds safe integer range')
  }
  return size
}

export function maximumSealedBlockSize(suite: number, chunkSize: number): number {
  requireChunkSize(chunkSize)
  return sealedBlockSize(suite, chunkSize)
}

export class ChunkOpener {
  readonly maxSealedBytes: number

  readonly #suite: number
  readonly #chunksPerSegment: number
  readonly #runtime: CryptoRuntime
  readonly #streamKey: Uint8Array<ArrayBuffer>
  readonly #segmentKeys = new Map<number, CryptoKey>()

  private constructor(
    suite: number,
    chunkSize: number,
    streamKey: Uint8Array<ArrayBuffer>,
    runtime: CryptoRuntime,
  ) {
    this.#suite = suite
    this.#chunksPerSegment = SEGMENT_BYTES / chunkSize
    this.#streamKey = streamKey
    this.#runtime = runtime
    this.maxSealedBytes = maximumSealedBlockSize(suite, chunkSize)
  }

  static async create(
    suite: number,
    readSecret: Uint8Array,
    chunkSize: number,
    runtime: CryptoRuntime = defaultCryptoRuntime(),
  ): Promise<ChunkOpener> {
    maximumSealedBlockSize(suite, chunkSize)
    const streamKey = await deriveStreamKey(readSecret, runtime)
    return new ChunkOpener(suite, chunkSize, streamKey, runtime)
  }

  static fromStreamKey(
    suite: number,
    streamKey: Uint8Array,
    chunkSize: number,
    runtime: CryptoRuntime = defaultCryptoRuntime(),
  ): ChunkOpener {
    maximumSealedBlockSize(suite, chunkSize)
    if (streamKey.byteLength !== DERIVED_KEY_BYTES) {
      throw new CryptoError(
        'invalid-key-material',
        `Stream key must be exactly ${DERIVED_KEY_BYTES} bytes`,
      )
    }
    return new ChunkOpener(suite, chunkSize, copyBytes(streamKey), runtime)
  }

  async open(index: number | ChunkIndex, sealedBlock: Uint8Array): Promise<Uint8Array> {
    if (!Number.isSafeInteger(index) || index < 0 || index >= MAX_CHUNK_COUNT) {
      throw new CryptoError(
        'block-index-out-of-range',
        `Block index must be an integer in [0, ${MAX_CHUNK_COUNT})`,
      )
    }
    const minimumBytes = sealedBlockSize(this.#suite, 0)
    if (sealedBlock.byteLength < minimumBytes) {
      throw new CryptoError(
        'block-too-short',
        `Sealed block must be at least ${minimumBytes} bytes`,
      )
    }
    if (sealedBlock.byteLength > this.maxSealedBytes) {
      throw new CryptoError(
        'block-too-long',
        `Sealed block exceeds the ${this.maxSealedBytes}-byte allocation ceiling`,
      )
    }

    const snapshot = copyBytes(sealedBlock)
    const trailerBytes = suiteTrailerBytes(this.#suite)
    const body = snapshot.subarray(0, snapshot.byteLength - trailerBytes)
    const nonce = body.subarray(0, GCM_NONCE_BYTES)
    const ciphertext = body.subarray(GCM_NONCE_BYTES)
    const segment = Math.floor(index / this.#chunksPerSegment)
    const cachedKey = this.#segmentKeys.get(segment)
    const key = cachedKey ?? (await this.#deriveSegmentKey(segment))
    const aad = new Uint8Array(1 + 8)
    aad[0] = this.#suite
    aad.set(encodeUint64(index), 1)

    let plaintext: ArrayBuffer
    try {
      plaintext = await this.#runtime.subtle.decrypt(
        {
          name: 'AES-GCM',
          iv: nonce,
          additionalData: aad,
          tagLength: GCM_TAG_BYTES * 8,
        },
        key,
        ciphertext,
      )
    } catch (cause) {
      throw new CryptoError(
        'authentication-failed',
        `Block ${index} failed AES-GCM authentication`,
        { cause },
      )
    }
    if (cachedKey === undefined && !this.#segmentKeys.has(segment)) {
      // Authentication, not attacker-selected geometry, grants cache residency.
      this.#segmentKeys.set(segment, key)
    }
    return new Uint8Array(plaintext)
  }

  async #deriveSegmentKey(segment: number): Promise<CryptoKey> {
    const rawKey = await deriveSegmentKey(this.#streamKey, segment, this.#runtime)
    return importAesGcmKey(rawKey, this.#runtime)
  }
}

export async function createChunkOpener(
  suite: number,
  readSecret: Uint8Array,
  chunkSize: number,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<ChunkOpener> {
  return ChunkOpener.create(suite, readSecret, chunkSize, runtime)
}

export function createChunkOpenerFromStreamKey(
  suite: number,
  streamKey: Uint8Array,
  chunkSize: number,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): ChunkOpener {
  return ChunkOpener.fromStreamKey(suite, streamKey, chunkSize, runtime)
}
