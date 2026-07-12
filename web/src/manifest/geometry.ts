import {
  MAX_CHUNK_BYTES,
  MAX_CHUNK_COUNT,
  MAX_STREAM_BYTES,
  MIN_CHUNK_BYTES,
  type ByteLength,
  type ChunkCount,
  type ChunkSize,
  type ManifestEntry,
} from '../contracts'
import { ManifestError } from './errors'

export interface PackedGeometry {
  readonly chunkSize: ChunkSize
  readonly streamBytes: ByteLength
  readonly chunkCount: ChunkCount
}

function requireSafeInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value)) {
    throw new ManifestError('schema-mismatch', `${label} must be a safe integer`)
  }
}

export function validateGeometry(chunkSize: number, streamBytes: number): PackedGeometry {
  requireSafeInteger(chunkSize, 'chunk size')
  if (chunkSize <= 0 || !Number.isInteger(Math.log2(chunkSize))) {
    throw new ManifestError(
      'chunk-size-not-power-of-two',
      'Chunk size must be a positive power of two',
    )
  }
  if (chunkSize < MIN_CHUNK_BYTES) {
    throw new ManifestError(
      'chunk-size-too-small',
      `Chunk size must be at least ${MIN_CHUNK_BYTES} bytes`,
    )
  }
  if (chunkSize > MAX_CHUNK_BYTES) {
    throw new ManifestError(
      'chunk-size-too-large',
      `Chunk size must not exceed ${MAX_CHUNK_BYTES} bytes`,
    )
  }
  requireSafeInteger(streamBytes, 'stream length')
  if (streamBytes < 0) {
    throw new ManifestError('negative-stream', 'Packed stream length must not be negative')
  }
  if (streamBytes > MAX_STREAM_BYTES) {
    throw new ManifestError(
      'stream-too-large',
      `Packed stream exceeds the ${MAX_STREAM_BYTES}-byte protocol ceiling`,
    )
  }
  const chunkCount =
    Math.floor(streamBytes / chunkSize) + (streamBytes % chunkSize === 0 ? 0 : 1)
  if (chunkCount > MAX_CHUNK_COUNT) {
    throw new ManifestError(
      'too-many-chunks',
      `Packed stream exceeds the ${MAX_CHUNK_COUNT}-chunk protocol ceiling`,
    )
  }
  return Object.freeze({
    chunkSize: chunkSize as ChunkSize,
    streamBytes: streamBytes as ByteLength,
    chunkCount: chunkCount as ChunkCount,
  })
}

export function deriveGeometry(
  entries: readonly ManifestEntry[],
  chunkSize: number,
): PackedGeometry {
  validateGeometry(chunkSize, 0)
  const seen = new Set<string>()
  let streamBytes = 0
  for (const entry of entries) {
    if (seen.has(entry.path)) {
      throw new ManifestError('duplicate-path', 'Manifest contains a duplicate path')
    }
    seen.add(entry.path)
    if (entry.kind === 'directory' || entry.size === 0) {
      continue
    }
    if (entry.size > MAX_STREAM_BYTES - streamBytes) {
      throw new ManifestError(
        'stream-too-large',
        `Packed stream exceeds the ${MAX_STREAM_BYTES}-byte protocol ceiling`,
      )
    }
    streamBytes += entry.size
  }
  return validateGeometry(chunkSize, streamBytes)
}
