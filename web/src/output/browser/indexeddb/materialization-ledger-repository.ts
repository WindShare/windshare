import { FILE_CHECKPOINT_MATERIALIZER_FSA_TREE } from '../../persistence/checkpoint'
import type { CheckpointNamespaceBinding } from '../../persistence/journal'
import { validateBinding } from '../../materialization-ledger/codec'
import {
  decodeMaterializationLedgerPageSummaryV1,
  decodeMaterializationLedgerSealV1,
  deriveMaterializationLedgerSealId,
} from '../../materialization-ledger/evidence'
import {
  decodeMaterializationLedgerEntryV1,
  type FinalFileMaterializationCommit,
  type FinalFileMaterializationCommitReceipt,
  type MaterializationLedgerJournal,
  type MaterializationLedgerPageSummaryScan,
  type MaterializationLedgerRetirementResult,
} from '../../materialization-ledger/journal'
import {
  createMaterializationLedgerPageSummary,
  sealMaterializationLedgerPages,
  validateMaterializationLedgerSealPages,
} from '../../materialization-ledger/page'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  type MaterializationDirectoryAdmittedEntryV1,
  type MaterializationDirectoryFinalizedEntryV1,
  type MaterializationFinalFileProofV1,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryPage,
  type MaterializationLedgerPageRequest,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerSealPurpose,
  type MaterializationLedgerSealV1,
} from '../../materialization-ledger/model'
import {
  INDEXEDDB_BY_OPERATION_INDEX,
  INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
  INDEXEDDB_FILE_FINAL_PROOF_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
  INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE,
  requestResult,
  transactionCompletion,
} from '../indexeddb-database'
import {
  appendDirectoryAdmissionTransaction,
  appendDirectoryFinalizationTransaction,
  commitFinalFileTransaction,
  persistMaterializationLedgerPageTransaction,
  persistMaterializationLedgerSealTransaction,
  prepareFinalFileCommit,
  readMaterializationFinalProofTransaction,
  retireMaterializationLedgerTransaction,
  scanMaterializationLedgerEntryPage,
  scanMaterializationLedgerPageSummaries,
  type IndexedDbSemanticTransactionFaults,
} from './materialization-ledger-transactions'

export class IndexedDbMaterializationLedgerParticipant implements MaterializationLedgerJournal {
  readonly #database: IDBDatabase
  readonly #checkpointBinding: CheckpointNamespaceBinding
  readonly #faults: IndexedDbSemanticTransactionFaults | undefined
  readonly #assertOpen: () => void

  constructor(input: Readonly<{
    database: IDBDatabase
    checkpointBinding: CheckpointNamespaceBinding
    faults?: IndexedDbSemanticTransactionFaults
    assertOpen: () => void
  }>) {
    this.#database = input.database
    this.#checkpointBinding = input.checkpointBinding
    this.#faults = input.faults
    this.#assertOpen = input.assertOpen
  }

