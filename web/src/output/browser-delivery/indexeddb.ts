import {
  DEFAULT_OUTPUT_DATABASE_NAME, INDEXEDDB_BROWSER_DELIVERY_FILE_STORE, INDEXEDDB_BROWSER_SAVE_POLICY_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_BY_OPERATION_FILE_INDEX,
  INDEXEDDB_FILE_FINAL_PROOF_STORE, INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX,
  openIndexedDbCheckpointDatabase, requestResult, transactionCompletion,
} from '../browser/indexeddb-database'
import { readStoredCheckpoint } from '../browser/indexeddb/repository-transactions'
import { FILE_ID_BYTES, OPERATION_ID_BYTES, fileCheckpointDigest } from '../persistence/checkpoint'
import { snapshotIdentity } from '../workspace/canonical'
import { requireBrowserTargetProof, validateBrowserTargetProof, validateDirectCheckpointProof } from './final-proof'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import { authorizeBrowserDeliveryRestart, advanceBrowserDeliveryRecord, assertBrowserDeliveryTransition, stageCheckpoint, targetCheckpoint } from './lifecycle'
import {
  BROWSER_DELIVERY_PAGE_LIMIT, type BrowserDeliveryRecordV1, type BrowserDeliveryTrace,
  type BrowserSavePolicyV1,
} from './model'
import { validateBrowserSavePolicy } from './policy'
import { snapshotBrowserDeliveryRecord, validateBrowserDeliveryRecord } from './records'
import {
  BrowserDeliveryConcurrencyError, type BrowserDeliveryFilePage, type BrowserDeliveryFileScan,
  type BrowserDeliveryRepository,
} from './repository'

const MUTATION_STORES = [
  INDEXEDDB_BROWSER_SAVE_POLICY_STORE, INDEXEDDB_BROWSER_DELIVERY_FILE_STORE,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE, INDEXEDDB_FILE_FINAL_PROOF_STORE,
]

export class IndexedDbBrowserDeliveryRepository implements BrowserDeliveryRepository {
  readonly #database: IDBDatabase
  readonly #trace: BrowserDeliveryTrace | undefined

