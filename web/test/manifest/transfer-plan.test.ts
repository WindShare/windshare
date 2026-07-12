import { describe, expect, it } from 'vitest'

import { bytesToHex } from '../../src/crypto'
import {
  PackedLayout,
  createSelection,
  createTransferPlan,
} from '../../src/manifest'
import { loadVectorFile } from '../vectors'
import { encodedManifest, vectorManifest, wireEntry } from './fixtures'
import { decodeCanonicalManifest } from '../../src/manifest/cbor'

interface ChunkRangeVector {
  readonly first: string
  readonly end: string
}

interface TransferPlanVector {
  readonly name: string
  readonly selectors: readonly string[] | null
  readonly selectedPaths: readonly string[]
  readonly selectedBytes: string
  readonly chunks: readonly ChunkRangeVector[]
  readonly planId: string
}

const vectors = loadVectorFile(
  new URL('../../../testvectors/transfer-plan.json', import.meta.url),
).cases as unknown as readonly TransferPlanVector[]

const utf8OrderingManifest = decodeCanonicalManifest(
  encodedManifest([
    wireEntry('𐀀-supplementary.txt', 0, 1),
    wireEntry('\uE000-bmp-private.txt', 0, 2),
  ]),
)

describe('immutable transfer plans', () => {
  it.each(vectors)('matches the Go selection and PlanID vector: $name', async (vector) => {
    const manifest =
      vector.name === 'utf8-byte-order-not-utf16'
        ? utf8OrderingManifest
        : vectorManifest()
    const plan = await createTransferPlan(manifest, vector.selectors)

    expect(plan.selectedEntries.map((entry) => entry.path)).toEqual(vector.selectedPaths)
    expect(String(plan.selectedBytes)).toBe(vector.selectedBytes)
    expect(
      plan.chunks.ranges.map((range) => ({
        first: String(range.first),
        end: String(range.end),
      })),
    ).toEqual(vector.chunks)
    expect(bytesToHex(plan.planId)).toBe(vector.planId)
  })

  it('canonicalizes selectors before deduplication and identity derivation', async () => {
    const manifest = vectorManifest()
    const composed = await createTransferPlan(manifest, ['tree/nai\u0308ve.txt'])
    const canonical = await createTransferPlan(manifest, ['tree/naïve.txt'])

    expect(composed.selectedEntries.map((entry) => entry.path)).toEqual([
      'tree/naïve.txt',
    ])
    expect(composed.planId).toEqual(canonical.planId)
    expect(composed.chunks).toEqual(canonical.chunks)
  })

  it('maps shared boundary chunks without materializing unselected siblings', () => {
    const layout = new PackedLayout(vectorManifest())

    expect(layout.chunkRanges(1)).toEqual([
      { path: 'tree/a.txt', offset: 1_024, length: 476 },
      { path: 'tree/b.bin', offset: 0, length: 548 },
    ])
    expect(layout.chunkRanges(2)).toEqual([
      { path: 'tree/b.bin', offset: 548, length: 152 },
      { path: 'tree/naïve.txt', offset: 0, length: 100 },
    ])
  })

  it('keeps null/all distinct from an intentionally empty selection', () => {
    expect(createSelection(null)).toEqual({ kind: 'all' })
    expect(createSelection([])).toEqual({ kind: 'paths', paths: [] })
  })

  it('rejects unknown and unsafe selectors instead of producing an empty plan', async () => {
    const manifest = vectorManifest()

    await expect(createTransferPlan(manifest, ['tree/missing'])).rejects.toMatchObject({
      code: 'unknown-selector',
    })
    await expect(createTransferPlan(manifest, ['../tree'])).rejects.toMatchObject({
      code: 'invalid-path',
    })
  })

  it('freezes plan projections and keeps full demand compact', async () => {
    const plan = await createTransferPlan(vectorManifest(), null)

    expect(Object.isFrozen(plan)).toBe(true)
    expect(Object.isFrozen(plan.selectedEntries)).toBe(true)
    expect(plan.chunks.ranges).toHaveLength(1)
    expect(() => (plan.selectedEntries as unknown as unknown[]).push({})).toThrow(TypeError)
  })
})
