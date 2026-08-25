import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  fileCheckpointIsComplete,
  validateFileCheckpoint,
  validateFileCheckpointTransition,
  type FileCheckpointV2,
} from '../../persistence/checkpoint'
import { checkpointMatchesNamespace, type PersistentHandleRecord } from '../../persistence/journal'
import {
  decodeMaterializationLedgerPageSummaryV1,
  decodeMaterializationLedgerSealV1,
} from '../../materialization-ledger/evidence'
import {
  decodeMaterializationFinalFileProofV1,
  decodeMaterializationLedgerEntryV1,
  type FinalFileMaterializationCommit,
  type FinalFileMaterializationCommitReceipt,
  type MaterializationLedgerPageSummaryScan,
  type MaterializationLedgerRetirementResult,
} from '../../materialization-ledger/journal'
import {
  MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  MaterializationLedgerEntryKind,
  type MaterializationDirectoryAdmittedEntryV1,
  type MaterializationDirectoryFinalizedEntryV1,
  type MaterializationFinalFileProofV1,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryPage,
  type MaterializationLedgerPageRequest,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerSealV1,
} from '../../materialization-ledger/model'
import {
  compareMaterializationLedgerEntryCursors,
  materializationLedgerEntryCursor,
  validateBinding,
} from '../../materialization-ledger/codec'
import { sameDurableCheckpointNamespace, durableCheckpointNamespaceIdentity } from '../../persistence/namespace'
import { equalCanonicalBytes } from '../../workspace/canonical'
import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX,
  INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX,
  INDEXEDDB_BY_OPERATION_SEAL_PAGE_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_FILE_FINAL_PROOF_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE,
  requestResult,
} from '../indexeddb-database'
import {
  abortConcurrency,
  abortIntegrity,
  readStoredCheckpoint,
  storedCheckpoint,
  validateCheckpointHandle,
} from './repository-transactions'

export const IndexedDbSemanticWriteStage = {
  CreatedHandle: 'created-handle',
  CreatedCheckpoint: 'created-checkpoint',
  CreatedCandidateDelete: 'created-candidate-delete',
  DurableCheckpoint: 'durable-checkpoint',
  FinalCheckpoint: 'final-checkpoint',
  FinalProof: 'final-proof',
  FinalLedgerEntry: 'final-ledger-entry',
  DirectoryAdmission: 'directory-admission',
  DirectoryFinalization: 'directory-finalization',
  LedgerPage: 'ledger-page',
  LedgerSeal: 'ledger-seal',
} as const

export type IndexedDbSemanticWriteStage =
  typeof IndexedDbSemanticWriteStage[keyof typeof IndexedDbSemanticWriteStage]

export interface IndexedDbSemanticTransactionFaults {
  afterQueuedWrite?(stage: IndexedDbSemanticWriteStage): void
}

export interface PreparedFinalFileCommit {
  readonly binding: MaterializationLedgerBindingV1
  readonly expectedCommittedCheckpoint: FileCheckpointV2
  readonly finalCheckpoint: FileCheckpointV2
  readonly finalProof: MaterializationFinalFileProofV1
  readonly ledgerEntry: FinalFileMaterializationCommit['records']['ledgerEntry']
  readonly expectedPersistedOwnedFileIdentity: string
}

