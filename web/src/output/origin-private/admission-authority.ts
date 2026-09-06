import {
  admitWorkspaceBudget,
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from '../workspace/budget'
import { snapshotIdentity } from '../workspace/canonical'
import {
  objectCapacityTotals, requireCapacityLength,
  type ObjectCapacityFence, type ObjectCapacityRecord, type ObjectGrowthRequest,
} from './object-capacity'

const WORKSPACE_BUDGET_DATABASE_NAME = 'windshare-workspace-budget'
const WORKSPACE_BUDGET_DATABASE_VERSION = 2
const CLAIM_STORE = 'workspace-budget-claims'
const OBJECT_STORE = 'workspace-object-capacity'
const CLAIM_BOUND = 1_048_576
const RESERVATION_BOUND = 1_024

export interface WorkspaceBudgetLeaseRecord {
  readonly id: string
  readonly operationId: string
  readonly token: string
  readonly budgetDigest: string
  readonly peakOwnedBytes: bigint
  readonly expiresAtMilliseconds: number
}

interface Account extends WorkspaceBudgetLeaseRecord {
  readonly occupiedBytes: bigint
  readonly outstandingGrowthBytes: bigint
  readonly metadataHeadroomBytes: bigint
}

export interface WorkspaceBudgetCapacityFacts {
  readonly estimatedQuotaBytes?: bigint
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
  | Readonly<{ kind: 'accepted'; capacity: WorkspaceCapacitySnapshot;
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }> }>
  | Readonly<{ kind: 'rejected'; capacity: WorkspaceCapacitySnapshot;
      admission: Extract<WorkspaceBudgetAdmission, { kind: 'rejected' }> }>

export interface OriginPrivateWorkspaceBudgetLeaseAuthority {
  claim(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision>
  readmit(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision>
  reclaim(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision>
  heartbeat(input: { readonly id: string; readonly token: string;
    readonly expiresAtMilliseconds: number; readonly nowMilliseconds: number }): Promise<void>
  reserveGrowth(fence: ObjectCapacityFence, input: ObjectGrowthRequest,
    reservationId: string, facts: WorkspaceBudgetCapacityFacts): Promise<void>
  settleGrowth(fence: ObjectCapacityFence, objectId: string,
    reservationId: string, actualLength?: bigint): Promise<void>
  reconcileObject(fence: ObjectCapacityFence, objectId: string,
    actualLength: bigint): Promise<void>
  release(id: string, token: string): Promise<void>
  close(): void
}

/** One IDB transaction fences ownership and changes both object and task accounting. */
export class IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority
implements OriginPrivateWorkspaceBudgetLeaseAuthority {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => this.close())
  }

  static async open(databaseName = WORKSPACE_BUDGET_DATABASE_NAME):
  Promise<IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority> {
    if (databaseName.length === 0) throw new TypeError('workspace budget database name is empty')
    return new IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority(await openDatabase(databaseName))
  }

  claim(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'claim')
  }

  readmit(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'readmit')
  }

  reclaim(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts): Promise<WorkspaceBudgetLeaseDecision> {
    return this.#decide(record, budget, facts, 'reclaim')
  }

  async heartbeat(input: { readonly id: string; readonly token: string;
    readonly expiresAtMilliseconds: number; readonly nowMilliseconds: number }): Promise<void> {
    validateLeaseTimes(input.expiresAtMilliseconds, input.nowMilliseconds)
    const transaction = this.#transaction()
    const account = await ownedAccount(transaction, {
      operationId: input.id, token: input.token, nowMilliseconds: input.nowMilliseconds,
    })
    transaction.objectStore(CLAIM_STORE).put({ ...account, expiresAtMilliseconds: input.expiresAtMilliseconds })
    await transactionCompletion(transaction)
  }

  async reserveGrowth(fence: ObjectCapacityFence, input: ObjectGrowthRequest,
    reservationId: string, facts: WorkspaceBudgetCapacityFacts): Promise<void> {
    if (input.operationId !== fence.operationId || reservationId.length === 0) {
      throw new TypeError('Object growth escaped its task')
    }
    requireCapacityLength(input.currentLength)
    requireCapacityLength(input.targetLength)
    requireCapacityLength(input.metadataHeadroom)
    const transaction = this.#transaction()
    const account = await ownedAccount(transaction, fence)
    const object = await readObject(transaction, fence, input.objectId, input.currentLength)
    if (object.occupiedBytes !== input.currentLength) {
      transaction.abort()
      throw new DOMException('Object length requires reconciliation', 'InvalidStateError')
    }
    if (object.reservations.length >= RESERVATION_BOUND ||
        object.reservations.some((entry) => entry.reservationId === reservationId)) {
      transaction.abort()
      throw new DOMException('Object reservation queue is full or duplicated', 'InvalidStateError')
    }
    const next: ObjectCapacityRecord = { ...object, reservations: [...object.reservations, {
      reservationId, targetLength: input.targetLength, metadataHeadroom: input.metadataHeadroom,
    }] }
    const replacement = updateAccount(account, object, next)
    const inventory = await accounts(transaction, facts.nowMilliseconds)
    const occupied = sum(inventory, (entry) => entry.occupiedBytes)
    const outstanding = sum(inventory, (entry) => entry.id === account.id
      ? replacement.outstandingGrowthBytes + replacement.metadataHeadroomBytes
      : entry.outstandingGrowthBytes + entry.metadataHeadroomBytes)
    const usage = facts.currentUsageBytes > occupied ? facts.currentUsageBytes : occupied
    const additionalCapacity = replacement.outstandingGrowthBytes + replacement.metadataHeadroomBytes -
      account.outstandingGrowthBytes - account.metadataHeadroomBytes
    // A lower advisory estimate must not strand already-admitted regions: filling
    // them cannot consume more capacity, and is the useful work under pressure.
    if (additionalCapacity > 0n && facts.estimatedQuotaBytes !== undefined &&
        usage + outstanding + facts.minimumReserveBytes > facts.estimatedQuotaBytes) {
      transaction.abort()
      throw new DOMException('Origin-private object growth exceeds estimated capacity', 'QuotaExceededError')
    }
    transaction.objectStore(OBJECT_STORE).put(next)
    transaction.objectStore(CLAIM_STORE).put(replacement)
    await transactionCompletion(transaction)
  }

  async settleGrowth(fence: ObjectCapacityFence, objectId: string,
    reservationId: string, actualLength?: bigint): Promise<void> {
    if (actualLength !== undefined) requireCapacityLength(actualLength)
    const transaction = this.#transaction()
    const account = await ownedAccount(transaction, fence)
    const object = await readObject(transaction, fence, objectId, 0n)
    const reservation = object.reservations.find((entry) => entry.reservationId === reservationId)
    if (reservation === undefined) {
      transaction.abort()
      throw new OriginPrivateWorkspaceBudgetOwnershipError()
    }
    const next: ObjectCapacityRecord = {
      ...object,
      occupiedBytes: actualLength ?? object.occupiedBytes,
      reservations: object.reservations.filter((entry) => entry.reservationId !== reservationId),
    }
    transaction.objectStore(OBJECT_STORE).put(next)
    transaction.objectStore(CLAIM_STORE).put(updateAccount(account, object, next))
    await transactionCompletion(transaction)
  }

  async reconcileObject(fence: ObjectCapacityFence, objectId: string, actualLength: bigint): Promise<void> {
    requireCapacityLength(actualLength)
    const transaction = this.#transaction()
    const account = await ownedAccount(transaction, fence)
    const object = await readObject(transaction, fence, objectId, actualLength)
    if (object.reservations.length > 0) {
      transaction.abort()
      throw new DOMException('Cannot reconcile an object with admitted writes', 'InvalidStateError')
    }
    const next = { ...object, occupiedBytes: actualLength }
    transaction.objectStore(OBJECT_STORE).put(next)
    transaction.objectStore(CLAIM_STORE).put(updateAccount(account, object, next))
    await transactionCompletion(transaction)
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#closed) return
    const transaction = this.#transaction()
    const store = transaction.objectStore(CLAIM_STORE)
    const account = await requestResult<Account | undefined>(store.get(id))
    if (account?.token === token) {
      // A crashed/failed write may have consumed its reservation. Retain that conservative
      // occupancy until recovery measures the task; releasing a lease is not deleting bytes.
      store.put({ ...account, token: '', expiresAtMilliseconds: 0,
        occupiedBytes: account.occupiedBytes + account.outstandingGrowthBytes,
        outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n })
    }
    await transactionCompletion(transaction)
  }

  /** Only namespace cleanup may call this, after proving the owned directory is absent. */
  async forgetDeletedOperation(operationId: string): Promise<void> {
    snapshotIdentity(operationId, 16, 'operation ID')
    const transaction = this.#transaction()
    transaction.objectStore(CLAIM_STORE).delete(operationId)
    const prefix = operationId + ':'
    transaction.objectStore(OBJECT_STORE).delete(IDBKeyRange.bound(prefix, prefix + '\uffff'))
    await transactionCompletion(transaction)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async #decide(record: WorkspaceBudgetLeaseRecord, budget: WorkspaceBudgetV1,
    facts: WorkspaceBudgetCapacityFacts, mode: 'claim' | 'readmit' | 'reclaim'):
  Promise<WorkspaceBudgetLeaseDecision> {
    snapshotIdentity(record.operationId, 16, 'operation ID')
    if (record.id !== record.operationId || record.operationId !== budget.operationId ||
        record.budgetDigest !== budget.digest || record.peakOwnedBytes !== budget.peakOwnedBytes) {
      throw new TypeError('Workspace lease escaped its canonical budget')
    }
    validateLeaseTimes(record.expiresAtMilliseconds, facts.nowMilliseconds)
    const transaction = this.#transaction()
    const store = transaction.objectStore(CLAIM_STORE)
    const inventory = await accounts(transaction, facts.nowMilliseconds)
    const existing = inventory.find((entry) => entry.id === record.id)
    if ((mode === 'readmit' && (existing?.token !== record.token ||
          existing.expiresAtMilliseconds <= facts.nowMilliseconds)) ||
        (mode === 'claim' && existing !== undefined && existing.token !== record.token &&
          existing.expiresAtMilliseconds > facts.nowMilliseconds)) {
      transaction.abort()
      throw new OriginPrivateWorkspaceBudgetOwnershipError()
    }
    if (existing !== undefined && existing.budgetDigest !== record.budgetDigest) {
      transaction.abort()
      throw new OriginPrivateWorkspaceBudgetOwnershipError()
    }
    const current = mode === 'readmit' && existing !== undefined ? existing : {
      ...record, occupiedBytes: facts.verifiedAlreadyOwnedBytes, outstandingGrowthBytes: 0n,
      metadataHeadroomBytes: budget.durableMetadataBytes,
    }
    const others = inventory.filter((entry) => entry.id !== record.id)
    const capacity: WorkspaceCapacitySnapshot = {
      ...(facts.estimatedQuotaBytes === undefined ? {} : { estimatedQuotaBytes: facts.estimatedQuotaBytes }),
      currentUsageBytes: maximum(facts.currentUsageBytes,
        sum(others, (entry) => entry.occupiedBytes) + current.occupiedBytes),
      minimumReserveBytes: facts.minimumReserveBytes,
      verifiedAlreadyOwnedBytes: current.occupiedBytes,
      outstandingGrowthBytes: current.outstandingGrowthBytes + sum(others, (entry) => entry.outstandingGrowthBytes),
      metadataHeadroomBytes: current.metadataHeadroomBytes - budget.durableMetadataBytes +
        sum(others, (entry) => entry.metadataHeadroomBytes),
    }
    const admission = admitWorkspaceBudget(budget, capacity)
    if (admission.kind === 'accepted') store.put({ ...current, ...record })
    await transactionCompletion(transaction)
    return admission.kind === 'accepted'
      ? { kind: 'accepted', capacity, admission } : { kind: 'rejected', capacity, admission }
  }

  #transaction(): IDBTransaction {
    if (this.#closed) throw new DOMException('Workspace budget authority is closed', 'InvalidStateError')
    return this.#database.transaction([CLAIM_STORE, OBJECT_STORE], 'readwrite', { durability: 'strict' })
  }
}

