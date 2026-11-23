import { describe, expect, it } from 'vitest'
import {
  singleFileNeedsPicker,
  singleFilePickerFloorBytes,
} from '../../src/application/receive/single-file'
import {
  planArtifactOffers,
  PORTABLE_MAX_BYTES,
  type BrowserOutputCapabilities,
} from '../../src/output/backends/contract'

const MIB = 1024n * 1024n

function primarySingleFileBackend(
  capabilities: BrowserOutputCapabilities,
  exactBytes: bigint,
): string {
  const offers = planArtifactOffers({
    epoch: 1n,
    selectionKey: 'sel-single',
    capabilities,
    shape: {
      kind: 'single-file',
      file: {
        kind: 'file',
        id: new Uint8Array(16).fill(1),
        idText: 'file-1',
        name: 'sample.bin',
        expectedSize: exactBytes,
      },
      sourcePath: ['sample.bin'],
      exactBytes,
    },
  })
  return offers.kind === 'ready' ? offers.primary.backend : offers.kind
}

describe('singleFilePickerFloorBytes and singleFileNeedsPicker', () => {
  it('returns null whenever FSA can never be selected as the primary backend', () => {
    expect(singleFilePickerFloorBytes(undefined)).toBeNull()
    expect(singleFilePickerFloorBytes({
      indexedDb: true, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    })).toBeNull()
    expect(singleFilePickerFloorBytes({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: null,
    })).toBeNull()
    expect(singleFilePickerFloorBytes({
      indexedDb: true, fsa: true, opfs: true, portable: false, availableStorageBytes: null,
    })).toBeNull()
  })

  it('computes the exact FSA primary-backend threshold and stays equivalent to planArtifactOffers', () => {
    const cases: ReadonlyArray<Readonly<{ caps: BrowserOutputCapabilities; expectedFloor: bigint }>> = [
      {
        caps: { indexedDb: true, fsa: true, opfs: false, portable: false, availableStorageBytes: null },
        expectedFloor: 0n,
      },
      {
        caps: { indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null },
        expectedFloor: PORTABLE_MAX_BYTES + 1n,
      },
      {
        caps: { indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 32n * MIB },
        expectedFloor: PORTABLE_MAX_BYTES + 1n,
      },
      {
        caps: { indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 128n * MIB },
        expectedFloor: 128n * MIB + 1n,
      },
      {
        caps: { indexedDb: true, fsa: true, opfs: true, portable: false, availableStorageBytes: 0n },
        expectedFloor: 1n,
      },
      {
        caps: { indexedDb: true, fsa: true, opfs: true, portable: false, availableStorageBytes: 20n * MIB },
        expectedFloor: 20n * MIB + 1n,
      },
    ]

    for (const { caps, expectedFloor } of cases) {
      const floor = singleFilePickerFloorBytes(caps)
      expect(floor).toBe(expectedFloor)
      expect(singleFileNeedsPicker(expectedFloor, floor)).toBe(true)
      expect(primarySingleFileBackend(caps, expectedFloor)).toBe('fsa')
      if (expectedFloor > 0n) {
        const below = expectedFloor - 1n
        expect(singleFileNeedsPicker(below, floor)).toBe(false)
        expect(primarySingleFileBackend(caps, below)).not.toBe('fsa')
      }
    }
  })

  it('handles null floor, unknown sizes, and exact boundary comparisons in singleFileNeedsPicker', () => {
    expect(singleFileNeedsPicker(100n, null)).toBe(false)
    expect(singleFileNeedsPicker(undefined, 0n)).toBe(false)
    expect(singleFileNeedsPicker(undefined, 100n)).toBe(false)
    expect(singleFileNeedsPicker(0n, 0n)).toBe(true)
    expect(singleFileNeedsPicker(99n, 100n)).toBe(false)
    expect(singleFileNeedsPicker(100n, 100n)).toBe(true)
    expect(singleFileNeedsPicker(101n, 100n)).toBe(true)
  })
})
