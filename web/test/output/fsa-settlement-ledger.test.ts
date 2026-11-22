import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  FILE_CHECKPOINT_COMMIT_PUBLISHED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  FILE_CHECKPOINT_PHASE_PUBLISHED,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import type {
  CheckpointNamespaceBinding,
  FileCheckpointPage,
  FileCheckpointScan,
} from '../../src/output/persistence/journal'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import type { PersistentDirectTreeMaterializationEvidence } from '../../src/output/execution/persistent-execution'
import {
  forEachFSAFileCheckpointPage,
  scanAllFSAFileCheckpoints,
  type FSASemanticOutputRepository,
} from '../../src/output/file-system-access/checkpoint-repository'
import { FSASettlementLedgerAuthority } from '../../src/output/file-system-access/settlement-ledger'
import { fsaCheckpointSetDigest, type DirectTreeIntent } from '../../src/output/file-system-access/settlement-proof'

const PAGE_SIZE = 2

describe('FSA settlement ledger checkpoint scanning', () => {
  it('folds terminal settlement totals across pages without retaining checkpoints', async () => {
    const intent = directTreeIntent()
    const committed = [
      checkpoint(intent, 1, ['a.bin'], 10n, true),
      checkpoint(intent, 2, ['b.bin'], 25n, false),
      checkpoint(intent, 3, ['c.bin'], 40n, true),
      checkpoint(intent, 4, ['d.bin'], 5n, true),
      checkpoint(intent, 5, ['e.bin'], 100n, false),
    ]
    const repository = pagedRepository(intent, committed)
    const ledger = new FSASettlementLedgerAuthority({ intent, checkpoints: repository })

    const visitedPages: number[] = []
    await forEachFSAFileCheckpointPage(repository, 'committed', records => {
      visitedPages.push(records.length)
    })
    expect(visitedPages).toEqual([2, 2, 1])

    const scanned = await scanAllFSAFileCheckpoints(repository, 'committed')
    expect(scanned.map(record => record.recordId)).toEqual(
      [...committed].sort((left, right) => compareRecordId(left.recordId, right.recordId)).map(record => record.recordId),
    )

    repository.resetScanCounts()
    const snapshot = await ledger.checkpointSettlementSnapshot(materializationEvidence(intent))

    expect(snapshot).toEqual({
      fileCount: 3n,
      completedBytes: 55n,
      rootFileFinalized: false,
    })
    expect('checkpoints' in snapshot).toBe(false)
    expect(repository.committedScanCalls).toBe(3)
  })

  it('detects a finalized single-file root during page-by-page aggregation', async () => {
    const intent = directTreeIntent()
    const rootCheckpoint = checkpoint(intent, 1, [], 64n, true)
    const repository = pagedRepository(intent, [rootCheckpoint])
    const ledger = new FSASettlementLedgerAuthority({ intent, checkpoints: repository })

    const snapshot = await ledger.checkpointSettlementSnapshot(materializationEvidence(intent))

    expect(snapshot).toEqual({
      fileCount: 1n,
      completedBytes: 64n,
      rootFileFinalized: true,
    })
  })

  it('computes pause settlement totals and checkpoint digest in a single paginated scan', async () => {
    const intent = directTreeIntent()
    const committed = [
      checkpoint(intent, 1, ['a.bin'], 12n, true),
      checkpoint(intent, 2, ['b.bin'], 30n, false),
      checkpoint(intent, 3, ['c.bin'], 18n, true),
    ]
    const repository = pagedRepository(intent, committed)
    const ledger = new FSASettlementLedgerAuthority({ intent, checkpoints: repository })

    const settled = await ledger.resumableCheckpointSettlement(materializationEvidence(intent))
    const expectedOrder = [...committed].sort((left, right) => compareRecordId(left.recordId, right.recordId))

    expect(repository.committedScanCalls).toBe(2)
    expect(settled.snapshot).toEqual({
      fileCount: 2n,
      completedBytes: 30n,
      rootFileFinalized: false,
    })
    expect(settled.evidence.checkpointCount).toBe(3n)
    expect(settled.evidence.checkpoints).toEqual(expectedOrder)
    expect(settled.evidence.checkpointSetDigest).toBe(
      await fsaCheckpointSetDigest(intent, expectedOrder),
    )
  })

  it('rejects foreign evidence and unresolved candidates before scanning committed pages', async () => {
    const intent = directTreeIntent()
    const repository = pagedRepository(intent, [checkpoint(intent, 1, ['a.bin'], 10n, true)], 1n)
    const ledger = new FSASettlementLedgerAuthority({ intent, checkpoints: repository })

    await expect(ledger.checkpointSettlementSnapshot(Object.freeze({
      ...materializationEvidence(intent),
      materializationBindingDigest: identity(32, 99),
    }))).rejects.toThrow(/does not match/)
    await expect(ledger.checkpointSettlementSnapshot(materializationEvidence(intent)))
      .rejects.toThrow(/unresolved checkpoint candidates/)
    await expect(ledger.resumableCheckpointSettlement(materializationEvidence(intent)))
      .rejects.toThrow(/unresolved checkpoint candidates/)
    expect(repository.committedScanCalls).toBe(0)
  })
})

