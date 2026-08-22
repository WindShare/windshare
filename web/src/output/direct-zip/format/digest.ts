import {
  directZipCanonicalFrame,
  directZipCanonicalRecord,
  directZipCanonicalU64,
  snapshotDirectZipFixedBytes,
  type DirectZipCanonicalBytes,
} from './canonical'
import { requireDirectZipFsaOffset } from './offset'

export const DIRECT_ZIP_EPOCH_DIGEST_DOMAIN = 'windshare/direct-zip-epoch-digest/v1' as const
export const DIRECT_ZIP_SHA256_BYTES = 32

const SHA256_BLOCK_BYTES = 64
const SHA256_LENGTH_BYTES = 8
const SHA256_MAXIMUM_MESSAGE_BYTES = ((1n << 64n) - 1n) / 8n
const SHA256_INITIAL_STATE = Uint32Array.from([
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
  0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
])

/** Streaming SHA-256 keeps epoch integrity bounded even when an epoch spans the full target. */
export class DirectZipSha256Accumulator {
  readonly #state = Uint32Array.from(SHA256_INITIAL_STATE)
  readonly #tail = new Uint8Array(SHA256_BLOCK_BYTES)
  #tailBytes = 0
  #totalBytes = 0n

  update(bytes: Uint8Array): void {
    if (!(bytes instanceof Uint8Array)) throw new TypeError('direct ZIP SHA-256 input is invalid')
    const nextTotal = this.#totalBytes + BigInt(bytes.byteLength)
    if (nextTotal > SHA256_MAXIMUM_MESSAGE_BYTES) {
      throw new RangeError('direct ZIP SHA-256 message exceeds its format length')
    }
    this.#totalBytes = nextTotal
    let offset = 0
    if (this.#tailBytes > 0) {
      const copyBytes = Math.min(SHA256_BLOCK_BYTES - this.#tailBytes, bytes.byteLength)
      this.#tail.set(bytes.subarray(0, copyBytes), this.#tailBytes)
      this.#tailBytes += copyBytes
      offset = copyBytes
      if (this.#tailBytes === SHA256_BLOCK_BYTES) {
        compressSha256Block(this.#state, this.#tail, 0)
        this.#tailBytes = 0
      }
    }
    while (offset + SHA256_BLOCK_BYTES <= bytes.byteLength) {
      compressSha256Block(this.#state, bytes, offset)
      offset += SHA256_BLOCK_BYTES
    }
    if (offset < bytes.byteLength) {
      const remainder = bytes.subarray(offset)
      this.#tail.set(remainder, 0)
      this.#tailBytes = remainder.byteLength
    }
  }

  digest(): DirectZipCanonicalBytes {
    const state = Uint32Array.from(this.#state)
    const pendingBytes = this.#tailBytes + 1 + SHA256_LENGTH_BYTES
    const paddedBytes = Math.ceil(pendingBytes / SHA256_BLOCK_BYTES) * SHA256_BLOCK_BYTES
    const finalBlocks = new Uint8Array(paddedBytes)
    finalBlocks.set(this.#tail.subarray(0, this.#tailBytes))
    finalBlocks[this.#tailBytes] = 0x80
    new DataView(finalBlocks.buffer).setBigUint64(
      finalBlocks.byteLength - SHA256_LENGTH_BYTES,
      this.#totalBytes * 8n,
      false,
    )
    for (let offset = 0; offset < finalBlocks.byteLength; offset += SHA256_BLOCK_BYTES) {
      compressSha256Block(state, finalBlocks, offset)
    }
    const output = new Uint8Array(DIRECT_ZIP_SHA256_BYTES)
    const view = new DataView(output.buffer)
    state.forEach((value, index) => view.setUint32(index * 4, value, false))
    return output
  }

  get byteLength(): bigint {
    return this.#totalBytes
  }
}

export interface DirectZipEpochDigestInputV1 {
  readonly predecessorRoot: Uint8Array
  readonly start: bigint
  readonly end: bigint
  readonly contentDigest: Uint8Array
}

export function directZipEpochGenesisRoot(): DirectZipCanonicalBytes {
  return new Uint8Array(DIRECT_ZIP_SHA256_BYTES)
}

export function digestDirectZipArchiveBytes(bytes: Uint8Array): DirectZipCanonicalBytes {
  const accumulator = new DirectZipSha256Accumulator()
  accumulator.update(bytes)
  return accumulator.digest()
}

export function directZipEpochDigestCanonicalV1(
  input: DirectZipEpochDigestInputV1,
): DirectZipCanonicalBytes {
  if (input === null || typeof input !== 'object') {
    throw new TypeError('direct ZIP epoch digest input is invalid')
  }
  const predecessorRoot = snapshotDirectZipFixedBytes(
    input.predecessorRoot,
    DIRECT_ZIP_SHA256_BYTES,
    'direct ZIP epoch predecessor root',
    true,
  )
  const contentDigest = snapshotDirectZipFixedBytes(
    input.contentDigest,
    DIRECT_ZIP_SHA256_BYTES,
    'direct ZIP epoch content digest',
  )
  requireDirectZipFsaOffset(input.start, 'direct ZIP epoch start')
  requireDirectZipFsaOffset(input.end, 'direct ZIP epoch end')
  if (input.end < input.start) throw new TypeError('direct ZIP epoch range is inverted')
  return directZipCanonicalRecord(DIRECT_ZIP_EPOCH_DIGEST_DOMAIN, [
    directZipCanonicalFrame(predecessorRoot),
    directZipCanonicalFrame(directZipCanonicalU64(input.start)),
    directZipCanonicalFrame(directZipCanonicalU64(input.end)),
    directZipCanonicalFrame(contentDigest),
  ])
}

export function chainDirectZipEpochDigestV1(
  input: DirectZipEpochDigestInputV1,
): DirectZipCanonicalBytes {
  return digestDirectZipArchiveBytes(directZipEpochDigestCanonicalV1(input))
}

function compressSha256Block(state: Uint32Array, bytes: Uint8Array, offset: number): void {
  const words = new Uint32Array(64)
  const view = new DataView(bytes.buffer, bytes.byteOffset + offset, SHA256_BLOCK_BYTES)
  for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(index * 4, false)
  for (let index = 16; index < words.length; index += 1) {
    const x = words[index - 15]!
    const y = words[index - 2]!
    words[index] = (
      smallSigma1(y) + words[index - 7]! + smallSigma0(x) + words[index - 16]!
    ) >>> 0
  }
  let a = state[0]!
  let b = state[1]!
  let c = state[2]!
  let d = state[3]!
  let e = state[4]!
  let f = state[5]!
  let g = state[6]!
  let h = state[7]!
  for (let index = 0; index < words.length; index += 1) {
    const t1 = (
      h + bigSigma1(e) + ((e & f) ^ (~e & g)) +
      SHA256_CONSTANTS[index]! + words[index]!
    ) >>> 0
    const t2 = (bigSigma0(a) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0
    h = g
    g = f
    f = e
    e = (d + t1) >>> 0
    d = c
    c = b
    b = a
    a = (t1 + t2) >>> 0
  }
  state[0] = (state[0]! + a) >>> 0
  state[1] = (state[1]! + b) >>> 0
  state[2] = (state[2]! + c) >>> 0
  state[3] = (state[3]! + d) >>> 0
  state[4] = (state[4]! + e) >>> 0
  state[5] = (state[5]! + f) >>> 0
  state[6] = (state[6]! + g) >>> 0
  state[7] = (state[7]! + h) >>> 0
}

function rotateRight(value: number, count: number): number {
  return (value >>> count) | (value << (32 - count))
}

function smallSigma0(value: number): number {
  return rotateRight(value, 7) ^ rotateRight(value, 18) ^ (value >>> 3)
}

function smallSigma1(value: number): number {
  return rotateRight(value, 17) ^ rotateRight(value, 19) ^ (value >>> 10)
}

function bigSigma0(value: number): number {
  return rotateRight(value, 2) ^ rotateRight(value, 13) ^ rotateRight(value, 22)
}

function bigSigma1(value: number): number {
  return rotateRight(value, 6) ^ rotateRight(value, 11) ^ rotateRight(value, 25)
}

const SHA256_CONSTANTS = Uint32Array.from([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1,
  0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
  0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786,
  0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147,
  0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
  0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
  0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a,
  0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
  0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])
