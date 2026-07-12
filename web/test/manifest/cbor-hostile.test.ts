import { encode, rfc8949EncodeOptions } from 'cborg'
import { describe, expect, it } from 'vitest'

import {
  MAX_CHUNK_BYTES,
  MAX_MTIME_MILLISECONDS,
  MAX_SEALED_MANIFEST_BYTES,
  MAX_STREAM_BYTES,
  MIN_MTIME_MILLISECONDS,
} from '../../src/contracts'
import { ManifestError, decodeCanonicalManifest } from '../../src/manifest'
import { encodedManifest, wireEntry } from './fixtures'

function hexBytes(hex: string): Uint8Array {
  const bytes = new Uint8Array(hex.length / 2)
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number.parseInt(hex.slice(index * 2, index * 2 + 2), 16)
  }
  return bytes
}

function withPrefix(prefix: number, body: Uint8Array): Uint8Array {
  const result = new Uint8Array(body.length + 1)
  result[0] = prefix
  result.set(body, 1)
  return result
}

function concatTestBytes(parts: readonly Uint8Array[]): Uint8Array {
  const length = parts.reduce((total, part) => total + part.byteLength, 0)
  const result = new Uint8Array(length)
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}

function capturedManifestError(bytes: Uint8Array): ManifestError {
  try {
    decodeCanonicalManifest(bytes)
  } catch (error) {
    expect(error).toBeInstanceOf(ManifestError)
    return error as ManifestError
  }
  throw new Error('expected manifest rejection')
}

function errorCode(bytes: Uint8Array): ManifestError['code'] {
  return capturedManifestError(bytes).code
}

