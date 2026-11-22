import { describe, expect, it } from 'vitest'
import { planArtifactOffers, PORTABLE_MAX_BYTES, SMALL_FILE_PORTABLE_THRESHOLD_BYTES } from '../../src/output/backends/contract'
import type { SelectionShape } from '../../src/transfer/discovery'

const FILE_ENTRY = Object.freeze({
  kind: 'file' as const,
  id: new Uint8Array(16).fill(1),
  idText: 'file-1',
  name: 'video.mp4',
  expectedSize: 1024n,
})

function singleShape(bytes: bigint): SelectionShape {
  return Object.freeze({
    kind: 'single-file',
    file: Object.freeze({ ...FILE_ENTRY, expectedSize: bytes }),
    sourcePath: Object.freeze([FILE_ENTRY.name]),
    exactBytes: bytes,
  })
}

describe('artifact offer planning', () => {
  it('prefers OPFS for single files when available and falls back to portable without OPFS', () => {
    const fullCaps = {
      indexedDb: true,
      fsa: true,
      opfs: true,
      portable: true,
      availableStorageBytes: 10n * 1024n * 1024n * 1024n,
    }
    const withOpfs = planArtifactOffers({
      epoch: 1n,
      selectionKey: 'k1',
      shape: singleShape(SMALL_FILE_PORTABLE_THRESHOLD_BYTES),
      capabilities: fullCaps,
    })
    expect(withOpfs.kind).toBe('ready')
    if (withOpfs.kind === 'ready') {
      expect(withOpfs.primary.backend).toBe('opfs')
      expect(withOpfs.alternatives.some(choice => choice.backend === 'portable')).toBe(true)
    }

    const portableOnly = planArtifactOffers({
      epoch: 2n,
      selectionKey: 'k2',
      shape: singleShape(SMALL_FILE_PORTABLE_THRESHOLD_BYTES),
      capabilities: { ...fullCaps, opfs: false, fsa: false },
    })
    expect(portableOnly.kind).toBe('ready')
    if (portableOnly.kind === 'ready') expect(portableOnly.primary.backend).toBe('portable')
  })

  it('reports portable-limit-exceeded when oversized file has no OPFS or FSA', () => {
    const result = planArtifactOffers({
      epoch: 1n,
      selectionKey: 'k3',
      shape: singleShape(PORTABLE_MAX_BYTES + 1n),
      capabilities: { indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null },
    })
    expect(result).toMatchObject({ kind: 'unavailable', reason: 'portable-limit-exceeded' })
  })

  it('reports storage-full when OPFS lacks quota and no fallback fits', () => {
    const result = planArtifactOffers({
      epoch: 1n,
      selectionKey: 'k4',
      shape: singleShape(PORTABLE_MAX_BYTES + 1024n),
      capabilities: { indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 1024n },
    })
    expect(result).toMatchObject({ kind: 'unavailable', reason: 'storage-full' })
  })

  it('prefers FSA direct folder then OPFS ZIP for multi-item trees', () => {
    const shape: SelectionShape = {
      kind: 'tree',
      suggestedName: 'Album',
      knownFileCount: 3,
      knownBytes: 10_000n,
      complete: true,
    }
    const withFsa = planArtifactOffers({
      epoch: 1n,
      selectionKey: 'k5',
      shape,
      capabilities: { indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 1_000_000n },
    })
    expect(withFsa.kind).toBe('ready')
    if (withFsa.kind === 'ready') {
      expect(withFsa.primary.backend).toBe('fsa')
      expect(withFsa.primary.artifactKind).toBe('directory-tree')
      expect(withFsa.zip?.backend).toBe('opfs')
      expect(withFsa.zip?.suggestedName).toBe('Album.zip')
    }
  })
})
