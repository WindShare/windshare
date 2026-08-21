import { encodeBase64Url } from '../../crypto/bytes'
import { snapshotIdentity } from '../workspace/canonical'
import {
  receiveOperationLeaseRecord,
  type ReceiveOperationLeaseRecord,
} from '../workspace/records'
import type {
  ReceiveOperationRepository,
  ReceiveOperationTransition,
} from '../workspace/repository'

const RECEIVE_OPERATION_LOCK_DOMAIN = 'windshare-receive-operation'
const CHECKPOINT_CLEANUP_LOCK_DOMAIN = 'windshare-output-cleanup'
const LEASE_ID_BYTES = 16

export interface BrowserLockHandle {
  readonly name: string
}

export interface BrowserLockManagerRuntime {
  request(
    name: string,
    options: { readonly mode: 'exclusive'; readonly ifAvailable?: true },
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void>
}

export interface BrowserReceiveOperationClock {
  now(): number
}

export interface BrowserReceiveOperationLease {
  readonly operationId: string
  readonly leaseId: string
  readonly acquiredAt: number
  heartbeat(): Promise<ReceiveOperationLeaseRecord>
  release(): Promise<void>
}

export interface BrowserCheckpointCleanupLease {
  release(): Promise<void>
}

export interface BrowserReceiveOperationLeaseOptions {
  readonly manager?: BrowserLockManagerRuntime
  readonly clock?: BrowserReceiveOperationClock
  readonly randomBytes?: (length: number) => Uint8Array
  /**
   * Lease acquisition can share the repository transaction that leaves a stable
   * lifecycle state. This is how resume clears its old deadline before work begins.
   */
  readonly acquireTransition?: Omit<
    ReceiveOperationTransition,
    'operationId' | 'expectedLeaseId' | 'lease'
  >
  readonly acquisitionTransitionCommitter?: BrowserReceiveOperationAcquisitionTransitionCommitter
}

export interface BrowserReceiveOperationAcquisitionTransitionCommitter {
  commitAcquisitionTransition(transition: ReceiveOperationTransition): Promise<void>
}

export class BrowserReceiveOperationBusyError extends DOMException {
  readonly operationId: string

