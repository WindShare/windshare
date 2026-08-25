import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createMaterializationLedgerBinding,
  materializationLedgerEntryCursor,
} from '../../src/output/materialization-ledger/codec'
import {
  MaterializationLedgerEvidenceOutcome,
  deriveMaterializationLedgerSealId,
  validateMaterializationLedgerEvidence,
} from '../../src/output/materialization-ledger/evidence'
import {
  createFinalizedFileMaterializationRecords,
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
} from '../../src/output/materialization-ledger/journal'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerSealPurpose,
  type MaterializationLedgerEntryV1,
} from '../../src/output/materialization-ledger/model'
import {
  createMaterializationLedgerPageSummary,
  sealMaterializationLedgerPages,
} from '../../src/output/materialization-ledger/page'
import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { outputFault, FaultScope, OutputFaultCode } from '../../src/transfer/fault'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { VerifiedFinalOutputFile } from '../../src/transfer/output-session'

describe('materialization ledger settlement evidence', () => {
  it('validates a published single-file [] fact against worker counts', async () => {
    const binding = await ledgerBinding()
    const file = await fileEntry(binding, [], 10, 12n)
    const seal = await sealEntries(binding, [file], 1n)

    expect(validateMaterializationLedgerEvidence({
      seal,
      worker: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 12n },
      rootExpectation: { kind: 'none', anchorKind: 'single-file' },
      outcome: MaterializationLedgerEvidenceOutcome.Published,
    })).toEqual(expect.objectContaining({
      fileCount: 1n,
      visibleDirectoryCount: 0n,
      completedBytes: 12n,
      aggregateRoot: seal.aggregateRoot,
    }))
  })

  it('distinguishes the materialized [] directory from visible directories', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding, 20)
    const final = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
    })
    const seal = await sealEntries(binding, [final, root], 2n)

    expect(seal.materializedDirectoryCount).toBe(1n)
    expect(seal.visibleDirectoryCount).toBe(0n)
    for (const anchorKind of ['directory', 'synthetic-root'] as const) {
      expect(validateMaterializationLedgerEvidence({
        seal,
        worker: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
        rootExpectation: {
          kind: 'materialized-directory',
          anchorKind,
          directoryId: root.directoryId,
          relativePath: path([]),
        },
        outcome: MaterializationLedgerEvidenceOutcome.Published,
      }).materializedDirectoryCount).toBe(1n)
    }
  })

  it('rejects worker count/byte mismatch and a foreign root expectation', async () => {
    const binding = await ledgerBinding()
    const file = await fileEntry(binding, [], 10, 12n)
    const seal = await sealEntries(binding, [file], 3n)

    expect(() => validateMaterializationLedgerEvidence({
      seal,
      worker: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 11n },
      rootExpectation: { kind: 'none', anchorKind: 'single-file' },
      outcome: MaterializationLedgerEvidenceOutcome.Published,
    })).toThrow('worker summary disagrees')
    expect(() => validateMaterializationLedgerEvidence({
      seal,
      worker: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 12n },
      rootExpectation: {
        kind: 'materialized-directory',
        anchorKind: 'directory',
        directoryId: identity(16, 90),
        relativePath: path([]),
      },
      outcome: MaterializationLedgerEvidenceOutcome.Published,
    })).toThrow('claimed by a file')
  })

  it('permits typed isolated directory outcomes only for partial terminal evidence', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding, 20)
    const isolated = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
      fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    })
    const seal = await sealEntries(binding, [root, isolated], 4n)
    const evidence = {
      seal,
      worker: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      rootExpectation: {
        kind: 'materialized-directory' as const,
        anchorKind: 'synthetic-root' as const,
        directoryId: root.directoryId,
        relativePath: path([]),
      },
    }

    expect(validateMaterializationLedgerEvidence({
      ...evidence,
      outcome: MaterializationLedgerEvidenceOutcome.Partial,
    }).isolatedDirectoryCount).toBe(1n)
    expect(() => validateMaterializationLedgerEvidence({
      ...evidence,
      outcome: MaterializationLedgerEvidenceOutcome.Published,
    })).toThrow('root directory is not finalized')
  })

  it('allows an incomplete resumable snapshot but rejects candidates and purpose confusion', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding, 20)
    const resumable = await sealEntries(
      binding,
      [root],
      5n,
      MaterializationLedgerSealPurpose.ResumableSnapshot,
    )
    const evidence = {
      seal: resumable,
      worker: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      rootExpectation: {
        kind: 'materialized-directory' as const,
        anchorKind: 'directory' as const,
        directoryId: root.directoryId,
        relativePath: path([]),
      },
    }
    expect(validateMaterializationLedgerEvidence({
      ...evidence,
      outcome: MaterializationLedgerEvidenceOutcome.Resumable,
    }).entryEventCount).toBe(1n)
    expect(() => validateMaterializationLedgerEvidence({
      ...evidence,
      outcome: MaterializationLedgerEvidenceOutcome.Partial,
    })).toThrow('terminal ledger seal')

    const withCandidate = await sealEntries(
      binding,
      [root],
      6n,
      MaterializationLedgerSealPurpose.ResumableSnapshot,
      1n,
    )
    expect(() => validateMaterializationLedgerEvidence({
      ...evidence,
      seal: withCandidate,
      outcome: MaterializationLedgerEvidenceOutcome.Resumable,
    })).toThrow('cannot retain checkpoint candidates')
  })
})

