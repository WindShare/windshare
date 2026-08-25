import type { DirectTreeRootExpectation } from '../../transfer/job/coordinate/direct-tree'
import type { MaterializationSummary } from '../../transfer/output-session'
import { FILE_CHECKPOINT_ID_BYTES } from '../persistence/checkpoint'
import { CanonicalRecordReader } from '../workspace/canonical-reader'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
import {
  canonicalRootFact,
  compareMaterializationLedgerEntryCursors,
  decodeRootFact,
  nonZeroU64,
  requireCanonicalBytes,
  requireExpectedBindingDigest,
  requireRecordProjection,
  snapshotMaterializationLedgerEntryCursor,
  snapshotRootFact,
  validateBinding,
} from './codec'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  MATERIALIZATION_LEDGER_U64_MAX,
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerEntryKind,
  MaterializationLedgerSealPurpose,
  hasOwn,
  requireDiscriminant,
  requireExactKeys,
  requireRecord,
  u64,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerCounts,
  type MaterializationLedgerEntryCursor,
  type MaterializationLedgerEntryOrder,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerRootFact,
  type MaterializationLedgerSealV1,
} from './model'

const LEDGER_PAGE_ID_DOMAIN = 'windshare/materialization-ledger/v1/page-id'
const LEDGER_PAGE_DOMAIN = 'windshare/materialization-ledger/v1/page'
const LEDGER_SEAL_ID_DOMAIN = 'windshare/materialization-ledger/v1/seal-id'
const LEDGER_SEAL_DOMAIN = 'windshare/materialization-ledger/v1/seal'
const SEAL_RESUMABLE_DISCRIMINANT = 1
const SEAL_TERMINAL_DISCRIMINANT = 2
const ABSENT_DISCRIMINANT = 0
const PRESENT_DISCRIMINANT = 1

export interface CreateMaterializationLedgerPageSummaryInput extends MaterializationLedgerCounts {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealId: string
  readonly pageOrdinal: bigint
  readonly firstEntry: MaterializationLedgerEntryCursor
  readonly lastEntry: MaterializationLedgerEntryCursor
  readonly canonicalEntryBytes: bigint
  readonly orderedEntriesDigest: string
  readonly rootPathFact: MaterializationLedgerRootFact
}

export interface CreateMaterializationLedgerSealInput extends MaterializationLedgerCounts {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealSequence: bigint
  readonly purpose: MaterializationLedgerSealPurpose
  readonly pageCount: bigint
  readonly candidateCheckpointCount: bigint
  readonly lastEntry?: MaterializationLedgerEntryCursor
  readonly entryPageRoot: string
  readonly aggregateRoot: string
  readonly rootPathFact: MaterializationLedgerRootFact
}
export const MaterializationLedgerEvidenceOutcome = {
  Published: 'published',
  Partial: 'partial',
  Resumable: 'resumable',
} as const

export type MaterializationLedgerEvidenceOutcome =
  typeof MaterializationLedgerEvidenceOutcome[keyof typeof MaterializationLedgerEvidenceOutcome]

export interface ValidatedMaterializationLedgerEvidence {
  readonly sealId: string
  readonly sealDigest: string
  readonly aggregateRoot: string
  readonly entryEventCount: bigint
  readonly pageCount: bigint
  readonly fileCount: bigint
  readonly visibleDirectoryCount: bigint
  readonly materializedDirectoryCount: bigint
  readonly finalizedDirectoryCount: bigint
  readonly isolatedDirectoryCount: bigint
  readonly completedBytes: bigint
}

