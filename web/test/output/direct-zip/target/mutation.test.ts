import { describe, expect, it, vi } from 'vitest'
import {
  chainDirectZipEpochDigestV1,
  concatDirectZipBytes,
  digestDirectZipArchiveBytes,
  directZipEpochGenesisRoot,
} from '../../../../src/output/direct-zip/format'
import { createDirectZipTarget } from '../../../../src/output/direct-zip/target'
import { createWriterHarness } from '../writer/fault-model'

describe('Direct ZIP ownership-safe cleanup', () => {
  it('requires the staged candidate range proof before deleting a candidate-length target', async () => {
    const writerHarness = createWriterHarness()
    const predecessorBytes = writerHarness.target.visible
    const candidateBytes = Uint8Array.of(0x51, 0x52, 0x53)
    const visible = concatDirectZipBytes([predecessorBytes, candidateBytes])
    let present = true
    const handle = Object.freeze({ id: 'file' })
    const parent = Object.freeze({ id: 'parent' })
    const removeExactName = vi.fn(async () => { present = false })
    const target = createDirectZipTarget({
      fileSystem: {
        queryPermission: vi.fn(async () => 'granted' as const),
        requestPermission: vi.fn(async () => 'granted' as const),
        lookupExactName: vi.fn(async () => present
          ? { kind: 'file' as const, handle }
          : { kind: 'absent' as const }),
        createFile: vi.fn(),
        snapshot: vi.fn(async () => ({
          size: BigInt(visible.byteLength),
          lastModified: 1,
          read: async (start: bigint, end: bigint) => visible.slice(Number(start), Number(end)),
        })),
        createWritable: vi.fn(),
        removeExactName,
      },
      handleBindings: {
        compareParent: vi.fn(async () => 'same' as const),
        compareFile: vi.fn(async () => 'same' as const),
        compareCurrentFiles: vi.fn(async () => 'same' as const),
        bindFile: vi.fn(),
      },
      reservations: { persistCandidate: vi.fn(), retireCandidate: vi.fn() },
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
    const predecessorContentDigest = digestDirectZipArchiveBytes(predecessorBytes)
    const predecessorEpochRoot = chainDirectZipEpochDigestV1({
      predecessorRoot: directZipEpochGenesisRoot(),
      start: 0n,
      end: BigInt(predecessorBytes.byteLength),
      contentDigest: predecessorContentDigest,
    })
    const candidateContentDigest = digestDirectZipArchiveBytes(candidateBytes)
    const candidateEpochRoot = chainDirectZipEpochDigestV1({
      predecessorRoot: predecessorEpochRoot,
      start: BigInt(predecessorBytes.byteLength),
      end: BigInt(visible.byteLength),
      contentDigest: candidateContentDigest,
    })
    const binding = {
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
      fileBinding: {
        handleRef: 'file', bindingDigest: new Uint8Array(32).fill(7), persistedHandle: handle,
      },
      bootstrapPrefixLength: BigInt(predecessorBytes.byteLength),
    } as const
    const predecessor = {
      committedLength: BigInt(predecessorBytes.byteLength),
      observation: {} as never,
      committedEpochs: [{
        start: 0n,
        end: BigInt(predecessorBytes.byteLength),
        predecessorRoot: directZipEpochGenesisRoot(),
        epochRoot: predecessorEpochRoot,
      }],
    } as const
    const candidate = {
      stagedEnd: BigInt(visible.byteLength),
      observation: {} as never,
      epoch: {
        start: BigInt(predecessorBytes.byteLength),
        end: BigInt(visible.byteLength),
        predecessorRoot: predecessorEpochRoot,
        epochRoot: candidateEpochRoot,
      },
    } as const

    const refused = await target.deleteProvenTarget({
      binding,
      currentParent: parent,
      predecessor,
      candidate: {
        ...candidate,
        epoch: { ...candidate.epoch, epochRoot: new Uint8Array(32) },
      },
      trustedAction: true,
    })
    expect(refused.kind).toBe('gated')
    expect(removeExactName).not.toHaveBeenCalled()

    await expect(target.deleteProvenTarget({
      binding,
      currentParent: parent,
      predecessor,
      candidate,
      trustedAction: true,
    })).resolves.toEqual({ kind: 'ready', value: { disposition: 'deleted' } })
    expect(removeExactName).toHaveBeenCalledOnce()
  })
})
