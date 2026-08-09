import type { BrowserReceiveOperationLease } from '../browser/session-lease'
import {
  DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
  DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
  MINIMUM_OPFS_QUOTA_RESERVE,
  type WorkspaceBudgetAdmission,
  type WorkspaceBudgetV1,
  type WorkspaceCapacitySnapshot,
} from '../workspace/budget'
import { snapshotIdentity } from '../workspace/canonical'
import type {
  WorkspaceBudgetAuthority,
  WorkspaceBudgetClaim,
  WorkspaceBudgetClaimResult,
} from '../workspace/stages'
import {
  IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority,
  type OriginPrivateWorkspaceBudgetLeaseAuthority,
  type WorkspaceBudgetCapacityFacts,
  type WorkspaceBudgetLeaseDecision,
  type WorkspaceBudgetLeaseRecord,
} from './admission-authority'

export const DEFAULT_OPFS_WORKSPACE_CLAIM_LEASE_MILLISECONDS = 120_000
export const DEFAULT_OPFS_WORKSPACE_CLAIM_HEARTBEAT_MILLISECONDS = 30_000

export interface OriginPrivateStorageEstimate {
  readonly usage?: number
  readonly quota?: number
}

export interface OriginPrivateWorkspaceBudgetOptions {
  readonly estimate: () => Promise<OriginPrivateStorageEstimate>
  readonly verifiedAlreadyOwnedBytes?: () => Promise<bigint>
  readonly jobLimitBytes?: bigint
  readonly processLimitBytes?: bigint
  readonly minimumReserveBytes?: bigint
  readonly authority?: OriginPrivateWorkspaceBudgetLeaseAuthority
  readonly databaseName?: string
  readonly now?: () => number
  readonly leaseMilliseconds?: number
  readonly heartbeatMilliseconds?: number
  readonly randomToken?: () => string
}

export interface OriginPrivateWorkspaceBudgetClaim extends WorkspaceBudgetClaim {
  readmit(verifiedAlreadyOwnedBytes: bigint): Promise<WorkspaceBudgetAdmission>
}

export class OriginPrivateWorkspaceBudgetAuthority implements WorkspaceBudgetAuthority {
  readonly #operationId: string
  readonly #authority: OriginPrivateWorkspaceBudgetLeaseAuthority
  readonly #ownsAuthority: boolean
  readonly #estimate: OriginPrivateWorkspaceBudgetOptions['estimate']
  readonly #verifiedAlreadyOwnedBytes: () => Promise<bigint>
  readonly #jobLimitBytes: bigint
  readonly #processLimitBytes: bigint
  readonly #minimumReserveBytes: bigint
  readonly #now: () => number
  readonly #leaseMilliseconds: number
  readonly #heartbeatMilliseconds: number
  readonly #token: string
  #activeClaim: ActiveOriginPrivateWorkspaceBudgetClaim | undefined
  #settled = false

