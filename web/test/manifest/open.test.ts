import { describe, expect, it, vi } from 'vitest'

import {
  CIPHER_SUITE_V1,
  MANIFEST_FINGERPRINT_BYTES,
  MAX_SEALED_MANIFEST_BYTES,
} from '../../src/contracts'
import { deriveManifestKey } from '../../src/crypto'
import {
  decodeCanonicalManifest,
  openSealedManifest,
} from '../../src/manifest'
import { b64ToBytes } from '../vectors'
import { manifestSealVector } from './fixtures'

describe('sealed manifest opening', () => {
  it('matches the Go key, nonce, canonical CBOR, schema, and fingerprint vector', async () => {
    const readSecret = b64ToBytes(manifestSealVector.readSecretB64)
    const sealed = b64ToBytes(manifestSealVector.sealedManifestB64)

    await expect(deriveManifestKey(readSecret)).resolves.toEqual(
      b64ToBytes(manifestSealVector.manifestKeyB64),
    )
    expect(sealed.subarray(0, 12)).toEqual(b64ToBytes(manifestSealVector.nonceB64))
    const decodedPlaintext = decodeCanonicalManifest(
      b64ToBytes(manifestSealVector.canonicalCborB64),
    )
    const opened = await openSealedManifest(CIPHER_SUITE_V1, readSecret, sealed)

    expect(opened.manifest).toEqual(decodedPlaintext)
    expect({
      v: opened.manifest.version,
      chunkSize: opened.manifest.chunkSize,
      entries: opened.manifest.entries.map((entry) => ({
        path: entry.path,
        size: entry.kind === 'file' ? entry.size : 0,
        mtime: entry.mtime,
        isDir: entry.kind === 'directory',
      })),
    }).toEqual(manifestSealVector.manifest)
    expect(opened.fingerprint).toEqual(sealed.slice(-MANIFEST_FINGERPRINT_BYTES))
  })

  it('rejects tampering, wrong keys, unknown suites, and structural size violations', async () => {
    const readSecret = b64ToBytes(manifestSealVector.readSecretB64)
    const sealed = b64ToBytes(manifestSealVector.sealedManifestB64)
    const tampered = sealed.slice()
    tampered[tampered.length - 1] = (tampered.at(-1) ?? 0) ^ 1

    await expect(openSealedManifest(1, readSecret, tampered)).rejects.toMatchObject({
      code: 'manifest-authentication-failed',
    })
    await expect(
      openSealedManifest(1, new Uint8Array(16).fill(0xff), sealed),
    ).rejects.toMatchObject({
      code: 'manifest-authentication-failed',
    })
    await expect(openSealedManifest(2, readSecret, sealed)).rejects.toMatchObject({
      code: 'unsupported-suite',
    })
    await expect(openSealedManifest(1, readSecret, new Uint8Array(27))).rejects.toMatchObject({
      code: 'sealed-manifest-too-short',
    })
  })

  it('rejects an oversized envelope before deriving or importing a key', async () => {
    const importKey = vi.spyOn(globalThis.crypto.subtle, 'importKey')
    try {
      await expect(
        openSealedManifest(
          1,
          b64ToBytes(manifestSealVector.readSecretB64),
          new Uint8Array(MAX_SEALED_MANIFEST_BYTES + 1),
        ),
      ).rejects.toMatchObject({ code: 'manifest-too-large' })
      expect(importKey).not.toHaveBeenCalled()
    } finally {
      importKey.mockRestore()
    }
  })

  it('snapshots sealed bytes before asynchronous key derivation', async () => {
    const sealed = b64ToBytes(manifestSealVector.sealedManifestB64)
    const expectedFingerprint = sealed.slice(-MANIFEST_FINGERPRINT_BYTES)
    const pending = openSealedManifest(
      CIPHER_SUITE_V1,
      b64ToBytes(manifestSealVector.readSecretB64),
      sealed,
    )
    sealed.fill(0)

    const opened = await pending
    expect(opened).toMatchObject({ manifest: { version: 1, chunkSize: 1_024 } })
    expect(opened.fingerprint).toEqual(expectedFingerprint)
    expect(Object.isFrozen(opened)).toBe(true)
    expect(Object.isFrozen(opened.manifest)).toBe(true)
    expect(Object.isFrozen(opened.manifest.entries)).toBe(true)
    expect(opened.manifest.entries.every(Object.isFrozen)).toBe(true)
  })
})