  private constructor(database: IDBDatabase, trace?: BrowserDeliveryTrace) {
    this.#database = database
    this.#trace = trace
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(options: {
    readonly databaseName?: string
    readonly trace?: BrowserDeliveryTrace
  } = {}): Promise<IndexedDbBrowserDeliveryRepository> {
    return new IndexedDbBrowserDeliveryRepository(
      await openIndexedDbCheckpointDatabase(options.databaseName ?? DEFAULT_OUTPUT_DATABASE_NAME),
      options.trace,
    )
  }

  async installPolicy(input: BrowserSavePolicyV1): Promise<BrowserSavePolicyV1> {
    const policy = validateBrowserSavePolicy(input)
    const result = await this.#transaction([INDEXEDDB_BROWSER_SAVE_POLICY_STORE], 'readwrite', async transaction => {
      const store = transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE)
      const prior: unknown = await requestResult(store.get(policy.operationId))
      if (prior !== undefined) {
        const existing = validateBrowserSavePolicy(prior as BrowserSavePolicyV1)
        if (existing.digest !== policy.digest) throw new BrowserDeliveryConcurrencyError('Browser save preference and authority are immutable')
        return existing
      }
      await requestResult(store.add(policy))
      return policy
    })
    this.#observe({ name: 'browser.delivery.policy_committed', operation_id: policy.operationId, policy_digest: policy.digest })
    return result
  }

  readPolicy(operationId: string): Promise<BrowserSavePolicyV1 | undefined> {
    const id = snapshotIdentity(operationId, OPERATION_ID_BYTES, 'operation ID')
    return this.#transaction([INDEXEDDB_BROWSER_SAVE_POLICY_STORE], 'readonly', async transaction => {
      const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(id))
      return raw === undefined ? undefined : validateBrowserSavePolicy(raw as BrowserSavePolicyV1)
    })
  }

  async createFile(input: BrowserDeliveryRecordV1): Promise<BrowserDeliveryRecordV1> {
    const result = await this.#transaction(MUTATION_STORES, 'readwrite', async transaction => {
      const policy = await requirePolicy(transaction, input.operationId)
      const record = validateBrowserDeliveryRecord(policy, input)
      if (record.generation !== 1n || record.state.kind !== 'receiving' || record.state.checkpoint !== undefined ||
          record.localMutation !== undefined) {
        throw new TypeError('File placement must be persisted before receiving starts')
      }
      const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
      const raw: unknown = await requestResult(store.get(fileKey(record.operationId, record.fileId)))
      if (raw !== undefined) {
        const existing = validateBrowserDeliveryRecord(policy, raw as BrowserDeliveryRecordV1)
        // Repeated placement acquisition returns its current state, never rewinds received progress.
        const comparable = snapshotBrowserDeliveryRecord(policy, {
          ...record, generation: existing.generation, state: existing.state,
          ...(existing.localMutation === undefined ? {} : { localMutation: existing.localMutation }),
        })
        if (comparable.digest !== existing.digest) throw new BrowserDeliveryConcurrencyError('Started source or placement changed')
        return existing
      }
      await requestResult(store.add(record))
      return record
    })
    this.#observeFile(result)
    return result
  }

  readFile(operationId: string, fileId: string): Promise<BrowserDeliveryRecordV1 | undefined> {
    const key = fileKey(operationId, fileId)
    return this.#transaction(MUTATION_STORES, 'readonly', async transaction => {
      const policy = await requirePolicy(transaction, operationId)
      const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE).get(key))
      return raw === undefined ? undefined : validateBrowserDeliveryRecord(policy, raw as BrowserDeliveryRecordV1)
    })
  }

  async replaceFile(previousInput: BrowserDeliveryRecordV1, nextInput: BrowserDeliveryRecordV1): Promise<void> {
    if (nextInput.state.kind === 'restart-authorized') throw new TypeError('Restart requires explicit authorization')
    await this.#replace(previousInput, nextInput)
  }

  async authorizeRestart(previous: BrowserDeliveryRecordV1, checkpoint: import('../persistence/checkpoint').FileCheckpointV2,
    authorizationId: string): Promise<BrowserDeliveryRecordV1> {
    const policy = await this.readPolicy(previous.operationId)
    if (policy === undefined) throw new BrowserDeliveryConcurrencyError()
    const next = authorizeBrowserDeliveryRestart(policy, previous, checkpoint, authorizationId)
    await this.#replace(previous, next)
    return next
  }

  async #replace(previousInput: BrowserDeliveryRecordV1, nextInput: BrowserDeliveryRecordV1): Promise<void> {
    const next = await this.#transaction(MUTATION_STORES, 'readwrite', async transaction => {
      const policy = await requirePolicy(transaction, previousInput.operationId)
      const previous = validateBrowserDeliveryRecord(policy, previousInput)
      const candidate = validateBrowserDeliveryRecord(policy, nextInput)
      assertBrowserDeliveryTransition(policy, previous, candidate)
      const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
      const raw: unknown = await requestResult(store.get(fileKey(previous.operationId, previous.fileId)))
      const current = raw === undefined ? undefined : validateBrowserDeliveryRecord(policy, raw as BrowserDeliveryRecordV1)
      if (current?.digest !== previous.digest) throw new BrowserDeliveryConcurrencyError()
      await requireCommittedCheckpoints(transaction, previous, candidate)
      const target = targetCheckpoint(candidate.state)
      if (target !== undefined && targetCheckpoint(previous.state) === undefined) {
        await requireBrowserTargetProof(transaction, target)
      }
      await requestResult(store.put(candidate))
      return candidate
    })
    this.#observeFile(next, previousInput.state.kind)
  }

  async finalizeDirect(
    previousInput: BrowserDeliveryRecordV1,
    proof: FinalFileCheckpointProof,
  ): Promise<BrowserDeliveryRecordV1> {
    const result = await this.#transaction(MUTATION_STORES, 'readwrite', async transaction => {
      const store = transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
      const [policyRaw, currentRaw, checkpointRaw, finalProofRaw] = await Promise.all([
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE).get(previousInput.operationId)),
        requestResult<unknown>(store.get(fileKey(previousInput.operationId, previousInput.fileId))),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).get(proof.recordId)),
        requestResult<unknown>(transaction.objectStore(INDEXEDDB_FILE_FINAL_PROOF_STORE)
          .index(INDEXEDDB_BY_OPERATION_RECORD_PROOF_INDEX).get([previousInput.operationId, proof.recordId])),
      ])
      if (policyRaw === undefined || currentRaw === undefined || checkpointRaw === undefined) {
        throw new BrowserDeliveryConcurrencyError('Direct finalization requires persisted policy, file, and checkpoint authority')
      }
      const policy = validateBrowserSavePolicy(policyRaw as BrowserSavePolicyV1)
      const previous = validateBrowserDeliveryRecord(policy, previousInput)
      if (previous.placement !== 'direct' || previous.state.kind !== 'receiving') {
        throw new TypeError('Only direct receiving can finalize without staging cleanup')
      }
      const checkpoint = readStoredCheckpoint(checkpointRaw)
      validateDirectCheckpointProof(proof, checkpoint)
      validateBrowserTargetProof(finalProofRaw, checkpoint)
      const saved = advanceBrowserDeliveryRecord(policy, previous, { kind: 'target-saved', target: checkpoint })
      const cleaned = advanceBrowserDeliveryRecord(policy, saved, { kind: 'cleaned', target: checkpoint })
      const current = validateBrowserDeliveryRecord(policy, currentRaw as BrowserDeliveryRecordV1)
      if (current.digest === cleaned.digest) return current
      if (current.digest !== previous.digest) throw new BrowserDeliveryConcurrencyError()
      await requestResult(store.put(cleaned))
      return cleaned
    })
    this.#observeFile(result, previousInput.state.kind)
    return result
  }

  scanFiles(input: BrowserDeliveryFileScan): Promise<BrowserDeliveryFilePage> {
    const operationId = snapshotIdentity(input.operationId, OPERATION_ID_BYTES, 'operation ID')
    const afterFileId = input.afterFileId === undefined ? undefined
      : snapshotIdentity(input.afterFileId, FILE_ID_BYTES, 'file ID')
    const limit = input.limit ?? BROWSER_DELIVERY_PAGE_LIMIT
    if (!Number.isInteger(limit) || limit < 1 || limit > BROWSER_DELIVERY_PAGE_LIMIT) {
      throw new TypeError('Browser delivery scan exceeds its fixed page limit')
    }
    return this.#transaction(MUTATION_STORES, 'readonly', async transaction => {
      const policy = await requirePolicy(transaction, operationId)
      const range = IDBKeyRange.bound([operationId, afterFileId ?? ''], [operationId, '\uffff'], afterFileId !== undefined)
      const raw: unknown[] = await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_DELIVERY_FILE_STORE)
        .index(INDEXEDDB_BY_OPERATION_FILE_INDEX).getAll(range, limit + 1))
      const records = raw.slice(0, limit).map(value => validateBrowserDeliveryRecord(policy, value as BrowserDeliveryRecordV1))
      return Object.freeze({
        records: Object.freeze(records),
        ...(raw.length <= limit ? {} : { nextFileId: records[records.length - 1]!.fileId }),
      })
    })
  }

  close(): void { this.#database.close() }

  async #transaction<T>(
    stores: readonly string[], mode: IDBTransactionMode,
    run: (transaction: IDBTransaction) => Promise<T>,
  ): Promise<T> {
    const transaction = this.#database.transaction([...stores], mode)
    const completion = transactionCompletion(transaction)
    // An abort can happen while a request rejection is unwinding the callback.
    completion.catch(() => undefined)
    try {
      const value = await run(transaction)
      await completion
      return value
    } catch (error) {
      try { transaction.abort() } catch { /* A failed transaction may already be complete. */ }
      await completion.catch(() => undefined)
      throw error
    }
  }

  #observeFile(record: BrowserDeliveryRecordV1, priorState?: BrowserDeliveryRecordV1['state']['kind']): void {
    this.#observe({
      name: 'browser.delivery.file_committed', operation_id: record.operationId,
      policy_digest: record.policyDigest, file_id: record.fileId, generation: record.generation,
      placement: record.placement, placement_reason: record.placementReason, state: record.state.kind,
      ...(priorState === undefined ? {} : { prior_state: priorState }),
    })
  }

  #observe(event: Parameters<BrowserDeliveryTrace>[0]): void {
    try { this.#trace?.(event) } catch { /* Diagnostic failure cannot undo a committed authority cut. */ }
  }
}

