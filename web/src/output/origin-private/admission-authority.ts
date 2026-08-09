import {
  admitWorkspaceBudget,
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from '../workspace/budget'
import { snapshotIdentity } from '../workspace/canonical'

const WORKSPACE_BUDGET_DATABASE_NAME = 'windshare-workspace-budget'
const WORKSPACE_BUDGET_DATABASE_VERSION = 1
const WORKSPACE_BUDGET_CLAIM_STORE = 'workspace-budget-claims'
const WORKSPACE_BUDGET_CLAIM_BOUND = 1_048_576

export interface WorkspaceBudgetLeaseRecord {
  readonly id: string
  readonly operationId: string
  readonly token: string
  readonly budgetDigest: string
  readonly peakOwnedBytes: bigint
  readonly expiresAtMilliseconds: number
}

export interface WorkspaceBudgetCapacityFacts {
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly verifiedAlreadyOwnedBytes: bigint
  readonly nowMilliseconds: number
}

export class OriginPrivateWorkspaceBudgetOwnershipError extends DOMException {
  constructor() {
    super('Workspace budget lease ownership changed', 'InvalidStateError')
  }
}

export type WorkspaceBudgetLeaseDecision =
  | Readonly<{
      kind: 'accepted'
      capacity: WorkspaceCapacitySnapshot
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }>
    }>
  | Readonly<{
      kind: 'rejected'
      capacity: WorkspaceCapacitySnapshot
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }>
    }>

export interface OriginPrivateWorkspaceBudgetLeaseAuthority {
  claim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision>
  readmit(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision>
  reclaim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision>
  heartbeat(input: {
    readonly id: string
    readonly token: string
    readonly expiresAtMilliseconds: number
    readonly nowMilliseconds: number
  }): Promise<void>
  release(id: string, token: string): Promise<void>
  close(): void
}

export class IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority
implements OriginPrivateWorkspaceBudgetLeaseAuthority {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(
    databaseName = WORKSPACE_BUDGET_DATABASE_NAME,
  ): Promise<IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority> {
    if (databaseName.length === 0) throw new TypeError('workspace budget database name is empty')
    return new IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority(
      await openDatabase(databaseName),
    )
  }

  claim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'claim')
  }

  readmit(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'readmit')
  }

  reclaim(
    record: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts,
  ): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'reclaim')
  }

  async heartbeat(input: {
    readonly id: string
    readonly token: string
    readonly expiresAtMilliseconds: number
    readonly nowMilliseconds: number
  }): Promise<void> {
    this.#requireOpen()
    validateLeaseTimes(input.expiresAtMilliseconds, input.nowMilliseconds)
    const transaction = this.#database.transaction(WORKSPACE_BUDGET_CLAIM_STORE, 'readwrite')
    const store = transaction.objectStore(WORKSPACE_BUDGET_CLAIM_STORE)
    const existing = await requestResult<unknown>(store.get(input.id))
    if (existing === undefined) {
      abortOwnership(transaction)
    }
    const record = validateLeaseRecord(existing as WorkspaceBudgetLeaseRecord)
    if (record.token !== input.token || record.expiresAtMilliseconds <= input.nowMilliseconds) {
      abortOwnership(transaction)
    }
    store.put(Object.freeze({ ...record, expiresAtMilliseconds: input.expiresAtMilliseconds }))
    await transactionCompletion(transaction)
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#closed) return
    const transaction = this.#database.transaction(WORKSPACE_BUDGET_CLAIM_STORE, 'readwrite')
    const store = transaction.objectStore(WORKSPACE_BUDGET_CLAIM_STORE)
    const existing = await requestResult<unknown>(store.get(id))
    if (existing !== undefined && validateLeaseRecord(existing as WorkspaceBudgetLeaseRecord).token === token) {
      store.delete(id)
    }
    await transactionCompletion(transaction)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async #decide(
    recordInput: WorkspaceBudgetLeaseRecord,
    budget: WorkspaceBudgetV1,
    factsInput: WorkspaceBudgetCapacityFacts,
    mode: 'claim' | 'readmit' | 'reclaim',
  ): Promise<WorkspaceBudgetLeaseDecision> {
    this.#requireOpen()
    const record = validateLeaseRecord(recordInput)
    const facts = validateCapacityFacts(factsInput)
    if (record.operationId !== budget.operationId || record.budgetDigest !== budget.digest ||
        record.peakOwnedBytes !== budget.peakOwnedBytes) {
      throw new TypeError('workspace budget lease escaped its canonical budget')
    }
    validateLeaseTimes(record.expiresAtMilliseconds, facts.nowMilliseconds)
    const transaction = this.#database.transaction(WORKSPACE_BUDGET_CLAIM_STORE, 'readwrite')
    const store = transaction.objectStore(WORKSPACE_BUDGET_CLAIM_STORE)
    const inventory = await liveClaimInventory(store, facts.nowMilliseconds, record.id)
    if (mode === 'readmit' && (inventory.existing === undefined ||
        inventory.existing.token !== record.token ||
        !sameBudgetAuthority(inventory.existing, record))) {
      abortOwnership(transaction)
    }
    if (mode === 'claim' && inventory.existing !== undefined &&
        inventory.existing.token !== record.token) {
      abortOwnership(transaction)
    }
    if (mode === 'reclaim' && inventory.existing !== undefined &&
        !sameBudgetAuthority(inventory.existing, record)) {
      abortOwnership(transaction)
    }
    const capacity = Object.freeze({
      jobLimitBytes: facts.jobLimitBytes,
      processLimitBytes: facts.processLimitBytes,
      otherActiveJobPeakBytes: inventory.otherActiveJobPeakBytes,
      estimatedQuotaBytes: facts.estimatedQuotaBytes,
      currentUsageBytes: facts.currentUsageBytes,
      minimumReserveBytes: facts.minimumReserveBytes,
      verifiedAlreadyOwnedBytes: facts.verifiedAlreadyOwnedBytes,
    })
    const admission = admitWorkspaceBudget(budget, capacity)
    if (admission.kind === 'accepted') store.put(record)
    await transactionCompletion(transaction)
    return admission.kind === 'accepted'
      ? Object.freeze({ kind: 'accepted', capacity, admission })
      : Object.freeze({ kind: 'rejected', capacity, admission })
  }

  #requireOpen(): void {
    if (this.#closed) {
      throw new DOMException('Workspace budget authority is closed', 'InvalidStateError')
    }
  }
}