  async commitFinalFile(
    input: FinalFileMaterializationCommit,
  ): Promise<FinalFileMaterializationCommitReceipt> {
    this.#assertOpen()
    const prepared = await prepareFinalFileCommit(input)
    await this.#binding(prepared.binding)
    const transaction = this.#database.transaction([
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      INDEXEDDB_FILE_FINAL_PROOF_STORE,
      INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
    ], 'readwrite')
    try {
      const receipt = await commitFinalFileTransaction(transaction, prepared, this.#faults)
      await transactionCompletion(transaction)
      return receipt
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async appendDirectoryAdmission(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryAdmittedEntryV1,
  ): Promise<'insert' | 'idempotent'> {
    const validated = await this.#binding(binding)
    const decoded = await decodeMaterializationLedgerEntryV1(entry, validated)
    if (decoded.kind !== 'directory-admitted') throw new TypeError('expected directory admission')
    return this.#entryWrite(transaction => appendDirectoryAdmissionTransaction(
      transaction,
      validated,
      decoded,
      this.#faults,
    ))
  }

  async appendDirectoryFinalization(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryFinalizedEntryV1,
  ): Promise<'insert' | 'idempotent'> {
    const validated = await this.#binding(binding)
    const decoded = await decodeMaterializationLedgerEntryV1(entry, validated)
    if (decoded.kind !== 'directory-finalized') throw new TypeError('expected directory finalization')
    return this.#entryWrite(transaction => appendDirectoryFinalizationTransaction(
      transaction,
      validated,
      decoded,
      this.#faults,
    ))
  }

  async scanMaterializationLedgerEntries(
    binding: MaterializationLedgerBindingV1,
    request: MaterializationLedgerPageRequest,
  ): Promise<MaterializationLedgerEntryPage> {
    const validated = await this.#binding(binding)
    const transaction = this.#database.transaction(
      INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
      'readonly',
    )
    const page = await scanMaterializationLedgerEntryPage(transaction, validated, request)
    await transactionCompletion(transaction)
    return page
  }

  async countCheckpointCandidates(binding: MaterializationLedgerBindingV1): Promise<bigint> {
    const validated = await this.#binding(binding)
    const transaction = this.#database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      'readonly',
    )
    const count = await requestResult<number>(
      transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE)
        .index(INDEXEDDB_BY_OPERATION_INDEX)
        .count(IDBKeyRange.only(validated.operationId)),
    )
    await transactionCompletion(transaction)
    return BigInt(count)
  }

  async persistMaterializationLedgerPage(
    binding: MaterializationLedgerBindingV1,
    page: MaterializationLedgerPageSummaryV1,
  ): Promise<'insert' | 'idempotent'> {
    const validated = await this.#binding(binding)
    const decoded = await decodeMaterializationLedgerPageSummaryV1(page, validated)
    const transaction = this.#database.transaction(
      INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
      'readwrite',
    )
    try {
      const result = await persistMaterializationLedgerPageTransaction(
        transaction,
        validated,
        decoded,
        this.#faults,
      )
      await transactionCompletion(transaction)
      return result
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async scanMaterializationLedgerPages(
    binding: MaterializationLedgerBindingV1,
    sealId: string,
    afterPageOrdinal?: bigint,
  ): Promise<MaterializationLedgerPageSummaryScan> {
    const validated = await this.#binding(binding)
    const transaction = this.#database.transaction(
      INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
      'readonly',
    )
    const result = await scanMaterializationLedgerPageSummaries(
      transaction,
      validated,
      sealId,
      afterPageOrdinal,
    )
    await transactionCompletion(transaction)
    return result
  }

  async persistMaterializationLedgerSeal(
    binding: MaterializationLedgerBindingV1,
    seal: MaterializationLedgerSealV1,
  ): Promise<'insert' | 'idempotent'> {
    const validated = await this.#binding(binding)
    const decoded = await decodeMaterializationLedgerSealV1(seal, validated)
    const transaction = this.#database.transaction(
      INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE,
      'readwrite',
    )
    try {
      const result = await persistMaterializationLedgerSealTransaction(
        transaction,
        validated,
        decoded,
        this.#faults,
      )
      await transactionCompletion(transaction)
      return result
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async sealMaterializationLedger(input: Readonly<{
    binding: MaterializationLedgerBindingV1
    sealSequence: bigint
    purpose: MaterializationLedgerSealPurpose
  }>): Promise<MaterializationLedgerSealV1> {
    const binding = await this.#binding(input.binding)
    const candidateCheckpointCount = await this.countCheckpointCandidates(binding)
    if (candidateCheckpointCount !== 0n) {
      throw new DOMException('Materialization ledger cannot seal with checkpoint candidates', 'InvalidStateError')
    }
    const sealId = await deriveMaterializationLedgerSealId(binding, input.sealSequence)
    let request: MaterializationLedgerPageRequest = {
      limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    }
    let pageOrdinal = 0n
    let directoryCarry: MaterializationDirectoryAdmittedEntryV1 | undefined
    for (;;) {
      const page = await this.scanMaterializationLedgerEntries(binding, request)
      if (page.entries.length === 0) break
      const built = await createMaterializationLedgerPageSummary({
        binding,
        sealId,
        pageOrdinal,
        page,
        request,
        ...(directoryCarry === undefined ? {} : { directoryCarry }),
      })
      await this.persistMaterializationLedgerPage(binding, built.summary)
      pageOrdinal += 1n
      directoryCarry = built.directoryCarry
      if (built.continuation === undefined) break
      request = { after: built.continuation, limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT }
    }
    const seal = await sealMaterializationLedgerPages({
      binding,
      sealSequence: input.sealSequence,
      purpose: input.purpose,
      candidateCheckpointCount,
      pages: this.#pages(binding, sealId),
    })
    await this.persistMaterializationLedgerSeal(binding, seal)
    return validateMaterializationLedgerSealPages({
      binding,
      seal,
      pages: this.#pages(binding, sealId),
    })
  }

  async readMaterializationFinalProof(
    binding: MaterializationLedgerBindingV1,
    proofId: string,
  ): Promise<MaterializationFinalFileProofV1 | undefined> {
    const validated = await this.#binding(binding)
    const transaction = this.#database.transaction(INDEXEDDB_FILE_FINAL_PROOF_STORE, 'readonly')
    const proof = await readMaterializationFinalProofTransaction(transaction, validated, proofId)
    await transactionCompletion(transaction)
    return proof
  }

  async retireMaterializationLedgerBatch(
    binding: MaterializationLedgerBindingV1,
    limit: typeof MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  ): Promise<MaterializationLedgerRetirementResult> {
    const validated = await this.#binding(binding)
    if (limit !== MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT) {
      throw new TypeError('materialization ledger retirement requires the fixed batch limit')
    }
    const stores = [
      INDEXEDDB_FILE_CHECKPOINT_CANDIDATE_STORE,
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      INDEXEDDB_FILE_CHECKPOINT_HANDLE_STORE,
      INDEXEDDB_FILE_FINAL_PROOF_STORE,
      INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
      INDEXEDDB_MATERIALIZATION_LEDGER_PAGE_STORE,
      INDEXEDDB_MATERIALIZATION_LEDGER_SEAL_STORE,
    ]
    const transaction = this.#database.transaction(stores, 'readwrite')
    const result = await retireMaterializationLedgerTransaction(
      transaction,
      validated,
      limit,
    )
    await transactionCompletion(transaction)
    return result
  }

  async #entryWrite(
    action: (transaction: IDBTransaction) => Promise<'insert' | 'idempotent'>,
  ): Promise<'insert' | 'idempotent'> {
    const transaction = this.#database.transaction(
      INDEXEDDB_MATERIALIZATION_LEDGER_ENTRY_STORE,
      'readwrite',
    )
    try {
      const result = await action(transaction)
      await transactionCompletion(transaction)
      return result
    } catch (error) {
      abortQuietly(transaction)
      throw error
    }
  }

  async #binding(input: MaterializationLedgerBindingV1): Promise<MaterializationLedgerBindingV1> {
    this.#assertOpen()
    const binding = await validateBinding(input)
    const expected = this.#checkpointBinding
    if (expected.materializerKind !== FILE_CHECKPOINT_MATERIALIZER_FSA_TREE ||
        binding.operationId !== expected.operationId ||
        binding.receiveIntentDigest !== expected.receiveIntentDigest ||
        binding.materializationBindingDigest !== expected.materializationBindingDigest ||
        binding.authorityRef !== expected.authorityRef) {
      throw new TypeError('materialization ledger escaped its checkpoint repository binding')
    }
    return binding
  }

  async *#pages(
    binding: MaterializationLedgerBindingV1,
    sealId: string,
  ): AsyncGenerator<MaterializationLedgerPageSummaryV1> {
    let after: bigint | undefined
    do {
      const page = await this.scanMaterializationLedgerPages(binding, sealId, after)
      yield* page.pages
      after = page.continuationPageOrdinal
    } while (after !== undefined)
  }
}

function abortQuietly(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // A terminal transaction already owns its result.
  }
}