export function validateMaterializationLedgerEvidence(input: Readonly<{
  readonly seal: MaterializationLedgerSealV1
  readonly worker: MaterializationSummary
  readonly rootExpectation: DirectTreeRootExpectation
  readonly outcome: MaterializationLedgerEvidenceOutcome
}>): ValidatedMaterializationLedgerEvidence {
  requireMatchingMaterializationSummary(input.seal, input.worker)
  if (input.seal.candidateCheckpointCount !== 0n) {
    throw new TypeError('materialization ledger seal cannot retain checkpoint candidates')
  }
  const requireComplete = input.outcome === MaterializationLedgerEvidenceOutcome.Published
  validateMaterializationLedgerRootExpectation(
    input.seal,
    input.rootExpectation,
    requireComplete,
  )
  switch (input.outcome) {
    case MaterializationLedgerEvidenceOutcome.Published:
      if (input.seal.purpose !== MaterializationLedgerSealPurpose.Terminal) {
        throw new TypeError('published materialization requires a terminal ledger seal')
      }
      requirePublishedDirectoryFinalization(input.seal)
      break
    case MaterializationLedgerEvidenceOutcome.Partial:
      if (input.seal.purpose !== MaterializationLedgerSealPurpose.Terminal) {
        throw new TypeError('partial terminal materialization requires a terminal ledger seal')
      }
      requireTypedDirectoryFinalization(input.seal)
      break
    case MaterializationLedgerEvidenceOutcome.Resumable:
      if (input.seal.purpose !== MaterializationLedgerSealPurpose.ResumableSnapshot) {
        throw new TypeError('resumable materialization requires a resumable ledger seal')
      }
      break
  }
  return Object.freeze({
    sealId: input.seal.sealId,
    sealDigest: input.seal.sealDigest,
    aggregateRoot: input.seal.aggregateRoot,
    entryEventCount: input.seal.entryEventCount,
    pageCount: input.seal.pageCount,
    fileCount: input.seal.fileCount,
    visibleDirectoryCount: input.seal.visibleDirectoryCount,
    materializedDirectoryCount: input.seal.materializedDirectoryCount,
    finalizedDirectoryCount: input.seal.finalizedDirectoryCount,
    isolatedDirectoryCount: input.seal.isolatedDirectoryCount,
    completedBytes: input.seal.fileBytes,
  })
}

export function requireMatchingMaterializationSummary(
  seal: MaterializationLedgerSealV1,
  worker: MaterializationSummary,
): void {
  const expectedEntryCount = checkedAdd(
    seal.fileCount,
    seal.visibleDirectoryCount,
    'visible materialization entry count',
  )
  if (worker.entryCount !== expectedEntryCount ||
      worker.fileCount !== seal.fileCount ||
      worker.directoryCount !== seal.visibleDirectoryCount ||
      worker.rawBytes !== seal.fileBytes) {
    throw new TypeError('worker summary disagrees with sealed materialization evidence')
  }
}

export function validateMaterializationLedgerRootExpectation(
  seal: MaterializationLedgerSealV1,
  expectation: DirectTreeRootExpectation,
  requireComplete: boolean,
): void {
  const fact = seal.rootPathFact
  if (expectation.kind === 'none') {
    if (fact.kind === 'directory') {
      throw new TypeError('single-file materialization has unexpected directory-root evidence')
    }
    if (requireComplete && (
      fact.kind !== MaterializationLedgerEntryKind.FileFinalized ||
      seal.fileCount !== 1n ||
      seal.materializedDirectoryCount !== 0n
    )) {
      throw new TypeError('published single-file materialization lacks its [] file fact')
    }
    return
  }

  if (fact.kind === 'absent') {
    if (requireComplete || seal.materializedDirectoryCount !== 0n) {
      throw new TypeError('materialized directory root evidence is missing')
    }
    return
  }
  if (fact.kind !== 'directory') {
    throw new TypeError('directory materialization root was claimed by a file')
  }
  if (fact.directoryId !== expectation.directoryId ||
      !samePath(fact.relativePath, expectation.relativePath)) {
    throw new TypeError('materialized directory root identity or path disagrees with expectation')
  }
  if (requireComplete &&
      fact.finalization.kind !== MaterializationLedgerDirectoryOutcome.Finalized) {
    throw new TypeError('published materialization root directory is not finalized')
  }
}

export function requirePublishedDirectoryFinalization(
  seal: MaterializationLedgerSealV1,
): void {
  if (seal.finalizedDirectoryCount !== seal.materializedDirectoryCount ||
      seal.isolatedDirectoryCount !== 0n) {
    throw new TypeError('published materialization requires every directory to finalize')
  }
}

export function requireTypedDirectoryFinalization(
  seal: MaterializationLedgerSealV1,
): void {
  if (checkedAdd(
    seal.finalizedDirectoryCount,
    seal.isolatedDirectoryCount,
    'typed directory outcome count',
  ) !== seal.materializedDirectoryCount) {
    throw new TypeError('partial terminal materialization has an untyped directory outcome')
  }
}

function checkedAdd(left: bigint, right: bigint, label: string): bigint {
  if (typeof left !== 'bigint' || typeof right !== 'bigint' ||
      left < 0n || right < 0n || left > MATERIALIZATION_LEDGER_U64_MAX - right) {
    throw new RangeError(`${label} exceeds u64`)
  }
  return left + right
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length &&
    left.every((segment, index) => segment === right[index])
}