  constructor(operationId: string) {
    super('The receive operation is already active in another browser context', 'InvalidStateError')
    this.operationId = snapshotIdentity(operationId, 16, 'operation ID')
  }
}

export function browserReceiveOperationLockName(operationId: string): string {
  const identity = snapshotIdentity(operationId, 16, 'operation ID')
  return `${RECEIVE_OPERATION_LOCK_DOMAIN}:${identity}`
}

/**
 * The Web Lock is the live cross-tab mutex; the IndexedDB lease is durable
 * evidence used to reject stale writers and to diagnose abandoned operations.
 */
export async function acquireBrowserReceiveOperationLease(
  repository: ReceiveOperationRepository,
  operationId: string,
  options: BrowserReceiveOperationLeaseOptions = {},
): Promise<BrowserReceiveOperationLease> {
  const identity = snapshotIdentity(operationId, 16, 'operation ID')
  const manager = options.manager ?? browserLockManager()
  const clock = options.clock ?? systemClock
  const randomBytes = options.randomBytes ?? secureRandomBytes
  const acquisitionTransitionCommitter = options.acquisitionTransitionCommitter ??
    defaultAcquisitionTransitionCommitter(repository)
  const lock = await acquireBrowserLease(
    browserReceiveOperationLockName(identity),
    true,
    () => new BrowserReceiveOperationBusyError(identity),
    manager,
  )
  const acquiredAt = clock.now()
  const leaseId = randomLeaseId(randomBytes)

  try {
    const existing = await repository.readLease(identity)
    const record = receiveOperationLeaseRecord({
      operationId: identity,
      leaseId,
      acquiredAt,
    })
    await acquisitionTransitionCommitter.commitAcquisitionTransition({
      ...(options.acquireTransition ?? {}),
      operationId: identity,
      ...(existing === undefined ? {} : { expectedLeaseId: existing.leaseId }),
      lease: { kind: 'put', record },
    })
  } catch (error) {
    await releaseAfterAcquisitionFailure(lock, error)
  }

  let releaseRequested = false
  let serial = Promise.resolve()
  let releasePromise: Promise<void> | undefined

  const heartbeat = (): Promise<ReceiveOperationLeaseRecord> => {
    if (releaseRequested) {
      return Promise.reject(new DOMException('Receive operation lease is closed', 'InvalidStateError'))
    }
    const operation = serial.then(async () => {
      const record = receiveOperationLeaseRecord({
        operationId: identity,
        leaseId,
        acquiredAt,
        heartbeatAt: clock.now(),
      })
      await repository.commitTransition({
        operationId: identity,
        expectedLeaseId: leaseId,
        lease: { kind: 'put', record },
      })
      return record
    })
    serial = operation.then(() => undefined, () => undefined)
    return operation
  }

  const release = (): Promise<void> => {
    if (releasePromise !== undefined) return releasePromise
    releaseRequested = true
    releasePromise = serial.then(async () => {
      let repositoryFailure: unknown
      try {
        await repository.commitTransition({
          operationId: identity,
          expectedLeaseId: leaseId,
          lease: { kind: 'delete', leaseId },
        })
      } catch (error) {
        repositoryFailure = error
      }
      try {
        await lock.release()
      } catch (lockFailure) {
        if (repositoryFailure !== undefined) {
          throw new AggregateError(
            [repositoryFailure, lockFailure],
            'Receive operation lease could not be released',
            { cause: lockFailure },
          )
        }
        throw lockFailure
      }
      if (repositoryFailure !== undefined) throw repositoryFailure
    })
    return releasePromise
  }

  return Object.freeze({
    operationId: identity,
    leaseId,
    acquiredAt,
    heartbeat,
    release,
  })
}

function defaultAcquisitionTransitionCommitter(
  repository: ReceiveOperationRepository,
): BrowserReceiveOperationAcquisitionTransitionCommitter {
  return Object.freeze({
    commitAcquisitionTransition: (transition: ReceiveOperationTransition) =>
      repository.commitTransition(transition),
  })
}

export async function verifyBrowserReceiveOperationLease(
  repository: Pick<ReceiveOperationRepository, 'readLease'>,
  lease: Pick<BrowserReceiveOperationLease, 'operationId' | 'leaseId' | 'acquiredAt'>,
): Promise<ReceiveOperationLeaseRecord> {
  const operationId = snapshotIdentity(lease.operationId, 16, 'operation ID')
  const leaseId = snapshotIdentity(lease.leaseId, 16, 'lease ID')
  const record = await repository.readLease(operationId)
  if (record === undefined || record.operationId !== operationId ||
      record.leaseId !== leaseId || record.acquiredAt !== lease.acquiredAt) {
    throw new DOMException(
      'The durable receive-operation lease does not match the live Web Lock owner',
      'InvalidStateError',
    )
  }
  return record
}

/** Serializes ownership-aware v5 cleanup across every page in the origin. */
export function acquireBrowserCheckpointCleanupLease(
  databaseName: string,
  manager: BrowserLockManagerRuntime = browserLockManager(),
): Promise<BrowserCheckpointCleanupLease> {
  if (databaseName.length === 0) {
    throw new TypeError('checkpoint database name must not be empty')
  }
  const encodedName = encodeBase64Url(new TextEncoder().encode(databaseName))
  return acquireBrowserLease(
    `${CHECKPOINT_CLEANUP_LOCK_DOMAIN}:${encodedName}`,
    false,
    () => new DOMException('Checkpoint cleanup is already active', 'InvalidStateError'),
    manager,
  )
}

async function acquireBrowserLease(
  name: string,
  failWhenUnavailable: boolean,
  unavailable: () => unknown,
  manager: BrowserLockManagerRuntime,
): Promise<BrowserCheckpointCleanupLease> {
  let acquiredResolve!: () => void
  let acquiredReject!: (reason: unknown) => void
  const acquired = new Promise<void>((resolve, reject) => {
    acquiredResolve = resolve
    acquiredReject = reject
  })
  let releaseResolve!: () => void
  const held = new Promise<void>((resolve) => {
    releaseResolve = resolve
  })
  const completion = manager.request(
    name,
    failWhenUnavailable ? { mode: 'exclusive', ifAvailable: true } : { mode: 'exclusive' },
    async (lock) => {
      if (lock === null) {
        acquiredReject(unavailable())
        return
      }
      acquiredResolve()
      await held
    },
  )
  completion.then(undefined, acquiredReject)
  await acquired

  let released = false
  return Object.freeze({
    release: async () => {
      if (released) return
      released = true
      releaseResolve()
      await completion
    },
  })
}

async function releaseAfterAcquisitionFailure(
  lock: BrowserCheckpointCleanupLease,
  acquisitionFailure: unknown,
): Promise<never> {
  try {
    await lock.release()
  } catch (releaseFailure) {
    throw new AggregateError(
      [acquisitionFailure, releaseFailure],
      'Receive operation lease acquisition could not release its Web Lock',
      { cause: releaseFailure },
    )
  }
  throw acquisitionFailure
}

function randomLeaseId(randomBytes: (length: number) => Uint8Array): string {
  const bytes = randomBytes(LEASE_ID_BYTES)
  if (!(bytes instanceof Uint8Array) || bytes.byteLength !== LEASE_ID_BYTES) {
    throw new TypeError('lease ID source must return exactly 16 bytes')
  }
  return snapshotIdentity(encodeBase64Url(bytes), LEASE_ID_BYTES, 'lease ID')
}

function secureRandomBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  globalThis.crypto.getRandomValues(bytes)
  return bytes
}

const systemClock: BrowserReceiveOperationClock = Object.freeze({
  now: () => Date.now(),
})

function browserLockManager(): BrowserLockManagerRuntime {
  const browserNavigator = globalThis.navigator as (Navigator & {
    readonly locks?: BrowserLockManagerRuntime
  }) | undefined
  const manager = browserNavigator?.locks
  if (manager === undefined) {
    throw new DOMException('Persistent output requires the Web Locks API', 'NotSupportedError')
  }
  return manager
}
