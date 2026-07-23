const ADMISSION_DATABASE_NAME = 'windshare-output-admission'
const ADMISSION_DATABASE_VERSION = 1
const ADMISSION_STORE = 'staging-reservations'

export interface AdmissionLeaseRecord {
  readonly id: string
  readonly token: string
  readonly logicalBytes: bigint
  readonly additionalBytes: bigint
  readonly expiresAtMilliseconds: number
}

export interface AdmissionAggregateLimits {
  readonly jobLimit: bigint
  readonly processLimit: bigint
  readonly quota: bigint
  readonly usage: bigint
  readonly reserve: bigint
  readonly nowMilliseconds: number
}

export interface OriginPrivateAdmissionAuthority {
  claim(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void>
  update(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void>
  heartbeat(
    id: string,
    token: string,
    expiresAtMilliseconds: number,
    nowMilliseconds: number,
  ): Promise<void>
  release(id: string, token: string): Promise<void>
  close(): void
}

export class IndexedDbOriginPrivateAdmissionAuthority implements OriginPrivateAdmissionAuthority {
  readonly #database: IDBDatabase
  #closed = false

  private constructor(database: IDBDatabase) {
    this.#database = database
    database.addEventListener('versionchange', () => {
      this.#closed = true
      database.close()
    })
  }

  static async open(
    databaseName = ADMISSION_DATABASE_NAME,
  ): Promise<IndexedDbOriginPrivateAdmissionAuthority> {
    if (databaseName.length === 0) throw new TypeError('OPFS admission database name is empty')
    return new IndexedDbOriginPrivateAdmissionAuthority(await openDatabase(databaseName))
  }

  claim(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    return this.#mutate(record.id, undefined, record, limits)
  }

  update(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    return this.#mutate(record.id, record.token, record, limits)
  }

  async heartbeat(
    id: string,
    token: string,
    expiresAtMilliseconds: number,
    nowMilliseconds: number,
  ): Promise<void> {
    this.#requireOpen()
    if (!Number.isSafeInteger(expiresAtMilliseconds) ||
        !Number.isSafeInteger(nowMilliseconds) ||
        nowMilliseconds < 0 || expiresAtMilliseconds <= nowMilliseconds) {
      throw new TypeError('OPFS admission heartbeat time is invalid')
    }
    const transaction = this.#database.transaction(ADMISSION_STORE, 'readwrite')
    const store = transaction.objectStore(ADMISSION_STORE)
    const existing = await requestResult<AdmissionLeaseRecord | undefined>(store.get(id))
    if (existing !== undefined) validateRecord(existing)
    if (existing === undefined || existing.token !== token ||
        existing.expiresAtMilliseconds <= nowMilliseconds) {
      transaction.abort()
      throw new Error('OPFS admission lease ownership changed')
    }
    store.put({ ...existing, expiresAtMilliseconds })
    await transactionCompletion(transaction)
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#closed) return
    const transaction = this.#database.transaction(ADMISSION_STORE, 'readwrite')
    const store = transaction.objectStore(ADMISSION_STORE)
    const existing = await requestResult<AdmissionLeaseRecord | undefined>(store.get(id))
    if (existing?.token === token) store.delete(id)
    await transactionCompletion(transaction)
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#database.close()
  }

  async #mutate(
    id: string,
    expectedToken: string | undefined,
    desired: AdmissionLeaseRecord,
    limits: AdmissionAggregateLimits,
  ): Promise<void> {
    this.#requireOpen()
    validateRecord(desired)
    validateLimits(limits)
    if (desired.expiresAtMilliseconds <= limits.nowMilliseconds) {
      throw new TypeError('OPFS admission lease must expire in the future')
    }
    const transaction = this.#database.transaction(ADMISSION_STORE, 'readwrite')
    const store = transaction.objectStore(ADMISSION_STORE)
    const aggregate = await aggregateLiveReservations(store, limits.nowMilliseconds, id)
    const existing = aggregate.byId
    if (expectedToken !== undefined &&
        (existing === undefined || existing.token !== expectedToken)) {
      transaction.abort()
      throw new Error('OPFS admission lease ownership changed')
    }
    // The session-level Web Lock is the exclusive same-ID authority. Replacing
    // that ID here lets a reload recover immediately while the new token fences
    // any suspended realm that later resumes.
    const logicalBytes = aggregate.logicalBytes - (existing?.logicalBytes ?? 0n) + desired.logicalBytes
    const additionalBytes =
      aggregate.additionalBytes - (existing?.additionalBytes ?? 0n) + desired.additionalBytes
    if (desired.logicalBytes > limits.jobLimit) {
      transaction.abort()
      throw quotaError('OPFS staging exceeds the per-job limit')
    }
    if (logicalBytes > limits.processLimit) {
      transaction.abort()
      throw quotaError('OPFS staging exceeds the cross-context process limit')
    }
    if (limits.usage + additionalBytes + limits.reserve > limits.quota) {
      transaction.abort()
      throw quotaError('OPFS staging would consume the shared browser quota reserve')
    }
    store.put(desired)
    await transactionCompletion(transaction)
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('OPFS admission authority is closed or version-obsolete')
  }
}

interface ReservationAggregate {
  readonly logicalBytes: bigint
  readonly additionalBytes: bigint
  readonly byId?: AdmissionLeaseRecord
}

function aggregateLiveReservations(
  store: IDBObjectStore,
  nowMilliseconds: number,
  targetId: string,
): Promise<ReservationAggregate> {
  return new Promise<ReservationAggregate>((resolve, reject) => {
    let logicalBytes = 0n
    let additionalBytes = 0n
    let byId: AdmissionLeaseRecord | undefined
    const request = store.openCursor()
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null) {
        resolve({
          logicalBytes,
          additionalBytes,
          ...(byId === undefined ? {} : { byId }),
        })
        return
      }
      const record = cursor.value as AdmissionLeaseRecord
      try {
        validateRecord(record)
      } catch (error) {
        reject(error)
        return
      }
      if (record.expiresAtMilliseconds <= nowMilliseconds) {
        cursor.delete()
      } else {
        logicalBytes += record.logicalBytes
        additionalBytes += record.additionalBytes
        if (record.id === targetId) byId = record
      }
      cursor.continue()
    })
  })
}

