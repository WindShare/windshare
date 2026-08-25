import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createMaterializationLedgerBinding,
  materializationLedgerEntryCursor,
} from '../../src/output/materialization-ledger/codec'
import { deriveMaterializationLedgerSealId } from '../../src/output/materialization-ledger/evidence'
import {
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
} from '../../src/output/materialization-ledger/journal'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MATERIALIZATION_LEDGER_U64_MAX,
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerSealPurpose,
  type MaterializationLedgerEntryV1,
} from '../../src/output/materialization-ledger/model'
import {
  OrderedSha256MerkleAccumulator,
  createMaterializationLedgerPageSummary,
  sealMaterializationLedgerPages,
  validateMaterializationLedgerEntryPage,
  validateMaterializationLedgerSealPages,
} from '../../src/output/materialization-ledger/page'
import { outputFault, FaultScope, OutputFaultCode } from '../../src/transfer/fault'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'

describe('materialization ledger pages and seals', () => {
  it('carries one stable directory admission when its finalization crosses page 128', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding)
    const rootFinal = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
    })
    const lower = (await Promise.all(Array.from({ length: 420 }, (_, index) =>
      childAdmission(binding, index + 1))))
      .filter(entry => compareEntries(entry, root) < 0)
      .slice(0, MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT - 1)
    expect(lower).toHaveLength(MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT - 1)

    const firstEntries = [...lower, root].sort(compareEntries)
    const firstContinuation = materializationLedgerEntryCursor(root)
    const sealId = await deriveMaterializationLedgerSealId(binding, 1n)
    const first = await createMaterializationLedgerPageSummary({
      binding,
      sealId,
      pageOrdinal: 0n,
      page: { entries: firstEntries, continuation: firstContinuation },
      request: { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
    })
    const second = await createMaterializationLedgerPageSummary({
      binding,
      sealId,
      pageOrdinal: 1n,
      page: { entries: [rootFinal] },
      request: {
        limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
        after: firstContinuation,
      },
      directoryCarry: first.directoryCarry!,
    })

    expect(first.summary.entryEventCount).toBe(128n)
    expect(first.directoryCarry?.entryId).toBe(root.entryId)
    expect(first.summary.rootPathFact).toMatchObject({
      kind: 'directory',
      finalization: { kind: 'missing' },
    })
    expect(second.summary.rootPathFact).toMatchObject({
      kind: 'directory',
      finalization: { kind: MaterializationLedgerDirectoryOutcome.Finalized },
    })

    const seal = await sealMaterializationLedgerPages({
      binding,
      sealSequence: 1n,
      purpose: MaterializationLedgerSealPurpose.Terminal,
      candidateCheckpointCount: 0n,
      pages: [first.summary, second.summary],
    })
    expect({
      firstPageId: first.summary.pageId,
      firstPageDigest: first.summary.pageDigest,
      secondPageId: second.summary.pageId,
      secondPageDigest: second.summary.pageDigest,
      sealId: seal.sealId,
      sealDigest: seal.sealDigest,
      entryPageRoot: seal.entryPageRoot,
      aggregateRoot: seal.aggregateRoot,
    }).toEqual({
      firstPageId: '6CPCnP2RD2UTGPLyOl2p9q_eONLu4BE7bGRAkmwyOSE',
      firstPageDigest: 'eXA6d9RJztL2Ecjj3wvybvpXtA4AeH3CmpkyD7avn0g',
      secondPageId: 'MZZ3TRo1Pm8N0m5L27Jcjz2mO6ziufMr2KwMhKN8xIY',
      secondPageDigest: 'Vpz_NxsTIMDnCPHIut7v5ICNqt0-RvCOOUY8ndLahsQ',
      sealId: 'KMSResBwf1bSklHfr3_P4MeAOrrXB8jDbg_9_5lw4FI',
      sealDigest: '3m15ItPBvO2uTbUKqxme-9PvhmwayduYWrfiKKp1i5M',
      entryPageRoot: 'bkZP_T-5BSglY4GKlx07ZOmOS8qXUtP6o6D0QilznUk',
      aggregateRoot: 'MV-n9UCa1vwjg3UaU60aHne2rXSEjj3q22FacAIDWKI',
    })
    expect(seal).toMatchObject({
      pageCount: 2n,
      entryEventCount: 129n,
      materializedDirectoryCount: 128n,
      finalizedDirectoryCount: 1n,
      visibleDirectoryCount: 127n,
      rootPathFact: {
        kind: 'directory',
        finalization: { kind: MaterializationLedgerDirectoryOutcome.Finalized },
      },
    })
    await expect(validateMaterializationLedgerSealPages({
      binding,
      seal,
      pages: [first.summary, second.summary],
    })).resolves.toEqual(seal)
  })

  it('rejects oversized, unordered, orphaned, and inexact cursor pages', async () => {
    const binding = await ledgerBinding()
    const entries = (await Promise.all(Array.from({ length: 130 }, (_, index) =>
      childAdmission(binding, index + 1)))).sort(compareEntries)
    const tail = materializationLedgerEntryCursor(entries[0]!)

    await expect(validateMaterializationLedgerEntryPage(
      { entries: entries.slice(0, 129) },
      { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
      binding,
    )).rejects.toThrow('fixed entry bound')
    await expect(validateMaterializationLedgerEntryPage(
      { entries: [entries[1]!, entries[0]!] },
      { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
      binding,
    )).rejects.toThrow('strict cursor order')
    await expect(validateMaterializationLedgerEntryPage(
      { entries: [entries[0]!], continuation: tail },
      { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
      binding,
    )).rejects.toThrow('short')
    await expect(validateMaterializationLedgerEntryPage(
      { entries: [entries[1]!] },
      { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT, after: tail },
      binding,
      entries[1],
    )).rejects.toThrow('carry does not match')
  })

  it('produces the same root after completion-order shuffles and changes for a mutation', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding)
    const finalized = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
    })
    const isolated = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
      fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
    })
    const child = await childAdmission(binding, 44)

    const first = await sealEntries(binding, 5n, [child, finalized, root])
    const shuffled = await sealEntries(binding, 5n, [root, child, finalized])
    const mutated = await sealEntries(binding, 5n, [root, child, isolated])

    expect(shuffled.aggregateRoot).toBe(first.aggregateRoot)
    expect(shuffled.sealDigest).toBe(first.sealDigest)
    expect(mutated.aggregateRoot).not.toBe(first.aggregateRoot)
    expect(mutated.sealId).toBe(first.sealId)
    expect(mutated.sealDigest).not.toBe(first.sealDigest)
  })

  it('keeps old seals immutable while a later snapshot appends events', async () => {
    const binding = await ledgerBinding()
    const root = await rootAdmission(binding)
    const finalized = await createMaterializationDirectoryFinalizedEntry(binding, root, {
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
    })
    const resumable = await sealEntries(binding, 1n, [root], 'resumable-snapshot')
    const terminal = await sealEntries(binding, 2n, [finalized, root])

    expect(resumable.sealId).not.toBe(terminal.sealId)
    expect(resumable.entryEventCount).toBe(1n)
    expect(resumable.rootPathFact).toMatchObject({
      kind: 'directory',
      finalization: { kind: 'missing' },
    })
    expect(terminal.entryEventCount).toBe(2n)
    expect(terminal.rootPathFact).toMatchObject({
      kind: 'directory',
      finalization: { kind: 'finalized' },
    })
  })

  it('rejects u64 byte overflow and keeps Merkle working memory logarithmic', async () => {
    const accumulator = new OrderedSha256MerkleAccumulator()
    for (let index = 0; index < 1_024; index += 1) {
      await accumulator.append(identity(32, (index % 255) + 1))
    }
    expect(accumulator.leafCount).toBe(1_024n)
    expect(accumulator.peakCount).toBeLessThanOrEqual(11)
    await expect(accumulator.finishRoot()).resolves.toMatch(/^[A-Za-z0-9_-]{43}$/u)

    const binding = await ledgerBinding()
    const first = await fileEntry(binding, ['one.bin'], 70, MATERIALIZATION_LEDGER_U64_MAX)
    const second = await fileEntry(binding, ['two.bin'], 80, 1n)
    const sealId = await deriveMaterializationLedgerSealId(binding, 9n)
    await expect(createMaterializationLedgerPageSummary({
      binding,
      sealId,
      pageOrdinal: 0n,
      page: { entries: [first, second].sort(compareEntries) },
      request: { limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT },
    })).rejects.toThrow('file bytes exceeds u64')
  })
})