describe('strict canonical manifest decoding', () => {
  it('rejects map order drift, duplicate keys, indefinite values, tags, and unknown fields', () => {
    const nonCanonicalOrder = hexBytes(
      'a3696368756e6b53697a6519040067656e747269657380617601',
    )
    const duplicateVersion = hexBytes(
      'a461760161760167656e747269657380696368756e6b53697a65190400',
    )
    const indefiniteEntries = hexBytes(
      'a361760167656e74726965739fff696368756e6b53697a65190400',
    )
    const tagged = withPrefix(0xc0, encodedManifest([]))
    const unknownField = encode(
      new Map<string, unknown>([
        ['v', 1],
        ['extra', 0],
        ['entries', []],
        ['chunkSize', 1_024],
      ]),
      rfc8949EncodeOptions,
    )

    expect(errorCode(nonCanonicalOrder)).toBe('non-canonical')
    expect(errorCode(duplicateVersion)).toBe('non-canonical')
    expect(errorCode(indefiniteEntries)).toBe('non-canonical')
    expect(errorCode(tagged)).toBe('non-canonical')
    expect(errorCode(unknownField)).toBe('schema-mismatch')
  })

  it('does not retain third-party decoder diagnostics from hostile CBOR', () => {
    const duplicateVersion = hexBytes(
      'a461760161760167656e747269657380696368756e6b53697a65190400',
    )
    const error = capturedManifestError(duplicateVersion)

    expect(error.code).toBe('non-canonical')
    expect(error.cause).toBeUndefined()
  })

  it('probes version before applying the known schema', () => {
    const future = encode(
      new Map<string, unknown>([
        ['v', 2],
        ['futureField', { shape: 'unknown' }],
      ]),
      rfc8949EncodeOptions,
    )

    expect(errorCode(future)).toBe('unsupported-version')
  })

  it('rejects unsafe integers and geometry before narrowing to JavaScript numbers', () => {
    expect(
      errorCode(encodedManifest([wireEntry('file', MAX_STREAM_BYTES + 1)])),
    ).toBe('stream-too-large')
    expect(
      errorCode(
        encodedManifest([
          wireEntry('file', 0, BigInt(MAX_MTIME_MILLISECONDS) + 1n),
        ]),
      ),
    ).toBe('mtime-out-of-range')
    expect(errorCode(encodedManifest([wireEntry('file', -1)]))).toBe('negative-size')
    expect(errorCode(encodedManifest([], 1n << 60n))).toBe('chunk-size-too-large')
    expect(errorCode(encodedManifest([wireEntry('file', 1.5)]))).toBe('schema-mismatch')
  })

  it('accepts the exact interoperable integer and maximum-stream boundaries', () => {
    const manifest = decodeCanonicalManifest(
      encodedManifest(
        [
          wireEntry('minimum-mtime', MAX_STREAM_BYTES, MIN_MTIME_MILLISECONDS),
          wireEntry('maximum-mtime', 0, MAX_MTIME_MILLISECONDS),
        ],
        MAX_CHUNK_BYTES,
      ),
    )

    expect(manifest.entries).toHaveLength(2)
    expect(manifest.entries[0]).toMatchObject({
      size: MAX_STREAM_BYTES,
      mtime: MIN_MTIME_MILLISECONDS,
    })
    expect(manifest.entries[1]).toMatchObject({ mtime: MAX_MTIME_MILLISECONDS })
  })

  it('matches Go by excluding a signed-64 directory size from stream geometry', () => {
    const manifest = decodeCanonicalManifest(
      encodedManifest([
        wireEntry('directory', 1n << 60n, 0, true),
        wireEntry('directory/file', 1),
      ]),
    )

    expect(manifest.entries).toEqual([
      { kind: 'directory', path: 'directory', mtime: 0 },
      { kind: 'file', path: 'directory/file', size: 1, mtime: 0 },
    ])
  })

  it.each([Number.NaN, Infinity, -Infinity])(
    'rejects non-finite CBOR numeric values: %s',
    (value) => {
      expect(errorCode(encodedManifest([wireEntry('file', value)]))).toBe(
        'non-canonical',
      )
    },
  )

  it('rejects CBOR undefined instead of coercing it into schema data', () => {
    const entry = wireEntry('file')
    entry.set('size', undefined)

    expect(errorCode(encodedManifest([entry]))).toBe('non-canonical')
  })

  it('rejects duplicate, folded-alias, implicit-prefix, and file-ancestor trees', () => {
    expect(
      errorCode(encodedManifest([wireEntry('same'), wireEntry('same')])),
    ).toBe('duplicate-path')
    expect(
      errorCode(encodedManifest([wireEntry('Straße'), wireEntry('strasse')])),
    ).toBe('path-collision')
    expect(
      errorCode(encodedManifest([wireEntry('Dir/a'), wireEntry('dir/b')])),
    ).toBe('path-collision')
    expect(
      errorCode(encodedManifest([wireEntry('file', 1), wireEntry('file/child', 1)])),
    ).toBe('path-type-conflict')
    expect(
      errorCode(encodedManifest([wireEntry('file/child', 1), wireEntry('file', 1)])),
    ).toBe('path-type-conflict')
  })

  it('rejects hostile declared collection sizes before decoder allocation', () => {
    const overBoundEntries = hexBytes(
      'a361760167656e74726965739a00100001696368756e6b53697a65190400',
    )

    expect(errorCode(overBoundEntries)).toBe('invalid-cbor')
  })

  it('bounds actual indefinite entries and nesting before recursive allocation', () => {
    const elementLimit = 1 << 20
    const elements = new Uint8Array(elementLimit + 1).fill(0xf6)
    const indefiniteEntries = concatTestBytes([
      hexBytes('a361760167656e74726965739f'),
      elements,
      hexBytes('ff696368756e6b53697a65190400'),
    ])
    let deeplyNested = encodedManifest([])
    for (let depth = 0; depth <= 32; depth += 1) {
      deeplyNested = withPrefix(0x81, deeplyNested)
    }

    expect(errorCode(indefiniteEntries)).toBe('invalid-cbor')
    expect(errorCode(deeplyNested)).toBe('invalid-cbor')
  })

  it('rejects an oversized raw decoder input before CBOR traversal', () => {
    expect(errorCode(new Uint8Array(MAX_SEALED_MANIFEST_BYTES + 1))).toBe(
      'manifest-too-large',
    )
  })

  it('fails closed across deterministic malformed-CBOR fuzz input', () => {
    let state = 0x5eed_c1
    for (let sample = 0; sample < 256; sample += 1) {
      const length = sample % 97
      const bytes = new Uint8Array(length)
      for (let index = 0; index < length; index += 1) {
        state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
        bytes[index] = state & 0xff
      }
      expect(() => decodeCanonicalManifest(bytes)).toThrow(ManifestError)
    }
  })
})