export async function prepareFinalFileCommit(
  input: FinalFileMaterializationCommit,
): Promise<PreparedFinalFileCommit> {
  const binding = await validateBinding(input.binding)
  validateFileCheckpoint(input.expectedCommittedCheckpoint)
  validateFileCheckpoint(input.records.finalCheckpoint)
  validateFinalCheckpointNamespace(binding, input.expectedCommittedCheckpoint)
  validateFinalCheckpointNamespace(binding, input.records.finalCheckpoint)
  if (input.expectedCommittedCheckpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      input.records.finalCheckpoint.commitState !== FILE_CHECKPOINT_COMMIT_VERIFIED ||
      !fileCheckpointIsComplete(input.records.finalCheckpoint)) {
    throw new TypeError('final file commit requires a verified complete checkpoint transition')
  }
  validateFileCheckpointTransition(
    input.expectedCommittedCheckpoint,
    input.records.finalCheckpoint,
  )
  const finalProof = await decodeMaterializationFinalFileProofV1(input.records.finalProof, binding)
  const ledgerEntry = await decodeMaterializationLedgerEntryV1(input.records.ledgerEntry, binding)
  if (ledgerEntry.kind !== MaterializationLedgerEntryKind.FileFinalized ||
      input.records.finalCheckpoint.checksum !== finalProof.checkpoint.recordDigest ||
      input.records.finalCheckpoint.checksum !== ledgerEntry.checkpoint.recordDigest ||
      finalProof.proofId !== ledgerEntry.finalProofId ||
      finalProof.proofDigest !== ledgerEntry.finalProofDigest ||
      input.records.finalCheckpoint.recordId !== finalProof.recordId ||
      input.records.finalCheckpoint.recordId !== ledgerEntry.checkpoint.recordId ||
      input.expectedPersistedOwnedFileIdentity !== input.records.finalCheckpoint.ownedObjectId) {
    throw new TypeError('final proof, checkpoint, handle identity, and ledger entry disagree')
  }
  return Object.freeze({
    binding,
    expectedCommittedCheckpoint: input.expectedCommittedCheckpoint,
    finalCheckpoint: input.records.finalCheckpoint,
    finalProof,
    ledgerEntry,
    expectedPersistedOwnedFileIdentity: input.expectedPersistedOwnedFileIdentity,
  })
}

export async function commitFinalFileTransaction(
  transaction: IDBTransaction,
  input: PreparedFinalFileCommit,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<FinalFileMaterializationCommitReceipt> {
  const candidates = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
  const committed = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE)
  const handles = transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE)
  const proofs = transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE)
  const entries = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE)

  // All classification reads are queued together because an empty IndexedDB
  // request queue is allowed to auto-commit before semantic validation finishes.
  const [candidateValue, committedValue, handleValues, proofValue, entryValue] = await Promise.all([
    requestResult<unknown>(candidates.get(input.finalCheckpoint.recordId)),
    requestResult<unknown>(committed.get(input.finalCheckpoint.recordId)),
    requestResult<unknown[]>(handles.index(INDEXEDDB_BY_OPERATION_OWNED_OBJECT_INDEX).getAll(
      IDBKeyRange.only([
        input.binding.operationId,
        input.expectedPersistedOwnedFileIdentity,
      ]),
      2,
    )),
    requestResult<unknown>(proofs.get(input.finalProof.proofId)),
    requestResult<unknown>(entries.index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX).get(
      [input.binding.operationId, input.ledgerEntry.pathKey, MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER],
    )),
  ])
  if (candidateValue !== undefined) {
    abortConcurrency(transaction, 'final file commit found an unresolved checkpoint candidate')
  }
  const handle = exactPersistedHandle(transaction, handleValues, input)
  if (handle.ownedObjectId !== input.expectedPersistedOwnedFileIdentity) {
    abortIntegrity(transaction, 'final file handle identity projection changed')
  }
  if (committedValue === undefined) {
    abortConcurrency(transaction, 'final file commit has no durable predecessor')
  }
  const current = readStoredCheckpoint(committedValue)
  if (!checkpointMatchesNamespace(current, durableCheckpointNamespaceIdentity(input.finalCheckpoint))) {
    abortIntegrity(transaction, 'final file predecessor escaped its checkpoint namespace')
  }
  const proofExact = proofValue !== undefined &&
    exactStoredCanonical(proofValue, input.finalProof, 'proofDigest')
  const entryExact = entryValue !== undefined &&
    exactStoredCanonical(entryValue, input.ledgerEntry, 'entryDigest')

  if (current.checksum === input.finalCheckpoint.checksum) {
    if (!proofExact || !entryExact) {
      abortIntegrity(transaction, 'final file transaction is partially durable')
    }
    return receipt(input, 'idempotent')
  }
  if (current.checksum !== input.expectedCommittedCheckpoint.checksum) {
    abortConcurrency(transaction, 'final file durable predecessor changed')
  }
  if (proofValue !== undefined || entryValue !== undefined) {
    abortConcurrency(transaction, 'final proof or materialization path is already claimed')
  }

  queueWrite(
    committed.put(storedCheckpoint(input.finalCheckpoint)),
    IndexedDbSemanticWriteStage.FinalCheckpoint,
    faults,
  )
  queueWrite(proofs.add(input.finalProof), IndexedDbSemanticWriteStage.FinalProof, faults)
  queueWrite(entries.add(input.ledgerEntry), IndexedDbSemanticWriteStage.FinalLedgerEntry, faults)
  return receipt(input, 'insert')
}

export async function appendDirectoryAdmissionTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  entry: MaterializationDirectoryAdmittedEntryV1,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<'insert' | 'idempotent'> {
  const decoded = entry
  if (decoded.kind !== MaterializationLedgerEntryKind.DirectoryAdmitted) {
    abortIntegrity(transaction, 'directory admission transaction received another entry kind')
  }
  return appendPathEntry(transaction, binding, decoded, faults)
}

export async function appendDirectoryFinalizationTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  entry: MaterializationDirectoryFinalizedEntryV1,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<'insert' | 'idempotent'> {
  const decoded = entry
  if (decoded.kind !== MaterializationLedgerEntryKind.DirectoryFinalized) {
    abortIntegrity(transaction, 'directory finalization transaction received another entry kind')
  }
  const store = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE)
  const [admissionValue, finalizationValue] = await Promise.all([
    requestResult<unknown>(store.index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX).get([
      binding.operationId,
      decoded.pathKey,
      MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    ])),
    requestResult<unknown>(store.index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX).get([
      binding.operationId,
      decoded.pathKey,
      MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
    ])),
  ])
  if (admissionValue === undefined) {
    abortConcurrency(transaction, 'directory finalization has no admitted path claim')
  }
  const admission = storedRecord(admissionValue)
  if (admission.kind !== MaterializationLedgerEntryKind.DirectoryAdmitted ||
      admission.operationId !== binding.operationId || admission.pathKey !== decoded.pathKey ||
      admission.entryId !== decoded.admissionEntryId ||
      admission.entryDigest !== decoded.admissionEntryDigest ||
      !(admission.canonicalBytes instanceof Uint8Array)) {
    abortIntegrity(transaction, 'directory finalization disagrees with its admission')
  }
  if (finalizationValue !== undefined) {
    if (exactStoredCanonical(finalizationValue, decoded, 'entryDigest')) return 'idempotent'
    abortConcurrency(transaction, 'directory path already has another finalization')
  }
  queueWrite(store.add(decoded), IndexedDbSemanticWriteStage.DirectoryFinalization, faults)
  return 'insert'
}

export async function scanMaterializationLedgerEntryPage(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  request: MaterializationLedgerPageRequest,
): Promise<MaterializationLedgerEntryPage> {
  if (request.limit !== MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT) {
    throw new TypeError('materialization ledger scan requires the fixed page limit')
  }
  const index = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE)
    .index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX)
  const lower = request.after === undefined
    ? [binding.operationId]
    : [binding.operationId, request.after.pathKey, request.after.entryOrder]
  const range = IDBKeyRange.bound(lower, [binding.operationId, []], request.after !== undefined)
  const values = await cursorValues(index, range, MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT)
  const entries = await Promise.all(values.map(value =>
    decodeMaterializationLedgerEntryV1(value, binding)))
  if (request.after !== undefined && entries[0] !== undefined &&
      compareMaterializationLedgerEntryCursors(
        materializationLedgerEntryCursor(entries[0]),
        request.after,
      ) <= 0) {
    throw new TypeError('materialization ledger cursor did not advance')
  }
  const continuation = entries.length === MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT
    ? materializationLedgerEntryCursor(entries.at(-1)!)
    : undefined
  return Object.freeze({
    entries: Object.freeze(entries),
    ...(continuation === undefined ? {} : { continuation }),
  })
}

export async function persistMaterializationLedgerPageTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  page: MaterializationLedgerPageSummaryV1,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<'insert' | 'idempotent'> {
  const decoded = page
  const store = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE)
  const existing = await requestResult<unknown>(store.get(decoded.pageId))
  if (existing !== undefined) {
    const current = await readStoredPage(existing, binding)
    if (sameCanonicalRecord(current, decoded, 'pageDigest')) return 'idempotent'
    abortIntegrity(transaction, 'immutable materialization ledger page changed')
  }
  queueWrite(store.add(storedPage(decoded)), IndexedDbSemanticWriteStage.LedgerPage, faults)
  return 'insert'
}

