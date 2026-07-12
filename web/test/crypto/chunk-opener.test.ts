import { describe, expect, it, vi } from 'vitest'

import { CIPHER_SUITE_V1, MAX_CHUNK_COUNT } from '../../src/contracts'
import {
  createChunkOpenerFromStreamKey,
  maximumSealedBlockSize,
} from '../../src/crypto'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface ChunkVector {
  readonly name: string
  readonly streamKeyB64: string
  readonly chunkSize: number
  readonly index: string
  readonly plaintextB64: string
  readonly blockCTB64: string
}

const vectors = loadVectorFile(
  new URL('../../../testvectors/chunk-seal.json', import.meta.url),
).cases as unknown as readonly ChunkVector[]

describe('AES-GCM chunk opening', () => {
  it.each(vectors)('opens the Go sealed-block vector: $name', async (vector) => {
    const opener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      b64ToBytes(vector.streamKeyB64),
      vector.chunkSize,
    )

    await expect(opener.open(Number(vector.index), b64ToBytes(vector.blockCTB64))).resolves.toEqual(
      b64ToBytes(vector.plaintextB64),
    )
  })

  it('binds ciphertext to its global index, key, and authenticated bytes', async () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('chunk vector is missing')
    }
    const sealed = b64ToBytes(vector.blockCTB64)
    const streamKey = b64ToBytes(vector.streamKeyB64)
    const opener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      streamKey,
      vector.chunkSize,
    )
    const tampered = sealed.slice()
    tampered[tampered.length - 1] = (tampered.at(-1) ?? 0) ^ 1

    await expect(opener.open(1, sealed)).rejects.toMatchObject({
      code: 'authentication-failed',
    })
    await expect(opener.open(0, tampered)).rejects.toMatchObject({
      code: 'authentication-failed',
    })
    streamKey[0] = (streamKey[0] ?? 0) ^ 1
    const wrongKeyOpener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      streamKey,
      vector.chunkSize,
    )
    await expect(wrongKeyOpener.open(0, sealed)).rejects.toMatchObject({
      code: 'authentication-failed',
    })
  })

  it('snapshots an accepted ciphertext before deriving a segment key', async () => {
    const vector = vectors[3]
    if (vector === undefined) {
      throw new Error('cross-segment vector is missing')
    }
    const sealed = b64ToBytes(vector.blockCTB64)
    const opener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      b64ToBytes(vector.streamKeyB64),
      vector.chunkSize,
    )
    const pending = opener.open(Number(vector.index), sealed)
    sealed.fill(0)

    await expect(pending).resolves.toEqual(b64ToBytes(vector.plaintextB64))
  })

  it('rejects allocation and index boundaries before authentication work', async () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('chunk vector is missing')
    }
    const opener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      b64ToBytes(vector.streamKeyB64),
      vector.chunkSize,
    )
    await expect(opener.open(MAX_CHUNK_COUNT, new Uint8Array(28))).rejects.toMatchObject({
      code: 'block-index-out-of-range',
    })
    await expect(opener.open(0, new Uint8Array(27))).rejects.toMatchObject({
      code: 'block-too-short',
    })
    await expect(
      opener.open(0, new Uint8Array(maximumSealedBlockSize(1, vector.chunkSize) + 1)),
    ).rejects.toMatchObject({ code: 'block-too-long' })
    expect(() => maximumSealedBlockSize(2, vector.chunkSize)).toThrowError(
      expect.objectContaining({ code: 'unsupported-suite' }),
    )
  })

  it.each([-1, 0.5, Number.NaN, Infinity, Number.MAX_SAFE_INTEGER])(
    'rejects an invalid global index before deriving a segment key: %s',
    async (index) => {
      const vector = vectors[0]
      if (vector === undefined) {
        throw new Error('chunk vector is missing')
      }
      const opener = createChunkOpenerFromStreamKey(
        CIPHER_SUITE_V1,
        b64ToBytes(vector.streamKeyB64),
        vector.chunkSize,
      )
      const importKey = vi.spyOn(globalThis.crypto.subtle, 'importKey')
      try {
        await expect(opener.open(index, new Uint8Array(28))).rejects.toMatchObject({
          code: 'block-index-out-of-range',
        })
        expect(importKey).not.toHaveBeenCalled()
      } finally {
        importKey.mockRestore()
      }
    },
  )

  it('snapshots the stream key at construction', async () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('chunk vector is missing')
    }
    const streamKey = b64ToBytes(vector.streamKeyB64)
    const opener = createChunkOpenerFromStreamKey(
      CIPHER_SUITE_V1,
      streamKey,
      vector.chunkSize,
    )
    streamKey.fill(0)

    await expect(opener.open(0, b64ToBytes(vector.blockCTB64))).resolves.toEqual(
      b64ToBytes(vector.plaintextB64),
    )
  })
})