export async function deriveMaterializationLedgerSealId(
  bindingInput: MaterializationLedgerBindingV1,
  sealSequence: bigint,
): Promise<string> {
  const binding = await validateBinding(bindingInput)
  const sequence = nonZeroU64(sealSequence, 'seal sequence')
  return canonicalDigest(canonicalRecord(LEDGER_SEAL_ID_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalU64(sequence)),
  ]))
}

export async function createMaterializationLedgerPageSummaryRecord(
  input: CreateMaterializationLedgerPageSummaryInput,
): Promise<MaterializationLedgerPageSummaryV1> {
  const binding = await validateBinding(input.binding)
  const sealId = snapshotIdentity(input.sealId, FILE_CHECKPOINT_ID_BYTES, 'seal ID')
  const pageOrdinal = u64(input.pageOrdinal, 'page ordinal')
  const firstEntry = snapshotMaterializationLedgerEntryCursor(input.firstEntry)
  const lastEntry = snapshotMaterializationLedgerEntryCursor(input.lastEntry)
  if (compareMaterializationLedgerEntryCursors(firstEntry, lastEntry) > 0) {
    throw new TypeError('materialization ledger page cursor range is reversed')
  }
  const counts = snapshotCounts(input)
  if (counts.entryEventCount === 0n ||
      counts.entryEventCount > BigInt(MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT)) {
    throw new TypeError('materialization ledger page entry count is outside its fixed bound')
  }
  const canonicalEntryBytes = u64(input.canonicalEntryBytes, 'canonical entry bytes')
  const orderedEntriesDigest = snapshotIdentity(
    input.orderedEntriesDigest,
    FILE_CHECKPOINT_ID_BYTES,
    'ordered entries digest',
  )
  const rootPathFact = snapshotRootFact(input.rootPathFact)
  const pageId = await materializationLedgerPageId(binding, sealId, pageOrdinal)
  const canonicalBytes = canonicalPageSummaryBytes({
    binding,
    sealId,
    pageId,
    pageOrdinal,
    firstEntry,
    lastEntry,
    counts,
    canonicalEntryBytes,
    orderedEntriesDigest,
    rootPathFact,
  })
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    sealId,
    pageId,
    pageDigest: await canonicalDigest(canonicalBytes),
    pageOrdinal,
    firstEntry,
    lastEntry,
    ...counts,
    canonicalEntryBytes,
    orderedEntriesDigest,
    rootPathFact,
    canonicalBytes,
  })
}

export async function decodeMaterializationLedgerPageSummaryV1(
  input: unknown,
  bindingInput: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerPageSummaryV1> {
  const binding = await validateBinding(bindingInput)
  const record = requireRecord(input, 'materialization ledger page summary')
  requireExactKeys(record, [
    'schemaVersion',
    'operationId',
    'ledgerBindingDigest',
    'sealId',
    'pageId',
    'pageDigest',
    'pageOrdinal',
    'firstEntry',
    'lastEntry',
    ...countKeys(),
    'canonicalEntryBytes',
    'orderedEntriesDigest',
    'rootPathFact',
    'canonicalBytes',
  ], 'materialization ledger page summary')
  const reader = CanonicalRecordReader.open(
    requireCanonicalBytes(record.canonicalBytes, 'page summary canonical bytes'),
    LEDGER_PAGE_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  )
  requireExpectedBindingDigest(
    reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'ledger binding digest'),
    binding,
  )
  const sealId = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'seal ID')
  const pageId = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'page ID')
  const rebuilt = await createMaterializationLedgerPageSummaryRecord({
    binding,
    sealId,
    pageOrdinal: reader.framedU64('page ordinal'),
    firstEntry: decodeCursor(reader.frame('first entry cursor')),
    lastEntry: decodeCursor(reader.frame('last entry cursor')),
    ...decodeCounts(reader),
    canonicalEntryBytes: reader.framedU64('canonical entry bytes'),
    orderedEntriesDigest: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'ordered entries digest',
    ),
    rootPathFact: decodeRootFact(reader.frame('root path fact')),
  })
  reader.finish('materialization ledger page summary')
  if (pageId !== rebuilt.pageId) {
    throw new TypeError('materialization ledger page ID disagrees with canonical bytes')
  }
  requireRecordProjection(record, rebuilt, 'materialization ledger page summary')
  return rebuilt
}

