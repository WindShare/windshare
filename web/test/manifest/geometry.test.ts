import { describe, expect, it } from 'vitest'

import {
  MAX_CHUNK_BYTES,
  MAX_CHUNK_COUNT,
  MAX_STREAM_BYTES,
  MIN_CHUNK_BYTES,
} from '../../src/contracts'
import {
  SEGMENT_BYTES,
} from '../../src/crypto'
import {
  ManifestError,
  validateGeometry,
} from '../../src/manifest'
import { loadVectorFile } from '../vectors'

interface GeometryVector {
  readonly name: string
  readonly chunkSize?: string
  readonly streamBytes?: string
  readonly expected?: string
  readonly chunkCount?: string
  readonly constants?: Readonly<Record<string, string>>
}

const vectors = loadVectorFile(
  new URL('../../../testvectors/geometry.json', import.meta.url),
).cases as unknown as readonly GeometryVector[]

const expectedCodes = new Map<string, ManifestError['code']>([
  ['chunk-size-too-small', 'chunk-size-too-small'],
  ['chunk-size-too-large', 'chunk-size-too-large'],
  ['chunk-size-not-power-of-two', 'chunk-size-not-power-of-two'],
  ['negative-stream', 'negative-stream'],
  ['too-many-chunks', 'too-many-chunks'],
  ['stream-too-large', 'stream-too-large'],
])

describe('packed-stream geometry', () => {
  it('matches the normative protocol constants', () => {
    const constants = vectors.find((vector) => vector.name === 'protocol-constants')?.constants

    expect(String(MIN_CHUNK_BYTES)).toBe(constants?.minChunkSize)
    expect(String(MAX_CHUNK_BYTES)).toBe(constants?.maxChunkSize)
    expect(String(MAX_CHUNK_COUNT)).toBe(constants?.maxChunkCount)
    expect(String(MAX_CHUNK_COUNT / 8)).toBe(constants?.maxChunkStateBytes)
    expect(String(MAX_STREAM_BYTES)).toBe(constants?.maxStreamBytes)
    expect(String(SEGMENT_BYTES)).toBe(constants?.segmentBytes)
  })

  it.each(vectors.filter((vector) => vector.expected === 'valid'))(
    'accepts the Go geometry boundary: $name',
    (vector) => {
      const geometry = validateGeometry(
        Number(vector.chunkSize),
        Number(vector.streamBytes),
      )

      expect(String(geometry.chunkCount)).toBe(vector.chunkCount)
    },
  )

  it.each(vectors.filter((vector) => expectedCodes.has(vector.expected ?? '')))(
    'rejects the Go geometry boundary: $name',
    (vector) => {
      try {
        validateGeometry(Number(vector.chunkSize), Number(vector.streamBytes))
      } catch (error) {
        expect(error).toBeInstanceOf(ManifestError)
        expect((error as ManifestError).code).toBe(expectedCodes.get(vector.expected ?? ''))
        return
      }
      throw new Error('expected invalid geometry')
    },
  )

  it.each([Number.NaN, Infinity, 1.5, Number.MAX_SAFE_INTEGER + 1])(
    'rejects unsafe numeric input before deriving chunk state: %s',
    (value) => {
      expect(() => validateGeometry(1_024, value)).toThrowError(
        expect.objectContaining<Partial<ManifestError>>({ code: 'schema-mismatch' }),
      )
    },
  )

  it('reports invalid chunk geometry before an independently invalid stream', () => {
    expect(() => validateGeometry(1_536, Number.NaN)).toThrowError(
      expect.objectContaining<Partial<ManifestError>>({
        code: 'chunk-size-not-power-of-two',
      }),
    )
  })
})
