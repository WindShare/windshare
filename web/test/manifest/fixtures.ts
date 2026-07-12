import { encode, rfc8949EncodeOptions } from 'cborg'

import { decodeCanonicalManifest } from '../../src/manifest'
import { b64ToBytes, loadVectorFile } from '../vectors'

export interface ManifestVectorEntry {
  readonly path: string
  readonly size: number
  readonly mtime: number
  readonly isDir: boolean
}

export interface ManifestSealVector {
  readonly name: string
  readonly readSecretB64: string
  readonly manifestKeyB64: string
  readonly nonceB64: string
  readonly manifest: {
    readonly v: number
    readonly chunkSize: number
    readonly entries: readonly ManifestVectorEntry[]
  }
  readonly canonicalCborB64: string
  readonly sealedManifestB64: string
}

export const manifestSealVector = loadVectorFile(
  new URL('../../../testvectors/manifest-seal.json', import.meta.url),
).cases[0] as unknown as ManifestSealVector

export function wireEntry(
  path: string,
  size: number | bigint = 0,
  mtime: number | bigint = 0,
  isDirectory = false,
): Map<string, unknown> {
  return new Map<string, unknown>([
    ['path', path],
    ['size', size],
    ['mtime', mtime],
    ['isDir', isDirectory],
  ])
}

export function encodedManifest(
  entries: readonly Map<string, unknown>[],
  chunkSize: number | bigint = 1_024,
  version: number | bigint = 1,
): Uint8Array {
  return encode(
    new Map<string, unknown>([
      ['v', version],
      ['entries', entries],
      ['chunkSize', chunkSize],
    ]),
    rfc8949EncodeOptions,
  )
}

export function vectorManifest() {
  return decodeCanonicalManifest(b64ToBytes(manifestSealVector.canonicalCborB64))
}
