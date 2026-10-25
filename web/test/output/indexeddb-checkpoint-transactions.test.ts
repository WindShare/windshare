import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  assertCheckpointInstallCapacity,
  assertPristineInitialCheckpoint,
  type IndexedDbCheckpointInventory,
} from '../../src/output/browser/indexeddb/repository-transactions'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  newFileCheckpointV2,
  type FileCheckpointSpec,
} from '../../src/output/persistence/checkpoint'

describe('IndexedDB checkpoint transaction admission', () => {
  it('enforces the operation-wide physical RecordID capacity before installation', () => {
    const candidate = checkpoint()
    const physicalRecordIds = new Set<string>()
    Object.defineProperty(physicalRecordIds, 'size', {
      value: MAX_CHECKPOINT_RECORDS_PER_OPERATION,
    })

    expect(() => assertCheckpointInstallCapacity(
      inventory({ physicalRecordIds }),
      candidate,
    )).toThrow(expect.objectContaining({ name: 'QuotaExceededError' }))
  })

  it('rejects a proposed object already claimed by another physical record', () => {
    const candidate = checkpoint()
    const existing = checkpoint({
      fileId: identity(16, 0x21),
      canonicalPath: ['other.bin'],
    })

    expect(() => assertCheckpointInstallCapacity(
      inventory({ operationRecords: [existing] }),
      candidate,
    )).toThrow(expect.objectContaining({ name: 'InvalidStateError' }))
  })

  it('admits only the pristine candidate-before-object crash state', () => {
    const candidate = checkpoint()
    expect(() => assertPristineInitialCheckpoint(candidate)).not.toThrow()
    expect(() => assertPristineInitialCheckpoint(checkpoint({
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 1n }],
    }))).toThrow('pristine active candidate')
  })
})

function inventory(overrides: Partial<IndexedDbCheckpointInventory>): IndexedDbCheckpointInventory {
  return {
    lineageRecords: [],
    operationRecords: [],
    physicalRecordIds: new Set<string>(),
    crossLineageOwnershipConflict: false,
    ...overrides,
  }
}

function checkpoint(overrides: Partial<FileCheckpointSpec> = {}) {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, 4),
    fileRevision: identity(16, 5),
    canonicalPath: ['file.bin'],
    exactSize: 8n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, 7),
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    ...overrides,
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