export async function scanMaterializationLedgerPageSummaries(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  sealId: string,
  afterPageOrdinal?: bigint,
): Promise<MaterializationLedgerPageSummaryScan> {
  const index = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE)
    .index(INDEXEDDB_BY_OPERATION_SEAL_PAGE_INDEX)
  const lower = afterPageOrdinal === undefined
    ? [binding.operationId, sealId]
    : [binding.operationId, sealId, ordinalKey(afterPageOrdinal)]
  const range = IDBKeyRange.bound(
    lower,
    [binding.operationId, sealId, []],
    afterPageOrdinal !== undefined,
  )
  const values = await cursorValues(index, range, MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT)
  const pages = await Promise.all(values.map(value => readStoredPage(value, binding)))
  const continuationPageOrdinal = pages.length === MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT
    ? pages.at(-1)?.pageOrdinal
    : undefined
  return Object.freeze({
    pages: Object.freeze(pages),
    ...(continuationPageOrdinal === undefined ? {} : { continuationPageOrdinal }),
  })
}

export async function persistMaterializationLedgerSealTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  seal: MaterializationLedgerSealV1,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<'insert' | 'idempotent'> {
  const decoded = seal
  const store = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE)
  const existing = await requestResult<unknown>(store.get(decoded.sealId))
  if (existing !== undefined) {
    const current = await readStoredSeal(existing, binding)
    if (sameCanonicalRecord(current, decoded, 'sealDigest')) return 'idempotent'
    abortIntegrity(transaction, 'immutable materialization ledger seal changed')
  }
  queueWrite(store.add(storedSeal(decoded)), IndexedDbSemanticWriteStage.LedgerSeal, faults)
  return 'insert'
}

export async function readMaterializationFinalProofTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  proofId: string,
): Promise<MaterializationFinalFileProofV1 | undefined> {
  const value = await requestResult<unknown>(
    transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE).get(proofId),
  )
  return value === undefined ? undefined : decodeMaterializationFinalFileProofV1(value, binding)
}

export async function retireMaterializationLedgerTransaction(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  limit: number,
): Promise<MaterializationLedgerRetirementResult> {
  const storeNames = [
    INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
    INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
    INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
    INDEXEDDB_FILE_FINAL_PROOF_STORE,
    INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
    INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
    INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE,
  ] as const
  const keyPages = await Promise.all(storeNames.map(name => requestResult<IDBValidKey[]>(
    transaction.objectStore(name).index(INDEXEDDB_BY_OPERATION_INDEX).getAllKeys(
      IDBKeyRange.only(binding.operationId),
      limit,
    ),
  )))
  const selectedIndex = keyPages.findIndex(keys => keys.length > 0)
  if (selectedIndex < 0) return Object.freeze({ deletedRows: 0, state: 'complete' })
  const store = transaction.objectStore(storeNames[selectedIndex]!)
  const keys = keyPages[selectedIndex]!
  for (const key of keys) store.delete(key)
  return Object.freeze({ deletedRows: keys.length, state: 'more' })
}

function validateFinalCheckpointNamespace(
  binding: MaterializationLedgerBindingV1,
  checkpoint: FileCheckpointV2,
): void {
  const expected = durableCheckpointNamespaceIdentity({
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: binding.authorityRef,
  })
  if (!sameDurableCheckpointNamespace(durableCheckpointNamespaceIdentity(checkpoint), expected)) {
    throw new TypeError('final checkpoint escaped its ledger namespace')
  }
}

async function appendPathEntry(
  transaction: IDBTransaction,
  binding: MaterializationLedgerBindingV1,
  entry: MaterializationDirectoryAdmittedEntryV1,
  faults?: IndexedDbSemanticTransactionFaults,
): Promise<'insert' | 'idempotent'> {
  const store = transaction.objectStore(INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE)
  const existing = await requestResult<unknown>(
    store.index(INDEXEDDB_BY_OPERATION_PATH_ORDER_INDEX).get([
      binding.operationId,
      entry.pathKey,
      MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    ]),
  )
  if (existing !== undefined) {
    if (exactStoredCanonical(existing, entry, 'entryDigest')) return 'idempotent'
    abortConcurrency(transaction, 'materialization path is already claimed')
  }
  queueWrite(store.add(entry), IndexedDbSemanticWriteStage.DirectoryAdmission, faults)
  return 'insert'
}

function exactPersistedHandle(
  transaction: IDBTransaction,
  values: readonly unknown[],
  input: PreparedFinalFileCommit,
): PersistentHandleRecord {
  if (values.length !== 1) {
    abortConcurrency(transaction, 'final file commit requires one exact persisted handle')
  }
  const handle = validateCheckpointHandle(values[0] as PersistentHandleRecord)
  if (handle.operationId !== input.binding.operationId ||
      handle.authorityRef !== input.binding.authorityRef ||
      handle.ownedObjectId !== input.expectedPersistedOwnedFileIdentity) {
    abortIntegrity(transaction, 'persisted final file handle is foreign')
  }
  return handle
}

