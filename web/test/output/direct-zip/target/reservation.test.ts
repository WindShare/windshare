import { describe, expect, it, vi } from 'vitest'
import { createDirectZipTarget } from '../../../../src/output/direct-zip/target'
import { createWriterHarness } from '../writer/fault-model'

describe('Direct ZIP bootstrap recovery authority', () => {
  it('retains the durable candidate when its exact-name file contains a partial marker', async () => {
    const writerHarness = createWriterHarness()
    const parent = Object.freeze({ id: 'parent' })
    const file = Object.freeze({ id: 'file' })
    const retireCandidate = vi.fn(async () => undefined)
    const target = createDirectZipTarget({
      fileSystem: {
        queryPermission: vi.fn(async () => 'granted' as const),
        requestPermission: vi.fn(async () => 'granted' as const),
        lookupExactName: vi.fn(async () => ({ kind: 'file' as const, handle: file })),
        createFile: vi.fn(),
        snapshot: vi.fn(async () => ({
          size: 1n,
          lastModified: 1,
          read: vi.fn(async () => Uint8Array.of(0x50)),
        })),
        createWritable: vi.fn(),
        removeExactName: vi.fn(),
      },
      handleBindings: {
        compareParent: vi.fn(async () => 'same' as const),
        compareFile: vi.fn(),
        compareCurrentFiles: vi.fn(),
        bindFile: vi.fn(),
      },
      reservations: { persistCandidate: vi.fn(), retireCandidate },
      operationLeases: {
        acquire: vi.fn(async () => ({
          leaseId: 'lease', generation: 1n, release: vi.fn(async () => undefined),
        })),
      },
      parentLocks: {
        acquire: vi.fn(async () => ({ name: 'parent', release: vi.fn(async () => undefined) })),
      },
      random: { bytes: vi.fn() },
    })
    const candidate = {
      operationId: writerHarness.marker.operationId,
      candidateId: writerHarness.marker.candidateId,
      resultRootComponent: 'root',
      stableName: 'target.zip',
      ownershipNonce: writerHarness.marker.ownershipNonce,
      targetRef: new Uint8Array(32).fill(5),
      bindingDigest: writerHarness.marker.bindingDigest,
      marker: writerHarness.marker,
      parentBinding: {
        handleRef: 'parent', bindingDigest: new Uint8Array(32).fill(6), persistedHandle: parent,
      },
    } as const

    const result = await target.resumeBootstrap({
      candidate,
      currentParent: parent,
      trustedAction: true,
    })

    expect(result).toEqual(expect.objectContaining({
      kind: 'gated',
      retainedEffect: candidate,
      decision: expect.objectContaining({
        kind: 'target-verification-required',
        reason: 'ownership-marker-incomplete',
      }),
    }))
    expect(retireCandidate).not.toHaveBeenCalled()
  })
})
