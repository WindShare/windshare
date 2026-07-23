import { MAXIMUM_OPEN_OUTPUT_FILES } from '../../transfer/output-session'
import { outputPathKey } from '../persistence/journal'
import {
  type AdmissionAggregateLimits,
  type AdmissionLeaseRecord,
  IndexedDbOriginPrivateAdmissionAuthority,
  type OriginPrivateAdmissionAuthority,
} from './admission-authority'

const GIBIBYTE = 1024n * 1024n * 1024n
const MEBIBYTE = 1024n * 1024n

export const DEFAULT_OPFS_JOB_STAGING_LIMIT = 8n * GIBIBYTE
export const DEFAULT_OPFS_PROCESS_STAGING_LIMIT = 16n * GIBIBYTE
export const MINIMUM_OPFS_QUOTA_RESERVE = 512n * MEBIBYTE
export const DEFAULT_OPFS_ADMISSION_LEASE_MILLISECONDS = 120_000
export const DEFAULT_OPFS_ADMISSION_HEARTBEAT_MILLISECONDS = 30_000
const QUOTA_RESERVE_DIVISOR = 10n

export interface OriginPrivateStorageEstimate {
  readonly usage?: number
  readonly quota?: number
}

export interface OriginPrivateQuotaOptions {
  readonly estimate: () => Promise<OriginPrivateStorageEstimate>
  readonly jobLimit?: bigint
  readonly processLimit?: bigint
  readonly authority?: OriginPrivateAdmissionAuthority
  readonly admissionDatabaseName?: string
  readonly now?: () => number
  readonly leaseMilliseconds?: number
  readonly heartbeatMilliseconds?: number
  readonly randomToken?: () => string
}

export interface OriginPrivateStagingTotals {
  readonly logicalBytes: bigint
  readonly additionalBytes: bigint
}

export interface OriginPrivateFileFootprint {
  readonly logicalBytes: bigint
  readonly coveredBytes: bigint
}

export interface OriginPrivateAdmissionSnapshot extends OriginPrivateStagingTotals {
  readonly activeReservations: number
}

export interface PreparedOriginPrivateFileCommit {
  /** Non-throwing publication acknowledgement after the file commit succeeds. */
  publish(): void
  rollback(): Promise<void>
}

export class OriginPrivateQuotaError extends Error {
  readonly kind = 'quota'

  constructor(message: string, cause?: unknown) {
    super(message, { cause })
    this.name = 'OriginPrivateQuotaError'
  }
}

interface ReservationValue {
  readonly size: bigint
  readonly additional: bigint
}

interface ActiveReservation {
  readonly previous: ReservationValue
  readonly current: ReservationValue
}

export class OriginPrivateStagingAdmission {
  readonly #sessionKey: string
  readonly #token: string
  readonly #jobLimit: bigint
  readonly #processLimit: bigint
  readonly #authority: OriginPrivateAdmissionAuthority
  readonly #estimate: OriginPrivateQuotaOptions['estimate']
  readonly #now: () => number
  readonly #leaseMilliseconds: number
  readonly #active = new Map<string, ActiveReservation>()
  #jobBytes: bigint
  #additionalBytes: bigint
  #tail: Promise<void> = Promise.resolve()
  #heartbeatTimer: ReturnType<typeof setInterval> | undefined
  #releasePromise: Promise<void> | undefined
  #failure: unknown
  #released = false