function validateRecord(record: AdmissionLeaseRecord): void {
  if (record.id.length === 0 || record.token.length === 0 ||
      record.logicalBytes < 0n || record.additionalBytes < 0n ||
      record.additionalBytes > record.logicalBytes ||
      !Number.isSafeInteger(record.expiresAtMilliseconds)) {
    throw new TypeError('OPFS admission lease record is invalid')
  }
}

function validateLimits(limits: AdmissionAggregateLimits): void {
  if (limits.jobLimit <= 0n || limits.processLimit <= 0n || limits.quota < 0n ||
      limits.usage < 0n || limits.reserve < 0n ||
      !Number.isSafeInteger(limits.nowMilliseconds)) {
    throw new TypeError('OPFS admission aggregate limits are invalid')
  }
}

function quotaError(message: string): DOMException {
  return new DOMException(message, 'QuotaExceededError')
}

async function openDatabase(name: string): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') {
    throw new DOMException('IndexedDB OPFS admission is unavailable', 'NotSupportedError')
  }
  const request = indexedDB.open(name, ADMISSION_DATABASE_VERSION)
  let rejected = false
  return new Promise<IDBDatabase>((resolve, reject) => {
    request.addEventListener('upgradeneeded', () => {
      if (!request.result.objectStoreNames.contains(ADMISSION_STORE)) {
        request.result.createObjectStore(ADMISSION_STORE, { keyPath: 'id' })
      }
    })
    request.addEventListener('blocked', () => {
      rejected = true
      reject(new DOMException(
        'OPFS admission database upgrade is blocked by another tab',
        'InvalidStateError',
      ))
    }, { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      if (rejected) {
        request.result.close()
        return
      }
      resolve(request.result)
    }, { once: true })
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
