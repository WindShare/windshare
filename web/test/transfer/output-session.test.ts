import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import { V2_CATALOG_NAME_BYTES } from '../../src/catalog/path-policy'
import {
  VerifiedDurableRanges,
  MAXIMUM_OUTPUT_PATH_SEGMENTS,
  MAXIMUM_OUTPUT_SEGMENT_BYTES,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
} from '../../src/transfer/output-session'

const source = Object.freeze({
  shareInstance: 'share',
  fileId: 'file',
  fileRevision: 'revision',
})

const ownership = Object.freeze({
  backend: 'fake',
  outputSessionId: 'session',
  canonicalPath: Object.freeze(['folder', 'file']),
  ownedFileIdentity: 'owned-file',
})

describe('OutputSession value contracts', () => {
  it('binds normalized durable ranges to one source revision and exact file size', () => {
    const durable = new VerifiedDurableRanges(ownership, source, 100n, [
      byteRange(20n, 30n),
      byteRange(0n, 10n),
      byteRange(10n, 20n),
    ])
    expect(durable.ownership).toEqual(ownership)
    expect(Object.isFrozen(durable.ownership.canonicalPath)).toBe(true)
    expect(durable.source).toEqual(source)
    expect(durable.ranges).toEqual([{ start: 0n, end: 30n }])
    expect(durable.covers(byteRange(5n, 25n))).toBe(true)
    expect(durable.covers(byteRange(5n, 31n))).toBe(false)
    expect(() => new VerifiedDurableRanges(
      ownership,
      source,
      10n,
      [byteRange(0n, 11n)],
    ))
      .toThrow(/exceeds/u)
  })

  it('snapshots capabilities, source identity, and output paths', () => {
    const capabilities = outputCapabilities({
      durability: 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime: false,
    })
    expect(Object.isFrozen(capabilities)).toBe(true)
    expect(outputSessionIdentity({ backend: 'fsa', outputSessionId: 'job' })).toEqual({
      backend: 'fsa',
      outputSessionId: 'job',
    })

    const path = ['folder', 'file.bin']
    const file = snapshotOutputFile({
      source: {
        shareInstance: 'share',
        fileId: 'file',
        fileRevision: 'revision',
      },
      path,
      exactSize: 3n,
    })
    path[0] = 'changed'
    expect(file.path).toEqual(['folder', 'file.bin'])
    expect(Object.isFrozen(file.path)).toBe(true)
    expect(Object.isFrozen(file.source)).toBe(true)
  })

  it('rejects structurally unsafe or semantically incomplete output identities', () => {
    expect(MAXIMUM_OUTPUT_SEGMENT_BYTES).toBe(V2_CATALOG_NAME_BYTES)
    expect(() => snapshotOutputDirectory({
      path: ['a'.repeat(V2_CATALOG_NAME_BYTES)],
    })).not.toThrow()
    expect(() => outputSessionIdentity({ backend: '', outputSessionId: 'job' }))
      .toThrow(/backend/u)
    expect(() => snapshotOutputDirectory({ path: [] })).toThrow(/segment/u)
    expect(() => snapshotOutputDirectory({ path: ['..'] })).toThrow(/unsafe/u)
    expect(() => snapshotOutputDirectory({ path: ['bad/name'] })).toThrow(/unsafe/u)
    expect(() => snapshotOutputDirectory({
      path: ['a'.repeat(MAXIMUM_OUTPUT_SEGMENT_BYTES + 1)],
    })).toThrow(/byte limit/u)
    expect(() => snapshotOutputDirectory({
      path: Array.from({ length: MAXIMUM_OUTPUT_PATH_SEGMENTS + 1 }, () => 'a'),
    })).toThrow(/segment limit/u)
    expect(() => snapshotOutputDirectory({
      path: Array.from({ length: 129 }, () => 'a'.repeat(MAXIMUM_OUTPUT_SEGMENT_BYTES)),
    })).toThrow(/path exceeds its byte limit/u)
    expect(() => snapshotOutputDirectory({ path: ['\ud800'] })).toThrow(/unsafe/u)
    expect(() => snapshotOutputFile({
      source: { shareInstance: '', fileId: 'file', fileRevision: 'revision' },
      path: ['file'],
      exactSize: 0n,
    })).toThrow(/shareInstance/u)
    expect(() => snapshotOutputFile({
      source: { shareInstance: 'share', fileId: 'file', fileRevision: 'revision' },
      path: ['file'],
      exactSize: -1n,
    })).toThrow(/negative/u)
    expect(() => new VerifiedDurableRanges(
      { ...ownership, canonicalPath: ['different'], ownedFileIdentity: '' },
      source,
      0n,
      [],
    )).toThrow(/owned output file/u)
  })
})