  private constructor(
    sessionKey: string,
    totals: OriginPrivateStagingTotals,
    authority: OriginPrivateAdmissionAuthority,
    options: OriginPrivateQuotaOptions,
  ) {
    this.#sessionKey = sessionKey
    this.#token = options.randomToken?.() ?? crypto.randomUUID()
    this.#jobLimit = options.jobLimit ?? DEFAULT_OPFS_JOB_STAGING_LIMIT
    this.#processLimit = options.processLimit ?? DEFAULT_OPFS_PROCESS_STAGING_LIMIT
    this.#authority = authority
    this.#estimate = options.estimate
    this.#now = options.now ?? Date.now
    this.#leaseMilliseconds = options.leaseMilliseconds ?? DEFAULT_OPFS_ADMISSION_LEASE_MILLISECONDS
    requirePositiveLimit(this.#jobLimit, 'OPFS job staging')
    requirePositiveLimit(this.#processLimit, 'OPFS process staging')
    requirePositiveSafeInteger(this.#leaseMilliseconds, 'OPFS admission lease')
    requireTotals(totals)
    this.#jobBytes = totals.logicalBytes
    this.#additionalBytes = totals.additionalBytes
  }

  static async open(
    sessionKey: string,
    totals: OriginPrivateStagingTotals,
    options: OriginPrivateQuotaOptions,
  ): Promise<OriginPrivateStagingAdmission> {
    const leaseMilliseconds = options.leaseMilliseconds ?? DEFAULT_OPFS_ADMISSION_LEASE_MILLISECONDS
    const heartbeatMilliseconds =
      options.heartbeatMilliseconds ?? DEFAULT_OPFS_ADMISSION_HEARTBEAT_MILLISECONDS
    requirePositiveSafeInteger(leaseMilliseconds, 'OPFS admission lease')
    requirePositiveSafeInteger(heartbeatMilliseconds, 'OPFS admission heartbeat')
    if (heartbeatMilliseconds >= leaseMilliseconds) {
      throw new RangeError('OPFS admission heartbeat must be shorter than its lease')
    }
    const authority = options.authority ??
      await IndexedDbOriginPrivateAdmissionAuthority.open(options.admissionDatabaseName)
    let admission: OriginPrivateStagingAdmission
    try {
      admission = new OriginPrivateStagingAdmission(sessionKey, totals, authority, options)
    } catch (error) {
      authority.close()
      throw error
    }
    let claimed = false
    try {
      await authority.claim(admission.#record(), await admission.#limits())
      claimed = true
      admission.#startHeartbeat(heartbeatMilliseconds)
      return admission
    } catch (error) {
      if (claimed) {
        await authority.release(admission.#sessionKey, admission.#token).catch(() => undefined)
      }
      authority.close()
      throw translateQuotaError(error)
    }
  }

  reserve(
    path: readonly string[],
    exactSize: bigint,
    previousFootprint: OriginPrivateFileFootprint,
  ): Promise<() => Promise<void>> {
    return this.#serialize(async () => {
      this.#requireOpen()
      if (exactSize < 0n) throw new RangeError('OPFS staged file size must not be negative')
      requireFootprint(previousFootprint)
      const key = outputPathKey(path)
      if (this.#active.has(key)) throw new Error('OPFS staged file already has an active reservation')
      if (this.#active.size >= MAXIMUM_OPEN_OUTPUT_FILES) {
        throw new OriginPrivateQuotaError('OPFS staging has reached its active reservation limit')
      }
      const previous = Object.freeze({
        size: previousFootprint.logicalBytes,
        additional: previousFootprint.logicalBytes - previousFootprint.coveredBytes,
      })
      const current = Object.freeze({
        size: exactSize,
        additional: maximum(0n, exactSize - previousFootprint.coveredBytes),
      })
      await this.#transition(previous, current)
      const reservation = Object.freeze({ previous, current })
      this.#active.set(key, reservation)
      let rolledBack = false
      return async () => {
        if (rolledBack || this.#released) return
        rolledBack = true
        await this.#serialize(async () => {
          this.#requireOpen()
          if (this.#active.get(key) !== reservation) return
          await this.#transition(current, previous)
          this.#active.delete(key)
        })
      }
    })
  }

  updateFile(path: readonly string[], coveredBytes: bigint): Promise<void> {
    return this.#serialize(async () => {
      this.#requireOpen()
      const key = outputPathKey(path)
      const reservation = this.#active.get(key)
      if (reservation === undefined) throw new Error('OPFS staged file reservation is not active')
      if (coveredBytes < 0n || coveredBytes > reservation.current.size) {
        throw new RangeError('OPFS durable coverage exceeds the staged file')
      }
      const current = Object.freeze({
        size: reservation.current.size,
        additional: reservation.current.size - coveredBytes,
      })
      await this.#transition(reservation.current, current)
      this.#active.set(key, Object.freeze({ previous: reservation.previous, current }))
    })
  }

  prepareFileCommit(path: readonly string[]): Promise<PreparedOriginPrivateFileCommit> {
    return this.#serialize(async () => {
      this.#requireOpen()
      const key = outputPathKey(path)
      const reservation = this.#active.get(key)
      if (reservation === undefined) throw new Error('OPFS staged file reservation is not active')
      const committed = Object.freeze({
        size: reservation.current.size,
        additional: 0n,
      })
      if (reservation.current.additional !== 0n) {
        await this.#transition(reservation.current, committed)
      }
      const prepared = Object.freeze({ previous: reservation.previous, current: committed })
      this.#active.set(key, prepared)
      let settled = false
      return Object.freeze({
        publish: () => {
          if (settled) return
          settled = true
          // All fallible quota and lease authority checks happened during prepare.
          // Publication acknowledgement must not reject after the file is durable.
          if (this.#active.get(key) === prepared) this.#active.delete(key)
        },
        rollback: async () => {
          if (settled || this.#released) return
          settled = true
          await this.#serialize(async () => {
            if (this.#released) return
            this.#requireOpen()
            if (this.#active.get(key) !== prepared) return
            await this.#transition(prepared.current, reservation.current)
            this.#active.set(key, reservation)
          })
        },
      })
    })
  }

  releaseFile(path: readonly string[]): Promise<void> {
    return this.#serialize(async () => {
      if (this.#released) return
      this.#requireOpen()
      const key = outputPathKey(path)
      const reservation = this.#active.get(key)
      if (reservation === undefined) return
      await this.#transition(reservation.current, { size: 0n, additional: 0n })
      this.#active.delete(key)
    })
  }

  snapshot(): OriginPrivateAdmissionSnapshot {
    return Object.freeze({
      logicalBytes: this.#jobBytes,
      additionalBytes: this.#additionalBytes,
      activeReservations: this.#active.size,
    })
  }

  async release(): Promise<void> {
    if (this.#released) return
    if (this.#releasePromise !== undefined) return this.#releasePromise
    const operation = this.#releaseAuthority()
    this.#releasePromise = operation
    try {
      await operation
    } finally {
      if (this.#releasePromise === operation) this.#releasePromise = undefined
    }
  }

  async #releaseAuthority(): Promise<void> {
    if (this.#heartbeatTimer !== undefined) clearInterval(this.#heartbeatTimer)
    this.#heartbeatTimer = undefined
    try {
      await this.#tail
      await this.#authority.release(this.#sessionKey, this.#token)
    } catch (error) {
      this.#failure = error
      throw error
    }
    this.#released = true
    try {
      this.#active.clear()
      this.#jobBytes = 0n
      this.#additionalBytes = 0n
    } finally {
      this.#authority.close()
    }
  }

  async #transition(previous: ReservationValue, current: ReservationValue): Promise<void> {
    const nextJobBytes = this.#jobBytes - previous.size + current.size
    const nextAdditional = this.#additionalBytes - previous.additional + current.additional
    if (nextJobBytes < 0n || nextAdditional < 0n) {
      throw new Error('OPFS staging scalar authority is inconsistent')
    }
    const record = this.#record(nextJobBytes, nextAdditional)
    try {
      await this.#authority.update(record, await this.#limits())
    } catch (error) {
      throw translateQuotaError(error)
    }
    this.#jobBytes = nextJobBytes
    this.#additionalBytes = nextAdditional
  }

  #record(
    logicalBytes = this.#jobBytes,
    additionalBytes = this.#additionalBytes,
  ): AdmissionLeaseRecord {
    const now = this.#now()
    requireSafeTime(now)
    const expiresAtMilliseconds = now + this.#leaseMilliseconds
    requireSafeTime(expiresAtMilliseconds)
    return Object.freeze({
      id: this.#sessionKey,
      token: this.#token,
      logicalBytes,
      additionalBytes,
      expiresAtMilliseconds,
    })
  }

  async #limits(): Promise<AdmissionAggregateLimits> {
    const estimate = await this.#estimate()
    const quota = storageInteger(estimate.quota, 'quota')
    const usage = storageInteger(estimate.usage ?? 0, 'usage')
    const nowMilliseconds = this.#now()
    requireSafeTime(nowMilliseconds)
    return Object.freeze({
      jobLimit: this.#jobLimit,
      processLimit: this.#processLimit,
      quota,
      usage,
      reserve: maximum(MINIMUM_OPFS_QUOTA_RESERVE, quota / QUOTA_RESERVE_DIVISOR),
      nowMilliseconds,
    })
  }

  #startHeartbeat(intervalMilliseconds: number): void {
    this.#heartbeatTimer = setInterval(() => {
      this.#heartbeat().catch((error: unknown) => { this.#failure = error })
    }, intervalMilliseconds)
  }

  async #heartbeat(): Promise<void> {
    if (this.#released) return
    const record = this.#record()
    const nowMilliseconds = this.#now()
    requireSafeTime(nowMilliseconds)
    await this.#authority.heartbeat(
      record.id,
      record.token,
      record.expiresAtMilliseconds,
      nowMilliseconds,
    )
  }

  #serialize<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation, operation)
    this.#tail = result.then(() => undefined, () => undefined)
    return result
  }

  #requireOpen(): void {
    if (this.#released) throw new Error('OPFS staging admission is released')
    if (this.#releasePromise !== undefined) throw new Error('OPFS staging admission is releasing')
    if (this.#failure !== undefined) {
      throw new Error('OPFS admission lease heartbeat failed', { cause: this.#failure })
    }
  }
}

function requireTotals(totals: OriginPrivateStagingTotals): void {
  if (totals.logicalBytes < 0n || totals.additionalBytes < 0n ||
      totals.additionalBytes > totals.logicalBytes) {
    throw new RangeError('OPFS staging totals are invalid')
  }
}

function requireFootprint(footprint: OriginPrivateFileFootprint): void {
  if (footprint.logicalBytes < 0n || footprint.coveredBytes < 0n ||
      footprint.coveredBytes > footprint.logicalBytes) {
    throw new RangeError('OPFS staged file footprint is invalid')
  }
}

function storageInteger(value: number | undefined, label: string): bigint {
  if (value === undefined || !Number.isFinite(value) || value < 0) {
    throw new OriginPrivateQuotaError(`Browser storage ${label} is unavailable`)
  }
  return BigInt(Math.floor(value))
}

function requirePositiveLimit(value: bigint, label: string): void {
  if (value <= 0n) throw new RangeError(`${label} limit must be positive`)
}

function requirePositiveSafeInteger(value: number, label: string): void {
  if (!Number.isSafeInteger(value) || value <= 0) throw new RangeError(`${label} must be positive`)
}

function requireSafeTime(value: number): void {
  if (!Number.isSafeInteger(value) || value < 0) throw new RangeError('OPFS admission clock is invalid')
}

function translateQuotaError(error: unknown): unknown {
  if (error instanceof DOMException && error.name === 'QuotaExceededError') {
    return new OriginPrivateQuotaError(error.message, error)
  }
  return error
}

function maximum(left: bigint, right: bigint): bigint {
  return left > right ? left : right
}