  private constructor(
    operationId: string,
    authority: OriginPrivateWorkspaceBudgetLeaseAuthority,
    ownsAuthority: boolean,
    options: OriginPrivateWorkspaceBudgetOptions,
  ) {
    this.#operationId = snapshotIdentity(operationId, 16, 'operation ID')
    this.#authority = authority
    this.#ownsAuthority = ownsAuthority
    this.#estimate = options.estimate
    this.#verifiedAlreadyOwnedBytes = options.verifiedAlreadyOwnedBytes ?? (async () => 0n)
    this.#jobLimitBytes = checkedPositiveU64(
      options.jobLimitBytes ?? DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
      'workspace job limit',
    )
    this.#processLimitBytes = checkedPositiveU64(
      options.processLimitBytes ?? DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
      'workspace process limit',
    )
    this.#minimumReserveBytes = checkedU64(
      options.minimumReserveBytes ?? MINIMUM_OPFS_QUOTA_RESERVE,
      'workspace quota reserve',
    )
    this.#now = options.now ?? Date.now
    this.#leaseMilliseconds = positiveSafeInteger(
      options.leaseMilliseconds ?? DEFAULT_OPFS_WORKSPACE_CLAIM_LEASE_MILLISECONDS,
      'workspace claim lease',
    )
    this.#heartbeatMilliseconds = positiveSafeInteger(
      options.heartbeatMilliseconds ?? DEFAULT_OPFS_WORKSPACE_CLAIM_HEARTBEAT_MILLISECONDS,
      'workspace claim heartbeat',
    )
    if (this.#heartbeatMilliseconds >= this.#leaseMilliseconds) {
      throw new RangeError('workspace claim heartbeat must be shorter than its lease')
    }
    this.#token = options.randomToken?.() ?? crypto.randomUUID()
    if (this.#token.length === 0) throw new TypeError('workspace claim token is empty')
  }

  static async open(
    operationId: string,
    options: OriginPrivateWorkspaceBudgetOptions,
  ): Promise<OriginPrivateWorkspaceBudgetAuthority> {
    const ownsAuthority = options.authority === undefined
    const authority = options.authority ??
      await IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority.open(options.databaseName)
    try {
      return new OriginPrivateWorkspaceBudgetAuthority(
        operationId,
        authority,
        ownsAuthority,
        options,
      )
    } catch (error) {
      if (ownsAuthority) authority.close()
      throw error
    }
  }

  claim(budget: WorkspaceBudgetV1): Promise<WorkspaceBudgetClaimResult> {
    return this.#acquireClaim(budget, 'claim')
  }

  reclaim(
    budget: WorkspaceBudgetV1,
    operationLease: Pick<BrowserReceiveOperationLease, 'operationId' | 'leaseId'>,
  ): Promise<WorkspaceBudgetClaimResult> {
    if (operationLease.operationId !== this.#operationId) {
      throw new TypeError('workspace budget reclaim requires the active operation lease')
    }
    snapshotIdentity(operationLease.leaseId, 16, 'lease ID')
    return this.#acquireClaim(budget, 'reclaim')
  }

  async #acquireClaim(
    budget: WorkspaceBudgetV1,
    mode: 'claim' | 'reclaim',
  ): Promise<WorkspaceBudgetClaimResult> {
    if (this.#settled || this.#activeClaim !== undefined) {
      throw new DOMException('Workspace budget authority already settled', 'InvalidStateError')
    }
    try {
      if (budget.operationId !== this.#operationId) {
        throw new TypeError('workspace budget belongs to another operation')
      }
      const verifiedAlreadyOwnedBytes = checkedU64(
        await this.#verifiedAlreadyOwnedBytes(),
        'verified already-owned bytes',
      )
      const record = this.#record(budget)
      const facts = await this.#capacityFacts(verifiedAlreadyOwnedBytes)
      const decision: WorkspaceBudgetLeaseDecision = mode === 'claim'
        ? await this.#authority.claim(record, budget, facts)
        : await this.#authority.reclaim(record, budget, facts)
      if (decision.kind === 'rejected') {
        this.#settled = true
        if (this.#ownsAuthority) this.#authority.close()
        return Object.freeze({
          kind: 'rejected',
          capacity: decision.capacity,
          admission: decision.admission,
        })
      }
      const claim = new ActiveOriginPrivateWorkspaceBudgetClaim({
        budget,
        initial: decision,
        record: () => this.#record(budget),
        capacityFacts: (ownedBytes) => this.#capacityFacts(ownedBytes),
        authority: this.#authority,
        heartbeatMilliseconds: this.#heartbeatMilliseconds,
        now: this.#now,
        token: this.#token,
        onReleased: () => {
          this.#settled = true
          this.#activeClaim = undefined
          if (this.#ownsAuthority) this.#authority.close()
        },
      })
      this.#activeClaim = claim
      claim.startHeartbeat()
      return Object.freeze({ kind: 'accepted', claim })
    } catch (error) {
      this.#settled = true
      if (this.#ownsAuthority) this.#authority.close()
      throw error
    }
  }

  #record(budget: WorkspaceBudgetV1): WorkspaceBudgetLeaseRecord {
    return Object.freeze({
      id: this.#operationId,
      operationId: this.#operationId,
      token: this.#token,
      budgetDigest: budget.digest,
      peakOwnedBytes: budget.peakOwnedBytes,
      expiresAtMilliseconds: checkedDeadline(this.#clock(), this.#leaseMilliseconds),
    })
  }

  async #capacityFacts(verifiedAlreadyOwnedBytes: bigint): Promise<WorkspaceBudgetCapacityFacts> {
    const estimate = await this.#estimate()
    return Object.freeze({
      jobLimitBytes: this.#jobLimitBytes,
      processLimitBytes: this.#processLimitBytes,
      estimatedQuotaBytes: storageEstimateBytes(estimate.quota, 'estimated quota'),
      currentUsageBytes: storageEstimateBytes(estimate.usage, 'current quota usage'),
      minimumReserveBytes: this.#minimumReserveBytes,
      verifiedAlreadyOwnedBytes: checkedU64(
        verifiedAlreadyOwnedBytes,
        'verified already-owned bytes',
      ),
      nowMilliseconds: this.#clock(),
    })
  }

  #clock(): number {
    const value = this.#now()
    if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('workspace claim clock is invalid')
    return value
  }
}

