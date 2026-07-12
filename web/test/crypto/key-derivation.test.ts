import { describe, expect, it } from 'vitest'

import {
  deriveManifestKey,
  deriveSegmentKey,
  deriveStreamKey,
} from '../../src/crypto'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface SegmentVector {
  readonly seg: number
  readonly keyB64: string
}

interface KeyDerivationVector {
  readonly name: string
  readonly readSecretB64: string
  readonly manifestKeyB64: string
  readonly streamKeyB64: string
  readonly segKeys: readonly SegmentVector[]
}

const vectorFile = loadVectorFile(
  new URL('../../../testvectors/keyderiv.json', import.meta.url),
)
const vectors = vectorFile.cases as unknown as readonly KeyDerivationVector[]

describe('HKDF-SHA256 key hierarchy', () => {
  it.each(vectors)('matches every Go key derivation vector: $name', async (vector) => {
    const readSecret = b64ToBytes(vector.readSecretB64)
    const manifestKey = await deriveManifestKey(readSecret)
    const streamKey = await deriveStreamKey(readSecret)

    expect(manifestKey).toEqual(b64ToBytes(vector.manifestKeyB64))
    expect(streamKey).toEqual(b64ToBytes(vector.streamKeyB64))
    for (const segment of vector.segKeys) {
      await expect(deriveSegmentKey(streamKey, segment.seg)).resolves.toEqual(
        b64ToBytes(segment.keyB64),
      )
    }
  })

  it('snapshots mutable key input before the first asynchronous boundary', async () => {
    const vector = vectors[0]
    if (vector === undefined) {
      throw new Error('key derivation vector is missing')
    }
    const readSecret = b64ToBytes(vector.readSecretB64)
    const pending = deriveManifestKey(readSecret)
    readSecret.fill(0xff)

    await expect(pending).resolves.toEqual(b64ToBytes(vector.manifestKeyB64))
  })

  it('rejects invalid key lengths and segment numbers before WebCrypto work', async () => {
    await expect(deriveManifestKey(new Uint8Array(15))).rejects.toMatchObject({
      code: 'invalid-key-material',
    })
    await expect(deriveStreamKey(new Uint8Array(17))).rejects.toMatchObject({
      code: 'invalid-key-material',
    })
    await expect(deriveSegmentKey(new Uint8Array(32), -1)).rejects.toMatchObject({
      code: 'invalid-key-material',
    })
    await expect(
      deriveSegmentKey(new Uint8Array(32), 0x1_0000_0000),
    ).rejects.toMatchObject({ code: 'invalid-key-material' })
  })
})
