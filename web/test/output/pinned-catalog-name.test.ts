import { describe, expect, it, vi } from 'vitest'
import { encodeCatalogName } from '../../src/catalog/path-policy'
import { openCatalogObject } from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createSignedCatalogFixture } from '../catalog/signed-catalog-fixture'
import {
  canonicalFrame, canonicalPath, canonicalText, canonicalU64, concatCanonicalBytes,
} from '../../src/transfer/encoding/canonical'
import { rawFileObjectId } from '../../src/output/origin-private/workspace-tree'
import {
  canonicalMaterializationPath, decodeMaterializationPath,
} from '../../src/output/materialization-ledger/model'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  FILE_CHECKPOINT_PHASE_ACTIVE, newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { finalFileCheckpointProof } from '../../src/output/persistence/journal'
import { decodeMaterializedManifestV1, sealMaterializedManifest } from '../../src/output/workspace/manifest'
import { createPublicationAttempt } from '../../src/output/workspace/aggregate'
import { createHandoffReceipt } from '../../src/output/workspace/receipts'
import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS, type ArtifactChoiceID } from '../../src/transfer/intent'
import {
  createDirectZipBootstrapCandidateV1, validateDirectZipBootstrapCandidateV1,
} from '../../src/output/direct-zip/journal/records'
import { directZipJournalBudgetDigestV1 } from '../../src/output/direct-zip/journal/budget'
import { encodeTaskCheckpoint, type TaskCheckpoint } from '../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../src/output/origin-private/task-checkpoint/store'
import { WorkspaceProgressiveStages } from '../../src/output/workspace/stages/progressive'
import type { WorkspaceStageRuntime } from '../../src/output/workspace/stages/runtime'

const NAME = '\u{16D63}\u{16D67}.txt'
const COMPOSED_NAME = '\u{16D69}.txt'
const id = (value: number, width = 16) => new Uint8Array(width).fill(value)
const identity = (value: number, width = 16) => encodeBase64Url(id(value, width))
const bytes = (value: string) => new TextEncoder().encode(value)