async function liveClaimInventory(
  store: IDBObjectStore,
  nowMilliseconds: number,
  targetId: string,
): Promise<Readonly<{
  otherActiveJobPeakBytes: bigint
  existing?: WorkspaceBudgetLeaseRecord
}>> {
  const records = await requestResult<unknown[]>(
    store.getAll(undefined, WORKSPACE_BUDGET_CLAIM_BOUND + 1),
  )
  if (records.length > WORKSPACE_BUDGET_CLAIM_BOUND) {
    throw new DOMException('Workspace budget claim inventory exceeds its bound', 'QuotaExceededError')
  }
  let otherActiveJobPeakBytes = 0n
  let existing: WorkspaceBudgetLeaseRecord | undefined
  for (const value of records) {
    const record = validateLeaseRecord(value as WorkspaceBudgetLeaseRecord)
    if (record.expiresAtMilliseconds <= nowMilliseconds) {
      store.delete(record.id)
    } else if (record.id === targetId) {
      existing = record
    } else {
      otherActiveJobPeakBytes = checkedAdd(otherActiveJobPeakBytes, record.peakOwnedBytes)
    }
  }
  return Object.freeze({
    otherActiveJobPeakBytes,
    ...(existing === undefined ? {} : { existing }),
  })
}

function sameBudgetAuthority(
  existing: WorkspaceBudgetLeaseRecord,
  replacement: WorkspaceBudgetLeaseRecord,
): boolean {
  return existing.operationId === replacement.operationId &&
    existing.budgetDigest === replacement.budgetDigest &&
    existing.peakOwnedBytes === replacement.peakOwnedBytes
}

function validateLeaseRecord(record: WorkspaceBudgetLeaseRecord): WorkspaceBudgetLeaseRecord {
  if (record === null || typeof record !== 'object' ||
      typeof record.id !== 'string' || record.id.length === 0 ||
      typeof record.token !== 'string' || record.token.length === 0 ||
      typeof record.peakOwnedBytes !== 'bigint' || record.peakOwnedBytes < 0n ||
      record.peakOwnedBytes > 0xffff_ffff_ffff_ffffn ||
      !Number.isSafeInteger(record.expiresAtMilliseconds)) {
    throw new TypeError('workspace budget lease record is invalid')
  }
  const operationId = snapshotIdentity(record.operationId, 16, 'operation ID')
  const budgetDigest = snapshotIdentity(record.budgetDigest, 32, 'workspace budget digest')
  if (record.id !== operationId) throw new TypeError('workspace budget lease ID is not canonical')
  return Object.freeze({ ...record, operationId, budgetDigest })
}

function validateCapacityFacts(facts: WorkspaceBudgetCapacityFacts): WorkspaceBudgetCapacityFacts {
  const values = [
    facts.jobLimitBytes,
    facts.processLimitBytes,
    facts.estimatedQuotaBytes,
    facts.currentUsageBytes,
    facts.minimumReserveBytes,
    facts.verifiedAlreadyOwnedBytes,
  ]
  if (values.some((value) =>
    typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) ||
      facts.jobLimitBytes === 0n || facts.processLimitBytes === 0n ||
      !Number.isSafeInteger(facts.nowMilliseconds) || facts.nowMilliseconds < 0) {
    throw new TypeError('workspace budget capacity facts are invalid')
  }
  return Object.freeze({ ...facts })
}

function validateLeaseTimes(expiresAtMilliseconds: number, nowMilliseconds: number): void {
  if (!Number.isSafeInteger(expiresAtMilliseconds) || !Number.isSafeInteger(nowMilliseconds) ||
      nowMilliseconds < 0 || expiresAtMilliseconds <= nowMilliseconds) {
    throw new TypeError('workspace budget lease time is invalid')
  }
}

function checkedAdd(left: bigint, right: bigint): bigint {
  const value = left + right
  if (value > 0xffff_ffff_ffff_ffffn) {
    throw new RangeError('workspace budget process accounting overflow')
  }
  return value
}

function abortOwnership(transaction: IDBTransaction): never {
  transaction.abort()
  throw new OriginPrivateWorkspaceBudgetOwnershipError()
}

async function openDatabase(name: string): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB workspace budget authority is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, WORKSPACE_BUDGET_DATABASE_VERSION)
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('upgradeneeded', () => {
      if (!request.result.objectStoreNames.contains(WORKSPACE_BUDGET_CLAIM_STORE)) {
        request.result.createObjectStore(WORKSPACE_BUDGET_CLAIM_STORE, { keyPath: 'id' })
      }
    })
    request.addEventListener('blocked', () => reject(new DOMException(
      'Workspace budget database upgrade is blocked by another context',
      'InvalidStateError',
    )), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}