export async function forgetDeletedWorkspaceCapacity(operationId: string): Promise<void> {
  // Without IndexedDB this context could never have persisted a capacity account.
  if (typeof indexedDB === 'undefined') return
  const authority = await IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority.open()
  try { await authority.forgetDeletedOperation(operationId) } finally { authority.close() }
}

function updateAccount(account: Account, before: ObjectCapacityRecord, after: ObjectCapacityRecord): Account {
  const previous = objectCapacityTotals(before)
  const next = objectCapacityTotals(after)
  return {
    ...account,
    occupiedBytes: requireCapacityLength(account.occupiedBytes + next.occupiedBytes - previous.occupiedBytes),
    outstandingGrowthBytes: requireCapacityLength(account.outstandingGrowthBytes +
      next.outstandingGrowthBytes - previous.outstandingGrowthBytes),
    metadataHeadroomBytes: requireCapacityLength(account.metadataHeadroomBytes +
      next.metadataHeadroomBytes - previous.metadataHeadroomBytes),
  }
}

async function ownedAccount(transaction: IDBTransaction, fence: ObjectCapacityFence): Promise<Account> {
  const account = await requestResult<Account | undefined>(transaction.objectStore(CLAIM_STORE).get(fence.operationId))
  if (account === undefined || account.token !== fence.token ||
      account.expiresAtMilliseconds <= fence.nowMilliseconds) {
    transaction.abort()
    throw new OriginPrivateWorkspaceBudgetOwnershipError()
  }
  return account
}