class ActiveOriginPrivateWorkspaceBudgetClaim implements OriginPrivateWorkspaceBudgetClaim {
  readonly budgetDigest: string
  readonly capacity: WorkspaceCapacitySnapshot
  readonly admission: Extract<WorkspaceBudgetAdmission, { kind: 'accepted' }>
  readonly #budget: WorkspaceBudgetV1
  readonly #record: () => WorkspaceBudgetLeaseRecord
  readonly #capacityFacts: (ownedBytes: bigint) => Promise<WorkspaceBudgetCapacityFacts>
  readonly #authority: OriginPrivateWorkspaceBudgetLeaseAuthority
  readonly #heartbeatMilliseconds: number
  readonly #now: () => number
  readonly #token: string
  readonly #onReleased: () => void
  #heartbeatTimer: ReturnType<typeof setInterval> | undefined
  #failure: unknown
  #releasePromise: Promise<void> | undefined

  constructor(input: {
    readonly budget: WorkspaceBudgetV1
    readonly initial: Extract<WorkspaceBudgetLeaseDecision, { kind: 'accepted' }>
    readonly record: () => WorkspaceBudgetLeaseRecord
    readonly capacityFacts: (ownedBytes: bigint) => Promise<WorkspaceBudgetCapacityFacts>
    readonly authority: OriginPrivateWorkspaceBudgetLeaseAuthority
    readonly heartbeatMilliseconds: number
    readonly now: () => number
    readonly token: string
    readonly onReleased: () => void
  }) {
    this.#budget = input.budget
    this.budgetDigest = input.budget.digest
    this.capacity = input.initial.capacity
    this.admission = input.initial.admission
    this.#record = input.record
    this.#capacityFacts = input.capacityFacts
    this.#authority = input.authority
    this.#heartbeatMilliseconds = input.heartbeatMilliseconds
    this.#now = input.now
    this.#token = input.token
    this.#onReleased = input.onReleased
  }

  startHeartbeat(): void {
    this.#heartbeatTimer = setInterval(() => {
      const record = this.#record()
      const heartbeat = this.#authority.heartbeat({
        id: record.id,
        token: this.#token,
        expiresAtMilliseconds: record.expiresAtMilliseconds,
        nowMilliseconds: checkedClock(this.#now()),
      })
      heartbeat.catch((error: unknown) => { this.#failure = error })
    }, this.#heartbeatMilliseconds)
  }

  async readmit(verifiedAlreadyOwnedBytes: bigint): Promise<WorkspaceBudgetAdmission> {
    this.#assertHealthy()
    const decision = await this.#authority.readmit(
      this.#record(),
      this.#budget,
      await this.#capacityFacts(checkedU64(verifiedAlreadyOwnedBytes, 'verified already-owned bytes')),
    )
    return decision.admission
  }

  release(): Promise<void> {
    if (this.#releasePromise !== undefined) return this.#releasePromise
    if (this.#heartbeatTimer !== undefined) clearInterval(this.#heartbeatTimer)
    const operation = this.#authority.release(this.#budget.operationId, this.#token)
      .finally(this.#onReleased)
    this.#releasePromise = operation
    return operation
  }

  #assertHealthy(): void {
    if (this.#releasePromise !== undefined) {
      throw new DOMException('Workspace budget claim is released', 'InvalidStateError')
    }
    if (this.#failure !== undefined) {
      throw new Error('Workspace budget claim heartbeat failed', {
        cause: this.#failure,
      })
    }
  }
}

function storageEstimateBytes(value: number | undefined, label: string): bigint {
  if (value === undefined) return 0n
  if (!Number.isSafeInteger(value) || value < 0) throw new TypeError(`${label} is invalid`)
  return BigInt(value)
}

function checkedDeadline(now: number, duration: number): number {
  if (now > Number.MAX_SAFE_INTEGER - duration) {
    throw new TypeError('workspace claim deadline overflows the clock')
  }
  return now + duration
}

function checkedClock(value: number): number {
  if (!Number.isSafeInteger(value) || value < 0) throw new TypeError('workspace claim clock is invalid')
  return value
}

function positiveSafeInteger(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) throw new TypeError(`${label} is invalid`)
  return value
}

function checkedPositiveU64(value: bigint, label: string): bigint {
  const result = checkedU64(value, label)
  if (result === 0n) throw new TypeError(`${label} must be positive`)
  return result
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}