function pagedRepository(
  intent: DirectTreeIntent,
  committed: readonly FileCheckpointV2[],
  candidateCount = 0n,
): FSASemanticOutputRepository & {
  readonly committedScanCalls: number
  resetScanCounts(): void
} {
  const binding: CheckpointNamespaceBinding = durableCheckpointNamespaceIdentity({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.reservation.digest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: intent.plan.reservation.authorityRef,
  })
  const sorted = [...committed].sort((left, right) => compareRecordId(left.recordId, right.recordId))
  let committedScanCalls = 0
  return {
    binding,
    get committedScanCalls() {
      return committedScanCalls
    },
    resetScanCounts() {
      committedScanCalls = 0
    },
    async countCheckpointCandidates(): Promise<bigint> {
      return candidateCount
    },
    async scanCommitted(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
      committedScanCalls += 1
      return slicePage(sorted, scan)
    },
    async scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage> {
      return slicePage([], scan)
    },
    async retireRecoveryMetadataBatch() {
      return Object.freeze({ deletedRows: 0, state: 'complete' as const })
    },
  } as unknown as FSASemanticOutputRepository & {
    readonly committedScanCalls: number
    resetScanCounts(): void
  }
}

function slicePage(
  sorted: readonly FileCheckpointV2[],
  scan: FileCheckpointScan,
): FileCheckpointPage {
  const filtered = scan.cursor === undefined
    ? sorted
    : sorted.filter(record => compareRecordId(record.recordId, scan.cursor!) > 0)
  const limit = Math.min(scan.limit ?? PAGE_SIZE, PAGE_SIZE)
  const records = Object.freeze(filtered.slice(0, limit))
  const nextCursor = filtered.length > limit ? records.at(-1)?.recordId : undefined
  return Object.freeze({
    records,
    ...(nextCursor === undefined ? {} : { nextCursor }),
  })
}

function checkpoint(
  intent: DirectTreeIntent,
  seed: number,
  canonicalPath: readonly string[],
  exactSize: bigint,
  finalized: boolean,
): FileCheckpointV2 {
  return newFileCheckpointV2({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: intent.plan.reservation.digest,
    fileId: identity(16, 10 + seed),
    fileRevision: identity(16, 20 + seed),
    canonicalPath,
    exactSize,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: intent.plan.reservation.authorityRef,
    ownedObjectId: identity(32, 30 + seed),
    stateGeneration: 1n,
    checkpointGeneration: 1n,
    verifiedRanges: finalized
      ? [{ start: 0n, end: exactSize }]
      : [{ start: 0n, end: exactSize / 2n }],
    phase: finalized ? FILE_CHECKPOINT_PHASE_PUBLISHED : FILE_CHECKPOINT_PHASE_PAUSED,
    commitState: finalized ? FILE_CHECKPOINT_COMMIT_PUBLISHED : FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
}

function materializationEvidence(
  intent: DirectTreeIntent,
): PersistentDirectTreeMaterializationEvidence {
  return Object.freeze({
    kind: 'direct-tree-checkpoints',
    materializationBindingDigest: intent.plan.reservation.digest,
    directories: Object.freeze({
      visibleCount: 0n,
      materializedCount: 0n,
      finalizedCount: 0n,
      isolatedCount: 0n,
    }),
  })
}

function directTreeIntent(): DirectTreeIntent {
  return Object.freeze({
    operationId: identity(16, 1),
    digest: identity(32, 2),
    plan: Object.freeze({
      reservation: Object.freeze({
        digest: identity(32, 3),
        authorityRef: identity(32, 4),
      }),
    }),
  }) as unknown as DirectTreeIntent
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}

function compareRecordId(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