export async function createMaterializationLedgerSealRecord(
  input: CreateMaterializationLedgerSealInput,
): Promise<MaterializationLedgerSealV1> {
  const binding = await validateBinding(input.binding)
  const sealSequence = nonZeroU64(input.sealSequence, 'seal sequence')
  const purpose = sealPurpose(input.purpose)
  const pageCount = u64(input.pageCount, 'page count')
  const candidateCheckpointCount = u64(
    input.candidateCheckpointCount,
    'candidate checkpoint count',
  )
  const counts = snapshotCounts(input)
  if (counts.finalizedDirectoryCount + counts.isolatedDirectoryCount >
      counts.materializedDirectoryCount) {
    throw new TypeError('materialization ledger seal finalizes more directories than it admits')
  }
  const lastEntry = input.lastEntry === undefined
    ? undefined
    : snapshotMaterializationLedgerEntryCursor(input.lastEntry)
  if ((pageCount === 0n) !== (lastEntry === undefined) ||
      (pageCount === 0n) !== (counts.entryEventCount === 0n)) {
    throw new TypeError('materialization ledger empty seal projections disagree')
  }
  const entryPageRoot = snapshotIdentity(
    input.entryPageRoot,
    FILE_CHECKPOINT_ID_BYTES,
    'entry page root',
  )
  const aggregateRoot = snapshotIdentity(
    input.aggregateRoot,
    FILE_CHECKPOINT_ID_BYTES,
    'aggregate root',
  )
  const rootPathFact = snapshotRootFact(input.rootPathFact)
  const sealId = await deriveMaterializationLedgerSealId(binding, sealSequence)
  const canonicalBytes = canonicalSealBytes({
    binding,
    sealId,
    sealSequence,
    purpose,
    pageCount,
    candidateCheckpointCount,
    lastEntry,
    counts,
    entryPageRoot,
    aggregateRoot,
    rootPathFact,
  })
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    sealId,
    sealDigest: await canonicalDigest(canonicalBytes),
    sealSequence,
    purpose,
    pageCount,
    candidateCheckpointCount,
    ...(lastEntry === undefined ? {} : { lastEntry }),
    entryPageRoot,
    aggregateRoot,
    ...counts,
    rootPathFact,
    canonicalBytes,
  })
}

export async function decodeMaterializationLedgerSealV1(
  input: unknown,
  bindingInput: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerSealV1> {
  const binding = await validateBinding(bindingInput)
  const record = requireRecord(input, 'materialization ledger seal')
  requireExactKeys(record, [
    'schemaVersion',
    'operationId',
    'ledgerBindingDigest',
    'sealId',
    'sealDigest',
    'sealSequence',
    'purpose',
    'pageCount',
    'candidateCheckpointCount',
    ...(hasOwn(record, 'lastEntry') ? ['lastEntry'] : []),
    'entryPageRoot',
    'aggregateRoot',
    ...countKeys(),
    'rootPathFact',
    'canonicalBytes',
  ], 'materialization ledger seal')
  const reader = CanonicalRecordReader.open(
    requireCanonicalBytes(record.canonicalBytes, 'ledger seal canonical bytes'),
    LEDGER_SEAL_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  )
  requireExpectedBindingDigest(
    reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'ledger binding digest'),
    binding,
  )
  const sealId = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'seal ID')
  const sealSequence = reader.framedU64('seal sequence')
  const purpose = decodeSealPurpose(reader.byte('seal purpose'))
  const pageCount = reader.framedU64('page count')
  const candidateCheckpointCount = reader.framedU64('candidate checkpoint count')
  const lastEntry = decodeOptionalCursor(reader.frame('last entry cursor'))
  const entryPageRoot = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'entry page root')
  const aggregateRoot = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'aggregate root')
  const counts = decodeCounts(reader)
  const rootPathFact = decodeRootFact(reader.frame('root path fact'))
  reader.finish('materialization ledger seal')
  const rebuilt = await createMaterializationLedgerSealRecord({
    binding,
    sealSequence,
    purpose,
    pageCount,
    candidateCheckpointCount,
    ...(lastEntry === undefined ? {} : { lastEntry }),
    entryPageRoot,
    aggregateRoot,
    ...counts,
    rootPathFact,
  })
  if (sealId !== rebuilt.sealId) {
    throw new TypeError('materialization ledger seal ID disagrees with canonical bytes')
  }
  requireRecordProjection(record, rebuilt, 'materialization ledger seal')
  return rebuilt
}

