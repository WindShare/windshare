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

import { ORIGIN_CAPACITY_DATABASE_NAME as WORKSPACE_BUDGET_DATABASE_NAME,
  WORKSPACE_CLAIM_STORE as CLAIM_STORE, WORKSPACE_OBJECT_STORE as OBJECT_STORE,
  STAGING_FILE_STORE, CAPACITY_INVENTORY_BOUND as CLAIM_BOUND, openOriginCapacityDatabase,
  capacityRequest as requestResult, capacityTransaction } from './capacity/database'
import { stagingCapacityTotals } from '../staging-budget/admission'
import {
  workspaceCapacityAccount, workspaceObjectCapacity, stagingFileCapacity,
  OBJECT_RESERVATION_BOUND as RESERVATION_BOUND,
  type WorkspaceCapacityAccount as Account, type WorkspaceBudgetLeaseRecord,
} from './capacity/records'
export type { WorkspaceBudgetLeaseRecord } from './capacity/records'

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
    return new IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority(await openOriginCapacityDatabase(databaseName))
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
    return this.#transact(async transaction => {
      const account = await ownedAccount(transaction, {
        operationId: input.id, token: input.token, nowMilliseconds: input.nowMilliseconds,
      })
      transaction.objectStore(CLAIM_STORE).put({ ...account, expiresAtMilliseconds: input.expiresAtMilliseconds })
    })
  }

  async reserveGrowth(fence: ObjectCapacityFence, input: ObjectGrowthRequest,
    reservationId: string, facts: WorkspaceBudgetCapacityFacts): Promise<void> {
    if (input.operationId !== fence.operationId || reservationId.length === 0) {
      throw new TypeError('Object growth escaped its task')
    }
    requireCapacityLength(input.currentLength)
    requireCapacityLength(input.targetLength)
    requireCapacityLength(input.metadataHeadroom)
    return this.#transact(async transaction => {
      const account = await ownedAccount(transaction, fence)
      const object = await readObject(transaction, fence, input.objectId, input.currentLength)
      if (object.occupiedBytes !== input.currentLength) {
        throw new DOMException('Object length requires reconciliation', 'InvalidStateError')
      }
      if (object.reservations.length >= RESERVATION_BOUND ||
          object.reservations.some((entry) => entry.reservationId === reservationId)) {
        throw new DOMException('Object reservation queue is full or duplicated', 'InvalidStateError')
      }
      const next: ObjectCapacityRecord = { ...object, reservations: [...object.reservations, {
        reservationId, targetLength: input.targetLength, metadataHeadroom: input.metadataHeadroom,
      }] }
      const replacement = updateAccount(account, object, next)
      const inventory = await accounts(transaction, facts.nowMilliseconds)
      const staging = await stagedCapacity(transaction)
      const occupied = sum(inventory, (entry) => entry.occupiedBytes) + staging.verifiedStagedBytes
      const outstanding = sum(inventory, (entry) => entry.id === account.id
        ? replacement.outstandingGrowthBytes + replacement.metadataHeadroomBytes
        : entry.outstandingGrowthBytes + entry.metadataHeadroomBytes) + staging.outstandingBytes
      const usage = facts.currentUsageBytes > occupied ? facts.currentUsageBytes : occupied
      const additionalCapacity = replacement.outstandingGrowthBytes + replacement.metadataHeadroomBytes -
        account.outstandingGrowthBytes - account.metadataHeadroomBytes
      // A lower advisory estimate must not strand already-admitted regions: filling
      // them cannot consume more capacity, and is the useful work under pressure.
      if (additionalCapacity > 0n && facts.estimatedQuotaBytes !== undefined &&
          usage + outstanding + facts.minimumReserveBytes > facts.estimatedQuotaBytes) {
        throw new DOMException('Origin-private object growth exceeds estimated capacity', 'QuotaExceededError')
      }
      transaction.objectStore(OBJECT_STORE).put(next)
      transaction.objectStore(CLAIM_STORE).put(replacement)
    })
  }

  async settleGrowth(fence: ObjectCapacityFence, objectId: string,
    reservationId: string, actualLength?: bigint): Promise<void> {
    if (actualLength !== undefined) requireCapacityLength(actualLength)
    return this.#transact(async transaction => {
      const account = await ownedAccount(transaction, fence)
      const object = await readObject(transaction, fence, objectId, 0n)
      const reservation = object.reservations.find((entry) => entry.reservationId === reservationId)
      if (reservation === undefined) {
        throw new OriginPrivateWorkspaceBudgetOwnershipError()
      }
      const next: ObjectCapacityRecord = {
        ...object,
        occupiedBytes: actualLength ?? object.occupiedBytes,
        reservations: object.reservations.filter((entry) => entry.reservationId !== reservationId),
      }
      transaction.objectStore(OBJECT_STORE).put(next)
      transaction.objectStore(CLAIM_STORE).put(updateAccount(account, object, next))
    })
  }

  async reconcileObject(fence: ObjectCapacityFence, objectId: string, actualLength: bigint): Promise<void> {
    requireCapacityLength(actualLength)
    return this.#transact(async transaction => {
      const account = await ownedAccount(transaction, fence)
      const object = await readObject(transaction, fence, objectId, actualLength)
      if (object.reservations.length > 0) {
        throw new DOMException('Cannot reconcile an object with admitted writes', 'InvalidStateError')
      }
      const next = { ...object, occupiedBytes: actualLength }
      transaction.objectStore(OBJECT_STORE).put(next)
      transaction.objectStore(CLAIM_STORE).put(updateAccount(account, object, next))
    })
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#closed) return
    return this.#transact(async transaction => {
      const store = transaction.objectStore(CLAIM_STORE)
      const account = await readAccount(transaction, id)
      if (account?.token === token) {
        // A crashed/failed write may have consumed its reservation. Retain that conservative
        // occupancy until recovery measures the task; releasing a lease is not deleting bytes.
        store.put({ ...account, token: '', expiresAtMilliseconds: 0,
          occupiedBytes: requireCapacityLength(account.occupiedBytes + account.outstandingGrowthBytes),
          outstandingGrowthBytes: 0n, metadataHeadroomBytes: 0n })
      }
    })
  }

  /** Only namespace cleanup may call this, after proving the owned directory is absent. */
  async forgetDeletedOperation(operationId: string): Promise<void> {
    snapshotIdentity(operationId, 16, 'operation ID')
    return this.#transact(async transaction => {
      transaction.objectStore(CLAIM_STORE).delete(operationId)
      const prefix = operationId + ':'
      transaction.objectStore(OBJECT_STORE).delete(IDBKeyRange.bound(prefix, prefix + '\uffff'))
    })
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
    return this.#transact(async transaction => {
      const store = transaction.objectStore(CLAIM_STORE)
      const inventory = await accounts(transaction, facts.nowMilliseconds)
      const existing = inventory.find((entry) => entry.id === record.id)
      if ((mode === 'readmit' && (existing?.token !== record.token ||
            existing.expiresAtMilliseconds <= facts.nowMilliseconds)) ||
          (mode === 'claim' && existing !== undefined && existing.token !== record.token &&
            existing.expiresAtMilliseconds > facts.nowMilliseconds)) {
        throw new OriginPrivateWorkspaceBudgetOwnershipError()
      }
      if (existing !== undefined && existing.budgetDigest !== record.budgetDigest) {
        throw new OriginPrivateWorkspaceBudgetOwnershipError()
      }
      const current = mode === 'readmit' && existing !== undefined ? existing : {
        ...record, occupiedBytes: facts.verifiedAlreadyOwnedBytes, outstandingGrowthBytes: 0n,
        metadataHeadroomBytes: budget.durableMetadataBytes,
      }
      const others = inventory.filter((entry) => entry.id !== record.id)
      const staging = await stagedCapacity(transaction)
      const capacity: WorkspaceCapacitySnapshot = {
        ...(facts.estimatedQuotaBytes === undefined ? {} : { estimatedQuotaBytes: facts.estimatedQuotaBytes }),
        currentUsageBytes: maximum(facts.currentUsageBytes,
          sum(others, (entry) => entry.occupiedBytes) + current.occupiedBytes + staging.verifiedStagedBytes),
        minimumReserveBytes: facts.minimumReserveBytes,
        verifiedAlreadyOwnedBytes: current.occupiedBytes,
        outstandingGrowthBytes: current.outstandingGrowthBytes + staging.outstandingBytes +
          sum(others, (entry) => entry.outstandingGrowthBytes),
        metadataHeadroomBytes: current.metadataHeadroomBytes - budget.durableMetadataBytes +
          sum(others, (entry) => entry.metadataHeadroomBytes),
      }
      const admission = admitWorkspaceBudget(budget, capacity)
      if (admission.kind === 'accepted') store.put(workspaceCapacityAccount({ ...current, ...record }))
      return admission.kind === 'accepted'
        ? { kind: 'accepted', capacity, admission } : { kind: 'rejected', capacity, admission }
    })
  }

  #transact<T>(update: (transaction: IDBTransaction) => Promise<T>): Promise<T> {
    if (this.#closed) throw new DOMException('Workspace budget authority is closed', 'InvalidStateError')
    return capacityTransaction(this.#database, update)
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
  const account = await readAccount(transaction, fence.operationId)
  if (account === undefined || account.token !== fence.token ||
      account.expiresAtMilliseconds <= fence.nowMilliseconds) {
    throw new OriginPrivateWorkspaceBudgetOwnershipError()
  }
  return account
}

async function readObject(transaction: IDBTransaction, fence: ObjectCapacityFence,
  objectId: string, initialLength: bigint): Promise<ObjectCapacityRecord> {
  snapshotIdentity(objectId, 32, 'owned object ID')
  const id = fence.operationId + ':' + objectId
  const value: unknown = await requestResult(transaction.objectStore(OBJECT_STORE).get(id))
  const object = value === undefined ? undefined : workspaceObjectCapacity(value)
  return object?.token === fence.token ? object : {
    id, operationId: fence.operationId, objectId, token: fence.token,
    occupiedBytes: initialLength, reservations: [],
  }
}

async function accounts(transaction: IDBTransaction, nowMilliseconds: number): Promise<Account[]> {
  const values = await requestResult<unknown[]>(transaction.objectStore(CLAIM_STORE).getAll(undefined, CLAIM_BOUND + 1))
  if (values.length > CLAIM_BOUND) throw new DOMException('Workspace task inventory exceeds its bound', 'QuotaExceededError')
  return values.map(workspaceCapacityAccount).map((account) => {
    if (account.expiresAtMilliseconds > nowMilliseconds || account.token === '') return account
    const expired = { ...account, token: '', expiresAtMilliseconds: 0,
      occupiedBytes: requireCapacityLength(account.occupiedBytes + account.outstandingGrowthBytes),
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

async function stagedCapacity(transaction: IDBTransaction) {
  const records = await requestResult<unknown[]>(transaction.objectStore(STAGING_FILE_STORE)
    .getAll(undefined, CLAIM_BOUND + 1))
  if (records.length > CLAIM_BOUND) throw new DOMException('Staging inventory exceeds its bound', 'QuotaExceededError')
  return stagingCapacityTotals(records.map(stagingFileCapacity))
}

async function readAccount(transaction: IDBTransaction, id: string): Promise<Account | undefined> {
  const value: unknown = await requestResult(transaction.objectStore(CLAIM_STORE).get(id))
  return value === undefined ? undefined : workspaceCapacityAccount(value)
}
