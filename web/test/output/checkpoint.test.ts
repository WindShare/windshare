import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  decodeFileCheckpointV2,
  encodeFileCheckpointV2,
  fileCheckpointDigest,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  validateFileCheckpointTransition,
  type FileCheckpointSpec,
} from '../../src/output/persistence/checkpoint'

describe('FileCheckpointV2', () => {
  it('round-trips the frozen v2 envelope and keeps range authority file-local', () => {
    const record = checkpoint({
      checkpointGeneration: 7n,
      verifiedRanges: [{ start: 0n, end: 12n }],
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })

    const encoded = encodeFileCheckpointV2(record)
    expect(new TextDecoder().decode(encoded.subarray(0, 8))).toBe('WSFCPV2\0')
    expect(decodeFileCheckpointV2(encoded)).toEqual(record)
    expect(record.ownershipMarker).toBe(FILE_CHECKPOINT_OWNERSHIP_MARKER)
    expect(record.namespace).toBe(FILE_CHECKPOINT_NAMESPACE)
    expect(fileCheckpointIsComplete(record)).toBe(true)
    expect(fileCheckpointDigest(record)).toBe(record.checksum)
  })

  it('derives one immutable record identity across candidate promotion', () => {
    const candidate = checkpoint({
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 12n }],
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    const verified = checkpoint({
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 12n }],
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })

    expect(verified.recordId).toBe(candidate.recordId)
    expect(verified.checksum).not.toBe(candidate.checksum)
    expect(() => validateFileCheckpointTransition(candidate, verified)).not.toThrow()
  })

  it('rejects tampering, v1 ownership, overlapping ranges, and generation-free range changes', () => {
    const encoded = encodeFileCheckpointV2(checkpoint())
    const lastIndex = encoded.length - 1
    const lastByte = encoded.at(lastIndex)
    if (lastByte === undefined) throw new Error('checkpoint envelope must not be empty')
    encoded[lastIndex] = lastByte ^ 0x01
    expect(() => decodeFileCheckpointV2(encoded)).toThrow('checksum')

    expect(() => checkpoint({ ownershipMarker: 'windshare/file-checkpoint/v1' }))
      .toThrow('ownership')
    expect(() => checkpoint({
      verifiedRanges: [
        { start: 0n, end: 8n },
        { start: 7n, end: 10n },
      ],
    })).toThrow('range')

    const previous = checkpoint({ checkpointGeneration: 4n, verifiedRanges: [] })
    const changed = checkpoint({
      checkpointGeneration: 4n,
      verifiedRanges: [{ start: 0n, end: 1n }],
    })
    expect(() => validateFileCheckpointTransition(previous, changed))
      .toThrow('without a generation')
  })

  it('does not claim complete aggregate evidence from candidate or partial coverage', () => {
    expect(fileCheckpointIsComplete(checkpoint({
      verifiedRanges: [{ start: 0n, end: 12n }],
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    }))).toBe(false)
    expect(fileCheckpointIsComplete(checkpoint({
      verifiedRanges: [{ start: 0n, end: 11n }],
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    }))).toBe(false)
  })
})

function checkpoint(overrides: Partial<FileCheckpointSpec> = {}) {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, 4),
    fileRevision: identity(16, 5),
    canonicalPath: ['folder', 'file.bin'],
    exactSize: 12n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, 7),
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    ...overrides,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