async function materializationLedgerPageId(
  binding: MaterializationLedgerBindingV1,
  sealId: string,
  pageOrdinal: bigint,
): Promise<string> {
  return canonicalDigest(canonicalRecord(LEDGER_PAGE_ID_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(sealId, FILE_CHECKPOINT_ID_BYTES, 'seal ID')),
    canonicalFrame(canonicalU64(pageOrdinal)),
  ]))
}

function canonicalPageSummaryBytes(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealId: string
  readonly pageId: string
  readonly pageOrdinal: bigint
  readonly firstEntry: MaterializationLedgerEntryCursor
  readonly lastEntry: MaterializationLedgerEntryCursor
  readonly counts: MaterializationLedgerCounts
  readonly canonicalEntryBytes: bigint
  readonly orderedEntriesDigest: string
  readonly rootPathFact: MaterializationLedgerRootFact
}): CanonicalBytes {
  return canonicalRecord(LEDGER_PAGE_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      input.binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(input.sealId, FILE_CHECKPOINT_ID_BYTES, 'seal ID')),
    canonicalFrame(canonicalIdentity(input.pageId, FILE_CHECKPOINT_ID_BYTES, 'page ID')),
    canonicalFrame(canonicalU64(input.pageOrdinal)),
    canonicalFrame(canonicalCursor(input.firstEntry)),
    canonicalFrame(canonicalCursor(input.lastEntry)),
    ...canonicalCounts(input.counts),
    canonicalFrame(canonicalU64(input.canonicalEntryBytes)),
    canonicalFrame(canonicalIdentity(
      input.orderedEntriesDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ordered entries digest',
    )),
    canonicalFrame(canonicalRootFact(input.rootPathFact)),
  ])
}

function canonicalSealBytes(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealId: string
  readonly sealSequence: bigint
  readonly purpose: MaterializationLedgerSealPurpose
  readonly pageCount: bigint
  readonly candidateCheckpointCount: bigint
  readonly lastEntry: MaterializationLedgerEntryCursor | undefined
  readonly counts: MaterializationLedgerCounts
  readonly entryPageRoot: string
  readonly aggregateRoot: string
  readonly rootPathFact: MaterializationLedgerRootFact
}): CanonicalBytes {
  return canonicalRecord(LEDGER_SEAL_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      input.binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(input.sealId, FILE_CHECKPOINT_ID_BYTES, 'seal ID')),
    canonicalFrame(canonicalU64(input.sealSequence)),
    canonicalU8(sealPurposeDiscriminant(input.purpose)),
    canonicalFrame(canonicalU64(input.pageCount)),
    canonicalFrame(canonicalU64(input.candidateCheckpointCount)),
    canonicalFrame(canonicalOptionalCursor(input.lastEntry)),
    canonicalFrame(canonicalIdentity(
      input.entryPageRoot,
      FILE_CHECKPOINT_ID_BYTES,
      'entry page root',
    )),
    canonicalFrame(canonicalIdentity(
      input.aggregateRoot,
      FILE_CHECKPOINT_ID_BYTES,
      'aggregate root',
    )),
    ...canonicalCounts(input.counts),
    canonicalFrame(canonicalRootFact(input.rootPathFact)),
  ])
}

function canonicalCounts(countsInput: MaterializationLedgerCounts): readonly CanonicalBytes[] {
  const counts = snapshotCounts(countsInput)
  return [
    canonicalFrame(canonicalU64(counts.entryEventCount)),
    canonicalFrame(canonicalU64(counts.fileCount)),
    canonicalFrame(canonicalU64(counts.fileBytes)),
    canonicalFrame(canonicalU64(counts.materializedDirectoryCount)),
    canonicalFrame(canonicalU64(counts.visibleDirectoryCount)),
    canonicalFrame(canonicalU64(counts.finalizedDirectoryCount)),
    canonicalFrame(canonicalU64(counts.isolatedDirectoryCount)),
  ]
}

function decodeCounts(reader: CanonicalRecordReader): MaterializationLedgerCounts {
  return snapshotCounts({
    entryEventCount: reader.framedU64('entry event count'),
    fileCount: reader.framedU64('file count'),
    fileBytes: reader.framedU64('file bytes'),
    materializedDirectoryCount: reader.framedU64('materialized directory count'),
    visibleDirectoryCount: reader.framedU64('visible directory count'),
    finalizedDirectoryCount: reader.framedU64('finalized directory count'),
    isolatedDirectoryCount: reader.framedU64('isolated directory count'),
  })
}