async function readObject(transaction: IDBTransaction, fence: ObjectCapacityFence,
  objectId: string, initialLength: bigint): Promise<ObjectCapacityRecord> {
  snapshotIdentity(objectId, 32, 'owned object ID')
  const id = fence.operationId + ':' + objectId
  const object = await requestResult<ObjectCapacityRecord | undefined>(transaction.objectStore(OBJECT_STORE).get(id))
  return object?.token === fence.token ? object : {
    id, operationId: fence.operationId, objectId, token: fence.token,
    occupiedBytes: initialLength, reservations: [],
  }
}

async function accounts(transaction: IDBTransaction, nowMilliseconds: number): Promise<Account[]> {
  const values = await requestResult<Account[]>(transaction.objectStore(CLAIM_STORE).getAll(undefined, CLAIM_BOUND + 1))
  if (values.length > CLAIM_BOUND) throw new DOMException('Workspace task inventory exceeds its bound', 'QuotaExceededError')
  return values.map((account) => {
    if (account.expiresAtMilliseconds > nowMilliseconds || account.token === '') return account
    const expired = { ...account, token: '', expiresAtMilliseconds: 0,
      occupiedBytes: account.occupiedBytes + account.outstandingGrowthBytes,
      outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n }
    transaction.objectStore(CLAIM_STORE).put(expired)
    return expired
  })
}

function sum(values: readonly Account[], select: (value: Account) => bigint): bigint {
  return values.reduce((total, value) => requireCapacityLength(total + select(value)), 0n)
}
function maximum(left: bigint, right: bigint): bigint { return left > right ? left : right }

function validateLeaseTimes(expires: number, now: number): void {
  if (!Number.isSafeInteger(expires) || !Number.isSafeInteger(now) || now < 0 || expires <= now) {
    throw new TypeError('Workspace budget lease time is invalid')
  }
}

async function openDatabase(name: string): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB workspace budget authority is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, WORKSPACE_BUDGET_DATABASE_VERSION)
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('upgradeneeded', () => {
      for (const store of [CLAIM_STORE, OBJECT_STORE]) {
        if (request.result.objectStoreNames.contains(store)) request.result.deleteObjectStore(store)
        request.result.createObjectStore(store, { keyPath: 'id' })
      }
    })
    request.addEventListener('blocked', () => reject(new DOMException('Workspace budget database upgrade is blocked', 'InvalidStateError')), { once: true })
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