function exactStoredCanonical<T extends { readonly canonicalBytes: Uint8Array }>(
  value: unknown,
  expected: T,
  digestKey: keyof T,
): boolean {
  const record = storedRecord(value)
  return record[digestKey as string] === expected[digestKey] &&
    record.canonicalBytes instanceof Uint8Array &&
    equalCanonicalBytes(record.canonicalBytes, expected.canonicalBytes)
}

function sameCanonicalRecord<T extends { readonly canonicalBytes: Uint8Array }>(
  left: T,
  right: T,
  digestKey: keyof T,
): boolean {
  return left[digestKey] === right[digestKey] &&
    equalCanonicalBytes(left.canonicalBytes, right.canonicalBytes)
}

function receipt(
  input: PreparedFinalFileCommit,
  classification: 'insert' | 'idempotent',
): FinalFileMaterializationCommitReceipt {
  return Object.freeze({
    classification,
    finalCheckpoint: input.finalCheckpoint,
    finalProof: input.finalProof,
    ledgerEntry: input.ledgerEntry,
  })
}

function storedPage(page: MaterializationLedgerPageSummaryV1) {
  return Object.freeze({
    pageId: page.pageId,
    operationId: page.operationId,
    sealId: page.sealId,
    pageOrdinal: ordinalKey(page.pageOrdinal),
    record: page,
  })
}

async function readStoredPage(
  value: unknown,
  binding: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerPageSummaryV1> {
  if (!recordWithNestedValue(value, 'pageId', 'record')) {
    throw new TypeError('IndexedDB materialization page row is invalid')
  }
  const page = await decodeMaterializationLedgerPageSummaryV1(value.record, binding)
  if (value.pageId !== page.pageId || value.operationId !== page.operationId ||
      value.sealId !== page.sealId || value.pageOrdinal !== ordinalKey(page.pageOrdinal)) {
    throw new TypeError('IndexedDB materialization page projections disagree')
  }
  return page
}

function storedSeal(seal: MaterializationLedgerSealV1) {
  return Object.freeze({
    sealId: seal.sealId,
    operationId: seal.operationId,
    sealSequence: ordinalKey(seal.sealSequence),
    record: seal,
  })
}

async function readStoredSeal(
  value: unknown,
  binding: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerSealV1> {
  if (!recordWithNestedValue(value, 'sealId', 'record')) {
    throw new TypeError('IndexedDB materialization seal row is invalid')
  }
  const seal = await decodeMaterializationLedgerSealV1(value.record, binding)
  if (value.sealId !== seal.sealId || value.operationId !== seal.operationId ||
      value.sealSequence !== ordinalKey(seal.sealSequence)) {
    throw new TypeError('IndexedDB materialization seal projections disagree')
  }
  return seal
}

function recordWithNestedValue(
  value: unknown,
  idKey: string,
  nestedKey: string,
): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null &&
    typeof (value as Record<string, unknown>)[idKey] === 'string' &&
    typeof (value as Record<string, unknown>)[nestedKey] === 'object'
}

function storedRecord(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError('IndexedDB semantic row is invalid')
  }
  return value as Record<string, unknown>
}

function ordinalKey(value: bigint): number {
  const converted = Number(value)
  if (!Number.isSafeInteger(converted) || converted < 0) {
    throw new TypeError('IndexedDB ledger ordinal exceeds its exact numeric key range')
  }
  return converted
}

function queueWrite(
  _request: IDBRequest,
  stage: IndexedDbSemanticWriteStage,
  faults?: IndexedDbSemanticTransactionFaults,
): void {
  faults?.afterQueuedWrite?.(stage)
}

function cursorValues(
  index: IDBIndex,
  range: IDBKeyRange,
  limit: number,
): Promise<readonly unknown[]> {
  return new Promise((resolve, reject) => {
    const values: unknown[] = []
    const request = index.openCursor(range, 'next')
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) {
        resolve(Object.freeze(values))
        return
      }
      values.push(cursor.value)
      if (values.length === limit) resolve(Object.freeze(values))
      else cursor.continue()
    })
  })
}