async function sealEntries(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  sequence: bigint,
  entriesInput: readonly MaterializationLedgerEntryV1[],
  purpose: 'terminal' | 'resumable-snapshot' = 'terminal',
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
    candidateCheckpointCount: 0n,
    pages: [page.summary],
  })
}

async function rootAdmission(binding: Awaited<ReturnType<typeof ledgerBinding>>) {
  return createMaterializationDirectoryAdmittedEntry(binding, {
    relativePath: path([]),
    directoryId: identity(16, 20),
    generation: identity(16, 21),
    ownedObjectId: identity(32, 22),
  })
}

async function childAdmission(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  value: number,
) {
  return createMaterializationDirectoryAdmittedEntry(binding, {
    relativePath: path([`d-${value}`]),
    directoryId: identity(16, (value % 250) + 1),
    generation: identity(16, ((value + 1) % 250) + 1),
    ownedObjectId: identity(32, ((value + 2) % 250) + 1),
    parent: {
      relativePath: path([]),
      directoryId: identity(16, 20),
      generation: identity(16, 21),
      ownedObjectId: identity(32, 22),
    },
  })
}

async function fileEntry(
  binding: Awaited<ReturnType<typeof ledgerBinding>>,
  relativePath: readonly string[],
  value: number,
  exactSize: bigint,
) {
  const {
    FILE_CHECKPOINT_COMMIT_VERIFIED,
    FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    FILE_CHECKPOINT_PHASE_ACTIVE,
    newFileCheckpointV2,
  } = await import('../../src/output/persistence/checkpoint')
  const { createFinalizedFileMaterializationRecords } = await import(
    '../../src/output/materialization-ledger/journal'
  )
  const { VerifiedFinalOutputFile } = await import('../../src/transfer/output-session')
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
        outputSessionId: 'page-test',
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
