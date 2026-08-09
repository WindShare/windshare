import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  finalFileCheckpointProof,
  validateFileCheckpointPage,
} from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import {
  RECEIVE_RECORD_RECEIPT,
  createPersistedReceiveRecord,
} from '../../src/output/workspace/records'
import { canonicalRecord } from '../../src/output/workspace/canonical'
import { prepareReceiveOperationTransition } from '../../src/output/workspace/repository'

describe('checkpoint journal authority', () => {
  it('requires bounded ordered pages with tail continuations', () => {
    const first = checkpoint(4, 1n)
    const second = checkpoint(5, 1n)
    const binding = durableCheckpointNamespaceIdentity(first)

    expect(validateFileCheckpointPage({
      records: [first],
      nextCursor: first.recordId,
    }, { direction: 'ascending', limit: 1 }, binding).nextCursor).toBe(first.recordId)

    expect(() => validateFileCheckpointPage({
      records: [first],
    }, { direction: 'ascending', limit: 1 }, binding)).toThrow('continuation')

    const ordered = [first, second].sort((left, right) =>
      left.recordId.localeCompare(right.recordId))
    expect(() => validateFileCheckpointPage({
      records: [...ordered].reverse(),
    }, { direction: 'ascending', limit: 3 }, binding)).toThrow('cursor')
  })

  it('creates aggregate proof only from one verified complete checkpoint', () => {
    const record = checkpoint(4, 3n)
    expect(finalFileCheckpointProof(record)).toEqual(expect.objectContaining({
      recordId: record.recordId,
      recordDigest: record.checksum,
      checkpointGeneration: 3n,
      fileId: record.fileId,
      complete: true,
    }))
    expect(finalFileCheckpointProof(record)).not.toHaveProperty('verifiedRanges')
  })

  it('rejects aggregate records that attempt to become a second range authority', async () => {
    const operationId = identity(16, 1)
    const canonicalBytes = canonicalRecord('windshare/receive-receipt/v1', 1, [])
    const receipt = await createPersistedReceiveRecord({
      operationId,
      kind: RECEIVE_RECORD_RECEIPT,
      canonicalBytes,
    })
    const contaminated = {
      ...receipt,
      verifiedRanges: [{ start: 0n, end: 12n }],
    }

    await expect(prepareReceiveOperationTransition({
      operationId,
      records: [contaminated],
    })).rejects.toThrow('cannot own verified ranges')
  })
})

function checkpoint(fileByte: number, generation: bigint): FileCheckpointV2 {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, fileByte),
    fileRevision: identity(16, fileByte + 10),
    canonicalPath: [`file-${fileByte}.bin`],
    exactSize: 12n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, fileByte + 20),
    stateGeneration: 1n,
    checkpointGeneration: generation,
    verifiedRanges: [{ start: 0n, end: 12n }],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