function snapshotCounts(input: MaterializationLedgerCounts): MaterializationLedgerCounts {
  const counts = Object.freeze({
    entryEventCount: u64(input.entryEventCount, 'entry event count'),
    fileCount: u64(input.fileCount, 'file count'),
    fileBytes: u64(input.fileBytes, 'file bytes'),
    materializedDirectoryCount: u64(
      input.materializedDirectoryCount,
      'materialized directory count',
    ),
    visibleDirectoryCount: u64(input.visibleDirectoryCount, 'visible directory count'),
    finalizedDirectoryCount: u64(input.finalizedDirectoryCount, 'finalized directory count'),
    isolatedDirectoryCount: u64(input.isolatedDirectoryCount, 'isolated directory count'),
  })
  const projectedEvents = counts.fileCount + counts.materializedDirectoryCount +
    counts.finalizedDirectoryCount + counts.isolatedDirectoryCount
  if (counts.visibleDirectoryCount > counts.materializedDirectoryCount ||
      projectedEvents !== counts.entryEventCount) {
    throw new TypeError('materialization ledger semantic counts are inconsistent')
  }
  return counts
}

function countKeys(): readonly string[] {
  return [
    'entryEventCount',
    'fileCount',
    'fileBytes',
    'materializedDirectoryCount',
    'visibleDirectoryCount',
    'finalizedDirectoryCount',
    'isolatedDirectoryCount',
  ]
}

function canonicalCursor(cursorInput: MaterializationLedgerEntryCursor): CanonicalBytes {
  const cursor = snapshotMaterializationLedgerEntryCursor(cursorInput)
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(cursor.pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key')),
    canonicalU8(cursor.entryOrder),
    canonicalFrame(canonicalIdentity(cursor.entryId, FILE_CHECKPOINT_ID_BYTES, 'entry ID')),
  ])
}

function decodeCursor(bytes: Uint8Array): MaterializationLedgerEntryCursor {
  const reader = CanonicalRecordReader.value(bytes)
  const result = snapshotMaterializationLedgerEntryCursor({
    pathKey: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'path key'),
    entryOrder: reader.byte('entry order') as MaterializationLedgerEntryOrder,
    entryId: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'entry ID'),
  })
  reader.finish('ledger entry cursor')
  return result
}

function canonicalOptionalCursor(
  cursor: MaterializationLedgerEntryCursor | undefined,
): CanonicalBytes {
  return cursor === undefined
    ? canonicalU8(ABSENT_DISCRIMINANT)
    : concatCanonicalBytes([
        canonicalU8(PRESENT_DISCRIMINANT),
        canonicalFrame(canonicalCursor(cursor)),
      ])
}

function decodeOptionalCursor(bytes: Uint8Array): MaterializationLedgerEntryCursor | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('entry cursor presence')
  if (discriminant === ABSENT_DISCRIMINANT) {
    reader.finish('absent ledger entry cursor')
    return undefined
  }
  requireDiscriminant(discriminant, PRESENT_DISCRIMINANT, 'entry cursor presence')
  const cursor = decodeCursor(reader.frame('ledger entry cursor'))
  reader.finish('present ledger entry cursor')
  return cursor
}

function sealPurpose(input: MaterializationLedgerSealPurpose): MaterializationLedgerSealPurpose {
  if (input !== MaterializationLedgerSealPurpose.ResumableSnapshot &&
      input !== MaterializationLedgerSealPurpose.Terminal) {
    throw new TypeError('materialization ledger seal purpose is invalid')
  }
  return input
}

function sealPurposeDiscriminant(input: MaterializationLedgerSealPurpose): number {
  return sealPurpose(input) === MaterializationLedgerSealPurpose.ResumableSnapshot
    ? SEAL_RESUMABLE_DISCRIMINANT
    : SEAL_TERMINAL_DISCRIMINANT
}

function decodeSealPurpose(discriminant: number): MaterializationLedgerSealPurpose {
  if (discriminant === SEAL_RESUMABLE_DISCRIMINANT) {
    return MaterializationLedgerSealPurpose.ResumableSnapshot
  }
  requireDiscriminant(discriminant, SEAL_TERMINAL_DISCRIMINANT, 'seal purpose')
  return MaterializationLedgerSealPurpose.Terminal
}