describe('frozen catalog names throughout output ownership', () => {
  it('preserves authenticated names through output identity, checkpoint, manifest and recovery', async () => {
    const fixture = await createSignedCatalogFixture()
    try {
      const directoryId = fixture.descriptor.syntheticRoot
      const generation = id(3)
      const sealed = await fixture.sealPage({
        directoryId, generation, pageIndex: 0, terminal: true, previousCommitment: new Uint8Array(32),
        entries: [{ kind: 'file', id: id(4), name: NAME, expectedSize: 12n }],
      })
      const opened = await openCatalogObject(sealed.object, fixture.descriptor, fixture.readSecret,
        { directoryId, generation, pageIndex: 0 })
      if (opened.kind !== 'page') throw new Error('Expected an authenticated page')
      const name = opened.page.entries[0]!.name
      expect(name).toBe(NAME)
      const revision = { fileId: identity(4), fileRevision: identity(5), exactSize: 12n }
      const ownedObjectId = await rawFileObjectId(identity(1), [name], revision)
      expect(ownedObjectId).not.toBe(await rawFileObjectId(identity(1), [COMPOSED_NAME], revision))
      expect(decodeMaterializationPath(canonicalMaterializationPath([name]))).toEqual([name])

      const checkpoint = newFileCheckpointV2({
        operationId: identity(1), receiveIntentDigest: identity(2, 32),
        materializationBindingDigest: identity(3, 32), ...revision, canonicalPath: [name],
        materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
        authorityRef: identity(6, 32), ownedObjectId, stateGeneration: 2n, checkpointGeneration: 3n,
        verifiedRanges: [{ start: 0n, end: 12n }], phase: FILE_CHECKPOINT_PHASE_ACTIVE,
        commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
      })
      const proof = finalFileCheckpointProof(checkpoint)
      const checkpoints = {
        readFinalCheckpoint: async () => proof,
        recoverFinalCheckpoint: async () => proof,
      }
      const manifest = await sealMaterializedManifest({
        operationId: checkpoint.operationId, receiveIntentDigest: checkpoint.receiveIntentDigest,
        materializationBindingDigest: checkpoint.materializationBindingDigest, generations: [],
        entries: [{
          kind: 'file', artifactPath: [name], ...revision, ownedObjectId,
          checkpoint: { recordId: checkpoint.recordId, recordDigest: checkpoint.checksum,
            checkpointGeneration: checkpoint.checkpointGeneration },
        }],
        checkpoints,
      })
      const restored = await decodeMaterializedManifestV1({
        canonicalBytes: manifest.canonicalBytes, operationId: checkpoint.operationId,
        receiveIntentDigest: checkpoint.receiveIntentDigest,
        materializationBindingDigest: checkpoint.materializationBindingDigest, checkpoints,
      })
      expect(restored).toEqual(manifest)
      expect(restored.entries[0]?.artifactPath).toEqual([NAME])
    } finally { fixture.close() }
  })

  it('preserves names in browser publication attempts and handoff receipts', async () => {
    const input = { operationId: identity(1), receiveIntentDigest: identity(2, 32),
      publicationAttemptId: identity(3), packagedArtifactDigest: identity(4, 32) }
    const attempt = await createPublicationAttempt({ ...input, route: {
      kind: 'handoff', suggestedName: NAME,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS, packagedFileSupported: true,
    } })
    expect(attempt.route).toMatchObject({ suggestedName: NAME })
    const receipt = await createHandoffReceipt({
      ...input, suggestedName: NAME, urlLeaseEndsAt: 1000, handoffStarted: true,
    })
    expect(receipt.suggestedName).toBe(NAME)
    expect(receipt.digest).not.toBe((await createHandoffReceipt({
      ...input, suggestedName: COMPOSED_NAME, urlLeaseEndsAt: 1000, handoffStarted: true,
    })).digest)
  })

  it('reopens Direct ZIP bootstrap candidates without renaming their physical target', async () => {
    const choiceId = identity(4, 32) as ArtifactChoiceID
    const candidate = await createDirectZipBootstrapCandidateV1({
      operationId: identity(1), candidateId: identity(2), leaseId: identity(3), leaseGeneration: 1n,
      selectionCanonicalBytes: Uint8Array.of(1), artifactCanonicalBytes: Uint8Array.of(2),
      choiceIdentityCanonicalBytes: Uint8Array.of(3), choiceId, preClickRanking: [choiceId],
      stablePhysicalName: NAME + '.zip', ownershipNonce: identity(5, 32),
      targetBindingDigest: identity(6, 32), parentHandleId: 'parent',
      policies: { encodingPolicyDigest: identity(7, 32), layoutPolicyDigest: identity(8, 32),
        checkpointPolicyDigest: identity(9, 32), epochPolicyDigest: identity(10, 32),
        journalBudgetDigest: await directZipJournalBudgetDigestV1() },
    })
    const restored = await validateDirectZipBootstrapCandidateV1(structuredClone(candidate))
    expect(restored).toEqual(candidate)
    expect(restored.stablePhysicalName).toBe(NAME + '.zip')
    await expect(createDirectZipBootstrapCandidateV1({ ...candidate, parentHandleId: 'e\u0301' }))
      .rejects.toThrow('well-formed NFC Unicode')
  })

  it('pauses a progressive ZIP with its exact selected names and fences a different selection', async () => {
    const checkpoint = zipCheckpoint(NAME)
    const store = { readCheckpoint: async () => checkpoint, readEntries: async () => [] } as unknown as TaskCheckpointStore
    const commitTransition = vi.fn(async () => undefined)
    const runtime = {
      intent: { operationId: checkpoint.object.operationId, digest: identity(2, 32) },
      leaseId: identity(3), repository: { commitTransition }, emit: vi.fn(),
      lifecycle: async () => ({ kind: 'receiving', operationId: checkpoint.object.operationId,
        receiveIntentDigest: identity(2, 32), generation: 1n, activeLeaseId: identity(3) }),
    } as unknown as WorkspaceStageRuntime
    const stage = new WorkspaceProgressiveStages(runtime)
    await expect(stage.pause(store, structuredClone(checkpoint))).resolves.toMatchObject({
      kind: 'resumable-receive', payloadKind: 'workspace-archive',
    })
    expect(commitTransition).toHaveBeenCalledOnce()
    await expect(stage.pause(store, zipCheckpoint(COMPOSED_NAME))).rejects.toThrow('matching committed checkpoint')
    expect(commitTransition).toHaveBeenCalledOnce()
    expect(JSON.parse(new TextDecoder().decode(encodeTaskCheckpoint(checkpoint))).selectedPaths).toEqual([[NAME]])
  })

  it.each(['ordinary.txt', '共享文件.txt'])('keeps existing canonical bytes for %s', name => {
    expect(canonicalPath([name])).toEqual(concatCanonicalBytes([
      canonicalU64(1n), canonicalFrame(canonicalText(name)),
    ]))
    expect(encodeTaskCheckpoint(zipCheckpoint(name))).toEqual(bytes(JSON.stringify(
      zipCheckpoint(name), (_, value: unknown) => typeof value === 'bigint' ? value.toString() : value,
    )))
  })

  it.each(['e\u0301', '\ufeffname', 'a/b', 'a\\b', '..', '', '\ud800'])(
    'rejects unsafe or noncanonical path fields without normalizing: %j', name => {
      expect(() => encodeCatalogName(name)).toThrow()
      expect(() => canonicalPath([name])).toThrow()
      expect(() => encodeTaskCheckpoint(zipCheckpoint(name))).toThrow()
      const encoded = concatCanonicalBytes([canonicalU64(1n), canonicalFrame(bytes(name))])
      // TextEncoder replaces an unpaired surrogate; malformed wire is covered separately.
      if (name !== '\ud800') expect(() => decodeMaterializationPath(encoded)).toThrow()
    },
  )

  it('retains root paths, rejects malformed UTF-8 and leaves generic NFC strict', () => {
    expect(decodeMaterializationPath(canonicalMaterializationPath([]))).toEqual([])
    const malformed = concatCanonicalBytes([canonicalU64(1n), canonicalFrame(Uint8Array.of(0xed, 0xa0, 0x80))])
    expect(() => decodeMaterializationPath(malformed)).toThrow()
    expect(() => canonicalText('e\u0301')).toThrow()
  })
})

function zipCheckpoint(name: string): TaskCheckpoint {
  return {
    object: { operationId: identity(1), objectId: identity(5, 32), kind: 'zip-archive', handleId: 'archive' },
    generation: 1n, allocatedLength: 0n, physicalLength: 0n, entryCount: 0n,
    discoveryComplete: false, selectedPaths: [[name]], artifactState: 'receiving',
  }
}
