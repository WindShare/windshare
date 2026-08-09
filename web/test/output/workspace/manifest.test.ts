import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
} from '../../../src/output/persistence/checkpoint'
import { finalFileCheckpointProof } from '../../../src/output/persistence/journal'
import { canonicalText } from '../../../src/output/workspace/canonical'
import {
  decodeMaterializedManifestV1,
  sealMaterializedManifest,
} from '../../../src/output/workspace/manifest'

describe('MaterializedManifestV1', () => {
  it('rejects malformed Unicode before hashing canonical text', () => {
    expect(() => canonicalText('\ud800')).toThrow('well-formed NFC Unicode')
  })

  it('seals only checkpoint-proven entries and records derived canonical bytes', async () => {
    const checkpoint = finalCheckpoint()
    const proof = finalFileCheckpointProof(checkpoint)
    const manifest = await sealMaterializedManifest({
      operationId: checkpoint.operationId,
      receiveIntentDigest: checkpoint.receiveIntentDigest,
      materializationBindingDigest: checkpoint.materializationBindingDigest,
      preparationBinding: { kind: 'absent' },
      generations: [{
        directoryId: identity(16, 8),
        generation: identity(16, 9),
      }],
      entries: [
        {
          kind: 'file',
          artifactPath: checkpoint.canonicalPath,
          fileId: checkpoint.fileId,
          fileRevision: checkpoint.fileRevision,
          exactSize: checkpoint.exactSize,
          ownedObjectId: checkpoint.ownedObjectId,
          checkpoint: {
            recordId: checkpoint.recordId,
            recordDigest: checkpoint.checksum,
            checkpointGeneration: checkpoint.checkpointGeneration,
          },
        },
        {
          kind: 'directory',
          artifactPath: ['folder'],
          directoryId: identity(16, 8),
          generation: identity(16, 9),
          ownedObjectId: identity(32, 11),
        },
      ],
      checkpoints: {
        readFinalCheckpoint: async () => proof,
      },
    })

    expect(manifest).toEqual(expect.objectContaining({
      entryCount: 2n,
      fileCount: 1n,
      directoryCount: 1n,
      rawBytes: 12n,
      canonicalMetadataBytes: BigInt(manifest.canonicalBytes.byteLength),
    }))
    expect(manifest.entries[1]).not.toHaveProperty('verifiedRanges')
    const recovery = {
      readFinalCheckpoint: async () => proof,
      recoverFinalCheckpoint: async () => proof,
    }
    await expect(decodeMaterializedManifestV1({
      canonicalBytes: manifest.canonicalBytes,
      operationId: checkpoint.operationId,
      receiveIntentDigest: checkpoint.receiveIntentDigest,
      materializationBindingDigest: checkpoint.materializationBindingDigest,
      checkpoints: recovery,
    })).resolves.toEqual(manifest)
    await expect(decodeMaterializedManifestV1({
      canonicalBytes: manifest.canonicalBytes,
      operationId: checkpoint.operationId,
      receiveIntentDigest: checkpoint.receiveIntentDigest,
      materializationBindingDigest: checkpoint.materializationBindingDigest,
      checkpoints: {
        ...recovery,
        recoverFinalCheckpoint: async () => undefined,
      },
    })).rejects.toThrow('unique final checkpoint authority')
  })

  it('sorts entries by unsigned UTF-8 artifact path rather than encoded segment count', async () => {
    const generations = [8, 9, 10].map((value) => ({
      directoryId: identity(16, value),
      generation: identity(16, value + 10),
    }))
    const manifest = await sealMaterializedManifest({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
      materializationBindingDigest: identity(32, 3),
      preparationBinding: { kind: 'absent' },
      generations,
      entries: [
        directoryEntry(['z'], 10),
        directoryEntry(['a', 'b'], 9),
        directoryEntry(['a'], 8),
      ],
      checkpoints: { readFinalCheckpoint: async () => undefined },
    })

    expect(manifest.entries.map((entry) => entry.artifactPath.join('/'))).toEqual([
      'a',
      'a/b',
      'z',
    ])
  })

  it('rejects a file when final checkpoint identity or coverage cannot be proven', async () => {
    const checkpoint = finalCheckpoint()
    const proof = finalFileCheckpointProof(checkpoint)
    await expect(sealMaterializedManifest({
      operationId: checkpoint.operationId,
      receiveIntentDigest: checkpoint.receiveIntentDigest,
      materializationBindingDigest: checkpoint.materializationBindingDigest,
      preparationBinding: { kind: 'absent' },
      generations: [],
      entries: [{
        kind: 'file',
        artifactPath: ['different.bin'],
        fileId: checkpoint.fileId,
        fileRevision: checkpoint.fileRevision,
        exactSize: checkpoint.exactSize,
        ownedObjectId: checkpoint.ownedObjectId,
        checkpoint: {
          recordId: checkpoint.recordId,
          recordDigest: checkpoint.checksum,
          checkpointGeneration: checkpoint.checkpointGeneration,
        },
      }],
      checkpoints: { readFinalCheckpoint: async () => proof },
    })).rejects.toThrow('not proven')
  })
})

function directoryEntry(path: readonly string[], value: number) {
  return {
    kind: 'directory' as const,
    artifactPath: path,
    directoryId: identity(16, value),
    generation: identity(16, value + 10),
    ownedObjectId: identity(32, value + 20),
  }
}

function finalCheckpoint() {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, 4),
    fileRevision: identity(16, 5),
    canonicalPath: ['folder', 'file.bin'],
    exactSize: 12n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, 7),
    stateGeneration: 2n,
    checkpointGeneration: 3n,
    verifiedRanges: [{ start: 0n, end: 12n }],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
