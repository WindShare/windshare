import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE,
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  CHECKPOINT_LINEAGE_DOMAIN,
  canonicalCheckpointLineageBytes,
  classifyCheckpointLineage,
  decodeFileCheckpointV2,
  deriveCheckpointLineageID,
  encodeFileCheckpointV2,
  fileCheckpointDigest,
  fileCheckpointIsComplete,
  newFileCheckpointV2,
  sameCheckpointLineageSpec,
  validateFileCheckpointTransition,
  type CheckpointLineageEvidence,
  type CheckpointLineageSpec,
  type FileCheckpointSpec,
} from '../../src/output/persistence/checkpoint'
import { validateFileCheckpointPage } from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'

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

  it('rejects legacy FSA coordinates from the current durable namespace', () => {
    const current = checkpoint()
    const legacy = checkpoint({ materializerKind: FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE })
    const binding = durableCheckpointNamespaceIdentity(current)

    expect(() => validateFileCheckpointPage(
      { records: [legacy] },
      { direction: 'ascending' },
      binding,
    )).toThrow('escaped its operation namespace')
  })

  it('admits an empty root-relative coordinate only for the current FSA materializer', () => {
    const rootFile = checkpoint({ canonicalPath: [] })
    const encoded = encodeFileCheckpointV2(rootFile)

    expect(decodeFileCheckpointV2(encoded)).toEqual(rootFile)
    expect(() => deriveCheckpointLineageID(rootFile)).not.toThrow()
    expect(() => checkpoint({
      canonicalPath: [],
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE,
    })).toThrow('path')
    expect(() => deriveCheckpointLineageID({
      ...lineageSpec(),
      canonicalPath: [],
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_LEGACY_FSA_TREE,
    })).toThrow('path')
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

describe('CheckpointLineageID', () => {
  it('uses the frozen domain, u64be frames, and unambiguous path segments', () => {
    const spec = lineageSpec()
    const encoder = new TextEncoder()
    const path = concat([
      u64(2n),
      frame(encoder.encode('folder')),
      frame(encoder.encode('file.bin')),
    ])
    const expected = concat([
      encoder.encode(CHECKPOINT_LINEAGE_DOMAIN),
      frame(identityBytesForTest(16, 1)),
      frame(identityBytesForTest(32, 2)),
      frame(identityBytesForTest(32, 3)),
      frame(identityBytesForTest(16, 4)),
      frame(path),
      frame(Uint8Array.of(FILE_CHECKPOINT_MATERIALIZER_FSA_TREE)),
      frame(identityBytesForTest(32, 6)),
    ])

    expect(canonicalCheckpointLineageBytes(spec)).toEqual(expected)
    expect(deriveCheckpointLineageID({ ...spec, canonicalPath: ['a', 'bc'] }))
      .not.toBe(deriveCheckpointLineageID({ ...spec, canonicalPath: ['ab', 'c'] }))
  })

  it('separates every included coordinate and compares full specs after a digest lookup', () => {
    const spec = lineageSpec()
    const baseline = deriveCheckpointLineageID(spec)
    const changes: readonly CheckpointLineageSpec[] = [
      { ...spec, operationId: identity(16, 11) },
      { ...spec, receiveIntentDigest: identity(32, 12) },
      { ...spec, materializationBindingDigest: identity(32, 13) },
      { ...spec, fileId: identity(16, 14) },
      { ...spec, canonicalPath: ['folder', 'other.bin'] },
      { ...spec, materializerKind: 1 },
      { ...spec, authorityRef: identity(32, 16) },
    ]

    for (const changed of changes) {
      expect(deriveCheckpointLineageID(changed)).not.toBe(baseline)
      expect(sameCheckpointLineageSpec(spec, changed)).toBe(false)
    }
    expect(sameCheckpointLineageSpec(spec, { ...spec })).toBe(true)
  })

  it('excludes revision, size, object, ranges, generations, and lifecycle', () => {
    const baseline = checkpoint()
    const lineage = deriveCheckpointLineageID(baseline)
    const variants = [
      { changesRecordId: true, record: checkpoint({ fileRevision: identity(16, 15) }) },
      { changesRecordId: true, record: checkpoint({ exactSize: 13n }) },
      { changesRecordId: true, record: checkpoint({ ownedObjectId: identity(32, 17) }) },
      {
        changesRecordId: false,
        record: checkpoint({ verifiedRanges: [{ start: 0n, end: 1n }] }),
      },
      {
        changesRecordId: false,
        record: checkpoint({ stateGeneration: 2n, checkpointGeneration: 2n }),
      },
      { changesRecordId: false, record: checkpoint({ phase: FILE_CHECKPOINT_PHASE_PAUSED }) },
    ]

    for (const variant of variants) {
      expect(deriveCheckpointLineageID(variant.record)).toBe(lineage)
      expect(variant.record.recordId !== baseline.recordId).toBe(variant.changesRecordId)
    }
  })

  it('rejects zero identities, non-canonical paths, and unknown materializers', () => {
    const spec = lineageSpec()
    expect(() => deriveCheckpointLineageID({ ...spec, operationId: identity(16, 0) }))
      .toThrow('non-zero')
    expect(() => deriveCheckpointLineageID({ ...spec, canonicalPath: ['folder', '..'] }))
      .toThrow('path')
    expect(() => deriveCheckpointLineageID({ ...spec, materializerKind: 99 }))
      .toThrow('materializer')
    expect(sameCheckpointLineageSpec(
      { ...spec, operationId: identity(16, 0) },
      { ...spec, operationId: identity(16, 0) },
    )).toBe(false)
  })
})

describe('checkpoint lineage decision precedence', () => {
  const request = { fileRevision: identity(16, 21), exactSize: 64n }
  const exact: CheckpointLineageEvidence = {
    ...request,
    ownedObjectId: identity(32, 31),
  }
  const otherRevision: CheckpointLineageEvidence = {
    ...exact,
    fileRevision: identity(16, 22),
  }
  const invalidSize: CheckpointLineageEvidence = { ...exact, exactSize: 65n }
  const otherObject: CheckpointLineageEvidence = {
    ...exact,
    ownedObjectId: identity(32, 32),
  }
  const cases = [
    ['absent', request, [], false, 'absent'],
    ['exact', request, [exact], false, 'exact'],
    ['duplicate observation', request, [exact, exact], false, 'exact'],
    ['revision conflict', request, [otherRevision], false, 'revision-conflict'],
    ['invalid size', request, [invalidSize], false, 'invalid'],
    ['invalid precedes revision', request, [otherRevision, invalidSize], false, 'invalid'],
    ['ownership conflict', request, [exact, otherObject], false, 'ownership-conflict'],
    [
      'revision precedes ownership',
      request,
      [exact, otherObject, otherRevision],
      false,
      'revision-conflict',
    ],
    ['cross-lineage ownership', request, [exact], true, 'ownership-conflict'],
    ['cross-lineage flag cannot occupy absence', request, [], true, 'absent'],
    ['invalid request', { fileRevision: identity(16, 0), exactSize: 64n }, [exact], false, 'invalid'],
    [
      'invalid evidence',
      request,
      [{ ...exact, ownedObjectId: identity(32, 0) }],
      false,
      'invalid',
    ],
  ] as const

  for (const [name, target, evidence, crossConflict, expected] of cases) {
    it(name, () => {
      expect(classifyCheckpointLineage(target, evidence, crossConflict)).toBe(expected)
    })
  }
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
  return encodeBase64Url(identityBytesForTest(width, value))
}

function lineageSpec(): CheckpointLineageSpec {
  return {
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, 4),
    canonicalPath: ['folder', 'file.bin'],
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
  }
}

function identityBytesForTest(width: number, value: number): Uint8Array {
  return new Uint8Array(width).fill(value)
}

function u64(value: bigint): Uint8Array {
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value, false)
  return result
}

function frame(value: Uint8Array): Uint8Array {
  return concat([u64(BigInt(value.byteLength)), value])
}

function concat(parts: readonly Uint8Array[]): Uint8Array {
  const output = new Uint8Array(parts.reduce((size, part) => size + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}
