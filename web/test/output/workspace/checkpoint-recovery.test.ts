import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
} from '../../../src/output/persistence/checkpoint'
import { durableCheckpointNamespaceIdentity } from '../../../src/output/persistence/namespace'
import {
  recoverFileCheckpointCandidates,
  type FileCheckpointCandidateProbe,
  type FileCheckpointRecoveryRepository,
} from '../../../src/output/persistent-tree/recovery'

describe('file checkpoint candidate recovery', () => {
  it('leaves ownership-unknown candidates unresolved for aggregate NeedsAttention', async () => {
    const candidate = newFileCheckpointV2({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
      materializationBindingDigest: identity(32, 3),
      fileId: identity(16, 4),
      fileRevision: identity(16, 5),
      canonicalPath: ['file.bin'],
      exactSize: 12n,
      materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
      authorityRef: identity(32, 6),
      ownedObjectId: identity(32, 7),
      stateGeneration: 1n,
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 4n }],
      phase: FILE_CHECKPOINT_PHASE_ACTIVE,
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    let resolved = false
    const repository: FileCheckpointRecoveryRepository = {
      binding: durableCheckpointNamespaceIdentity(candidate),
      scanCandidates: async () => ({ records: [candidate] }),
      readCommitted: async () => undefined,
      resolveCandidate: async () => {
        resolved = true
      },
    }
    const probe: FileCheckpointCandidateProbe = {
      observe: async () => ({ kind: 'ownership-unknown' }),
    }

    await expect(recoverFileCheckpointCandidates(repository, probe)).resolves.toEqual({
      resolved: 0,
      unknownRecordIds: [candidate.recordId],
    })
    expect(resolved).toBe(false)
  })
})

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