async function sealEntries(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  entriesInput: readonly MaterializationLedgerEntryV1[],
  sequence: bigint,
  purpose: MaterializationLedgerSealPurpose = MaterializationLedgerSealPurpose.Terminal,
  candidateCheckpointCount = 0n,
) {
  const entries = [...entriesInput].sort(compareEntries)
  const sealId = await deriveMaterializationLedgerSealId(binding, sequence)
  const page = await createMaterializationLedgerPageSummary({
    binding,
    sealId,
    pageOrdinal: 0n,
    page: { entries },
    request: { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
  })
  return sealMaterializationLedgerPages({
    binding,
    sealSequence: sequence,
    purpose,
    candidateCheckpointCount,
    pages: [page.summary],
  })
}

async function rootAdmission(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  value: number,
) {
  return createMaterializationDirectoryAdmittedEntry(binding, {
    relativePath: path([]),
    directoryId: identity(16, value),
    generation: identity(16, value + 1),
    ownedObjectId: identity(32, value + 2),
  })
}

async function fileEntry(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  relativePath: readonly string[],
  value: number,
  exactSize: bigint,
) {
  const checkpoint = newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, value),
    fileRevision: identity(16, value + 1),
    canonicalPath: relativePath,
    exactSize,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, value + 2),
    stateGeneration: 1n,
    checkpointGeneration: 2n,
    verifiedRanges: exactSize === 0n ? [] : [{ start: 0n, end: exactSize }],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  return (await createFinalizedFileMaterializationRecords({
    binding,
    finalCheckpoint: checkpoint,
    finalOutput: new VerifiedFinalOutputFile(
      {
        backend: 'browser-fsa',
        outputSessionId: 'evidence-test',
        canonicalPath: path(relativePath),
        ownedFileIdentity: checkpoint.ownedObjectId,
      },
      {
        shareInstance: identity(16, 9),
        fileId: checkpoint.fileId,
        fileRevision: checkpoint.fileRevision,
      },
      exactSize,
    ),
  })).ledgerEntry
}

function compareEntries(left: MaterializationLedgerEntryV1, right: MaterializationLedgerEntryV1) {
  const leftCursor = materializationLedgerEntryCursor(left)
  const rightCursor = materializationLedgerEntryCursor(right)
  if (leftCursor.pathKey < rightCursor.pathKey) return -1
  if (leftCursor.pathKey > rightCursor.pathKey) return 1
  return leftCursor.entryOrder - rightCursor.entryOrder
}

async function ledgerBinding() {
  return createMaterializationLedgerBinding({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    authorityRef: identity(32, 6),
  })
}

function path(value: readonly string[]) {
  return snapshotMaterializationRootRelativePath(value)
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