function fileKey(operationId: string, fileId: string): IDBValidKey[] {
  return [
    snapshotIdentity(operationId, OPERATION_ID_BYTES, 'operation ID'),
    snapshotIdentity(fileId, FILE_ID_BYTES, 'file ID'),
  ]
}

async function requirePolicy(transaction: IDBTransaction, operationId: string): Promise<BrowserSavePolicyV1> {
  const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_BROWSER_SAVE_POLICY_STORE)
    .get(snapshotIdentity(operationId, OPERATION_ID_BYTES, 'operation ID')))
  if (raw === undefined) throw new BrowserDeliveryConcurrencyError('Browser save policy must commit before receiving')
  return validateBrowserSavePolicy(raw as BrowserSavePolicyV1)
}

async function requireCommittedCheckpoints(
  transaction: IDBTransaction,
  previous: BrowserDeliveryRecordV1,
  next: BrowserDeliveryRecordV1,
): Promise<void> {
  const prior = [
    'checkpoint' in previous.state ? previous.state.checkpoint : undefined,
    stageCheckpoint(previous.state), targetCheckpoint(previous.state),
  ]
  const references = [
    'checkpoint' in next.state ? next.state.checkpoint : undefined,
    stageCheckpoint(next.state), targetCheckpoint(next.state),
  ].filter(checkpoint => checkpoint !== undefined).filter(checkpoint =>
    !prior.some(retained => retained !== undefined && fileCheckpointDigest(retained) === fileCheckpointDigest(checkpoint)))
  for (const checkpoint of references) {
    const raw: unknown = await requestResult(transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).get(checkpoint.recordId))
    if (raw === undefined || fileCheckpointDigest(readStoredCheckpoint(raw)) !== fileCheckpointDigest(checkpoint)) {
      throw new BrowserDeliveryConcurrencyError('Delivery proof must already be the exact committed storage checkpoint')
    }
  }
}
