import { linkAbortSignals, operationDeadlineSignal, delayWithAbort } from './scheduling/deadlines'
import { BlockResponseQueue, BlockInactivityTimeoutError } from './scheduling/block-response-wait'
import {
  LEASE_RETIREMENT_WAIT_MILLISECONDS, observeLeaseRetirement, LeaseRetirementQueue,
  type LeaseRetirementObservation, type LeaseRetirementOwner,
} from './scheduling/lease-retirement'
import type { ShareDescriptor } from '../catalog/records'
import { SenderObjectError } from '../crypto/sender-object'
import { equalBytes } from '../crypto/bytes'
import { createReceivedProtocolError, type ReceivedProtocolError } from '../diagnostics/incident/protocol-error'
import { protocolFailureFact, type FailureFact } from '../diagnostics/incident/fact'
import { CborError } from '../protocol/cbor'
import { MESSAGE_KIND, type SessionMessage } from '../session/message'
import type { ReceiverSessionRuntime, SessionOperation } from '../session/runtime'
import {
  OPERATION_CANCEL_REASON,
  SessionRuntimeError,
} from '../session/runtime-types'
import type {
  BlockDemand,
  BlockLane,
  BlockReadObserver,
  LaneSet,
} from './lane-set'
import type { BlockRouteEligibility } from './route-policy'
import {
  decodeLeaseResult,
  decodeOpenResults,
  decodeOperationComplete,
  encodeBlockRequest,
  encodeLeaseRequest,
  encodeOpenRequest,
  REVISION_CODE_QUOTA,
  REVISION_CODE_LEASE_EXPIRED,
  REVISION_CODE_INVALID_LEASE,
  REVISION_RETRY_MAXIMUM_MILLISECONDS,
  REVISION_RETRY_MINIMUM_MILLISECONDS,
  FragmentAssembler,
  FragmentInactivityError,
  type RemoteLease,
  type RevisionFailure,
} from './flow'
import {
  openBlockRecord,
  openRevisionObject,
  type BlockRecord,
  type FileRevisionDescriptor,
} from './records'
import { remoteOperationErrorFor, RemoteOperationError } from './session-operations'

export {
  CatalogSessionOperations,
  RemoteOperationError,
} from './session-operations'

const LEASE_RETRY_INITIAL_MILLISECONDS = 250
const LEASE_RETRY_MAXIMUM_MILLISECONDS = 2_000

export class RemoteRevisionError extends Error {
  readonly failure: RevisionFailure
  readonly protocolFailure: ReceivedProtocolError
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(protocolFailure: ReceivedProtocolError) {
    super(`Sender could not open the file revision (0x${protocolFailure.content.code.toString(16)})`)
    if (protocolFailure.requestKind !== 'open_revisions' || protocolFailure.content.scope !== 'revision') {
      throw new TypeError('Revision errors require an authenticated open-revisions result')
    }
    this.name = 'RemoteRevisionError'
    this.failure = Object.freeze({
      code: protocolFailure.content.code,
      retryable: protocolFailure.content.retryable,
      ...(protocolFailure.content.retryAfterMilliseconds === undefined
        ? {}
        : { retryAfterMilliseconds: protocolFailure.content.retryAfterMilliseconds }),
    })
    this.protocolFailure = protocolFailure
    this.failureFact = protocolFailureFact({
      error: this,
      stage: 'protocol_operation',
      // Wire retryability remains evidence; only the dedicated capacity type
      // grants a browser transfer authority to schedule another attempt.
      recoveryDisposition: 'none',
      protocolFailure,
    })
  }
}

export class RevisionCapacityBusyError extends RemoteRevisionError {
  readonly retryAfterMilliseconds: number

  constructor(protocolFailure: ReceivedProtocolError) {
    if (!isRevisionCapacityBusyFailure(protocolFailure.content)) {
      throw new TypeError('Revision capacity requires an exact retryable quota result')
    }
    super(protocolFailure)
    this.name = 'RevisionCapacityBusyError'
    this.retryAfterMilliseconds = protocolFailure.content.retryAfterMilliseconds
  }
}

export class RevisionLeaseExpiredError extends Error {
  constructor(options?: ErrorOptions) {
    super('Revision lease expired before an authenticated renewal completed', options)
    this.name = 'RevisionLeaseExpiredError'
  }
}

export function requiresRevisionLeaseReplacement(error: unknown): boolean {
  return error instanceof RevisionLeaseExpiredError ||
    (error instanceof RemoteOperationError && error.scope === 'revision' &&
      (error.code === REVISION_CODE_LEASE_EXPIRED || error.code === REVISION_CODE_INVALID_LEASE))
}

export class RevisionChangedDuringRecoveryError extends Error {
  constructor() {
    super('File revision changed while renewing file access')
    this.name = 'RevisionChangedDuringRecoveryError'
  }
}

export class BlockOperationError extends Error {
  readonly code: 'object-auth' | 'fragment-conflict'

  constructor(
    code: 'object-auth' | 'fragment-conflict',
    message: string,
    options: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'BlockOperationError'
    this.code = code
  }
}

interface RemoteLeaseState {
  lease: RemoteLease
  expiresAt: number
  readonly lifetime: AbortController
  timer?: ReturnType<typeof setTimeout>
  error?: unknown
  released: boolean
}

export interface OpenedRevision {
  readonly descriptor: FileRevisionDescriptor
  readonly leaseId: Uint8Array<ArrayBuffer>
  /** Relinquishes this consumer's lease. The service owns bounded remote retirement;
   * unconfirmed reclamation never changes the file result. Shared reads retain renewal. */
  /** Wait for shared-read safety; the session owns bounded remote reclamation afterward. */
  release(): Promise<void>
}

export interface RevisionReader {
  open(fileId: Uint8Array, signal?: AbortSignal): Promise<OpenedRevision>
}

export class RevisionService {
  readonly #session: ReceiverSessionRuntime
  readonly #share: ShareDescriptor
  readonly #readSecret: Uint8Array<ArrayBuffer>
  readonly #lanes: LaneSet
  readonly #beforeLeaseRelease: (leaseId: Uint8Array<ArrayBuffer>) => Promise<void>
  readonly #retirementOwner: LeaseRetirementOwner
  readonly #retirements: LeaseRetirementQueue
  readonly #now: () => number
  readonly #lifetime = new AbortController()
  readonly #leases = new Map<string, RemoteLeaseState>()
  #closed = false

  constructor(
    session: ReceiverSessionRuntime,
    share: ShareDescriptor,
    readSecret: Uint8Array,
    lanes: LaneSet,
    options: {
      readonly beforeLeaseRelease?: (leaseId: Uint8Array<ArrayBuffer>) => Promise<void>
      readonly onLeaseRetirement?: (observation: LeaseRetirementObservation) => void
      readonly now?: () => number
    } = {},
  ) {
    this.#session = session
    this.#share = share
    this.#readSecret = readSecret.slice()
    this.#lanes = lanes
    this.#beforeLeaseRelease = options.beforeLeaseRelease ?? (() => Promise.resolve())
    this.#retirementOwner = { signal: this.#lifetime.signal,
      ...(options.onLeaseRetirement === undefined ? {} : { observe: options.onLeaseRetirement }) }
    this.#retirements = new LeaseRetirementQueue(this.#retirementOwner)
    this.#now = options.now ?? (() => performance.now())
  }

  async open(
    fileId: Uint8Array,
    routes: BlockRouteEligibility,
    signal?: AbortSignal,
  ): Promise<OpenedRevision> {
    this.#requireOpen()
    return this.#lanes.requests.run(
      { kind: 'open_revisions', routes, ...(signal === undefined ? {} : { signal }) },
      route => this.#openOnLane(fileId, route.laneId, route.signal),
    )
  }

  async #openOnLane(fileId: Uint8Array, laneId: number, signal: AbortSignal): Promise<OpenedRevision> {
    const operation = await this.#session.beginOperation(
      MESSAGE_KIND.openRevisions,
      encodeOpenRequest(fileId),
      { laneId, ...(signal === undefined ? {} : { signal }) },
    )
    const message = await operation.next(signal)
    if (message.kind === MESSAGE_KIND.operationError) {
      throw remoteOperationErrorFor(this.#session, message)
    }
    if (message.kind !== MESSAGE_KIND.openResults) {
      throw new Error('Revision open received an unexpected response')
    }
    let result
    try {
      result = decodeOpenResults(message.body, fileId)
    } catch (error) {
      if (isProtocolContentFailure(error)) {
        return this.#failProtocol(error, 'Authenticated revision result is invalid')
      }
      throw error
    }
    if (result.failure !== undefined) {
      throw remoteRevisionErrorFor(result.failure, this.#session.authenticatedResponseCorrelation(message))
    }
    if (result.revisionObject === undefined || result.lease === undefined) {
      throw new Error('Revision open returned no authenticated outcome')
    }
    let descriptor
    try {
      descriptor = await openRevisionObject(
        result.revisionObject,
        this.#share,
        this.#readSecret,
        fileId,
      )
    } catch (error) {
      if (isProtocolContentFailure(error)) {
        return this.#failProtocol(error, 'File revision sender object failed authentication')
      }
      throw error
    }
    const lease = result.lease
    const state: RemoteLeaseState = {
      lease,
      expiresAt: this.#now() + lease.ttlMilliseconds,
      lifetime: new AbortController(),
      released: false,
    }
    const key = leaseKey(lease.id)
    if (this.#leases.has(key)) throw new Error('Sender reused an active revision lease ID')
    this.#leases.set(key, state)
    this.#scheduleRenewal(key, state)
    let releaseTask: Promise<void> | undefined
    return Object.freeze({
      descriptor,
      leaseId: lease.id,
      release: () => {
        // The barrier is injected by the receiver's broker owner so every future
        // revision consumer preserves first-waiter authorization automatically.
        releaseTask ??= this.#releaseWhenIdle(lease.id)
        return releaseTask
      },
    })
  }

  leaseError(leaseId: Uint8Array): unknown {
    const state = this.#leases.get(leaseKey(leaseId))
    if (state === undefined || state.released) return new Error('Revision lease is not active')
    if (state.error === undefined && this.#now() >= state.expiresAt) {
      state.error = new RevisionLeaseExpiredError()
    }
    return state.error
  }

  async #releaseWhenIdle(leaseId: Uint8Array<ArrayBuffer>): Promise<void> {
    const identity = { leaseId: leaseKey(leaseId), attempt: 0 }
    const deadline = operationDeadlineSignal(this.#lifetime.signal, LEASE_RETIREMENT_WAIT_MILLISECONDS,
      new DOMException('Shared revision reads are still active', 'TimeoutError'))
    observeLeaseRetirement(this.#retirementOwner, { ...identity, transition: 'waiting_for_reads' })
    const idle = Promise.resolve().then(() => this.#beforeLeaseRelease(leaseId))
    try {
      await awaitLeaseRelease(idle, deadline.signal)
    } catch (failure) {
      if (!deadline.signal.aborted) {
        observeLeaseRetirement(this.#retirementOwner, { ...identity, transition: 'abandoned',
          reason: 'barrier_failure', failure })
        throw failure
      }
      if (this.#closed) {
        observeLeaseRetirement(this.#retirementOwner, { ...identity, transition: 'abandoned',
          reason: 'service_closed', failure })
      } else {
        // The departing caller no longer owns this wait. Shared reads still own
        // the lease, so renewal continues until their barrier permits retirement.
        observeLeaseRetirement(this.#retirementOwner, { ...identity, transition: 'deferred_for_reads' })
        idle.then(() => this.#release(leaseId)).catch((failure: unknown) => {
          observeLeaseRetirement(this.#retirementOwner, { ...identity, transition: 'abandoned',
            reason: 'barrier_failure', failure })
        })
      }
      return
    } finally {
      deadline.close()
    }
    this.#release(leaseId)
  }

  #release(leaseId: Uint8Array): void {
    const key = leaseKey(leaseId)
    const state = this.#leases.get(key)
    if (state === undefined || state.released) return
    state.released = true
    state.lifetime.abort(new DOMException('Revision lease released', 'AbortError'))
    if (state.timer !== undefined) clearTimeout(state.timer)
    this.#leases.delete(key)
    this.#retirements.retire(key, async signal => {
      const message = await this.#lanes.requests.run(
        { kind: 'release_lease', signal },
        async route => {
          const operation = await this.#session.beginOperation(
            MESSAGE_KIND.releaseLease, encodeLeaseRequest(leaseId), route,
          )
          return operation.next(route.signal)
        },
      )
      if (message.kind === MESSAGE_KIND.operationError) {
        throw remoteOperationErrorFor(this.#session, message)
      }
      if (
        message.kind !== MESSAGE_KIND.operationComplete ||
        decodeOperationComplete(message.body) !== 0
      ) {
        throw new Error('Revision release received an invalid completion')
      }
    })
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#lifetime.abort(new DOMException('Revision service closed', 'AbortError'))
    this.#readSecret.fill(0)
    for (const state of this.#leases.values()) {
      state.released = true
      state.lifetime.abort(new DOMException('Revision service closed', 'AbortError'))
      if (state.timer !== undefined) clearTimeout(state.timer)
    }
    this.#leases.clear()
  }

  #scheduleRenewal(
    key: string,
    state: RemoteLeaseState,
    delayMilliseconds = state.lease.renewAfterMilliseconds,
  ): void {
    if (state.released || state.lease.renewAfterMilliseconds === 0) return
    const remaining = Math.max(0, state.expiresAt - this.#now())
    const delay = Math.min(delayMilliseconds, remaining)
    state.timer = setTimeout(() => {
      delete state.timer
      this.#renewUntilSettled(key, state).catch((error: unknown) => {
        if (this.#activeLease(key, state)) state.error = error
      })
    }, delay)
  }

  async #renewUntilSettled(key: string, state: RemoteLeaseState): Promise<void> {
    let retryDelay = LEASE_RETRY_INITIAL_MILLISECONDS
    while (this.#activeLease(key, state)) {
      const remaining = state.expiresAt - this.#now()
      if (remaining <= 0) {
        state.error = new RevisionLeaseExpiredError()
        return
      }
      const deadline = leaseDeadlineSignal(state.lifetime.signal, remaining)
      try {
        await this.#renewOnce(state, deadline.signal)
      } catch (error) {
        deadline.close()
        if (!(await this.#waitForRenewalRetry(key, state, error, retryDelay))) return
        retryDelay = Math.min(retryDelay * 2, LEASE_RETRY_MAXIMUM_MILLISECONDS)
        continue
      }
      deadline.close()
      if (!this.#activeLease(key, state)) return
      state.error = undefined
      state.expiresAt = this.#now() + state.lease.ttlMilliseconds
      this.#scheduleRenewal(key, state)
      return
    }
  }

  async #waitForRenewalRetry(
    key: string,
    state: RemoteLeaseState,
    error: unknown,
    retryDelay: number,
  ): Promise<boolean> {
    if (!this.#activeLease(key, state)) return false
    if (!isRetryableLeaseLaneFailure(error)) {
      state.error = error
      return false
    }
    const remaining = state.expiresAt - this.#now()
    if (remaining <= 0) {
      state.error = new RevisionLeaseExpiredError({ cause: error })
      return false
    }
    try {
      await delayWithAbort(Math.min(retryDelay, remaining), state.lifetime.signal)
      return this.#activeLease(key, state)
    } catch {
      return false
    }
  }

  async #renewOnce(state: RemoteLeaseState, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    const message = await this.#lanes.requests.run(
      { kind: 'renew_lease', signal },
      async route => {
        const operation = await this.#session.beginOperation(
          MESSAGE_KIND.renewLease, encodeLeaseRequest(state.lease.id), route,
        )
        return operation.next(route.signal)
      },
    )
    if (message.kind === MESSAGE_KIND.operationError) {
      throw remoteOperationErrorFor(this.#session, message)
    }
    if (message.kind !== MESSAGE_KIND.leaseResult) {
      throw new Error('Lease renewal received an unexpected response')
    }
    try {
      state.lease = decodeLeaseResult(message.body, state.lease.id)
    } catch (error) {
      if (isProtocolContentFailure(error)) {
        return this.#failProtocol(error, 'Authenticated lease renewal changed identity')
      }
      throw error
    }
  }

  #activeLease(key: string, state: RemoteLeaseState): boolean {
    return !state.released && !this.#closed && this.#leases.get(key) === state
  }

  async #failProtocol(error: unknown, message: string): Promise<never> {
    await this.#session.close()
    throw new SessionRuntimeError('session', message, { cause: error })
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Revision service is closed')
  }
}

export class SessionBlockLane implements BlockLane {
  readonly id: number
  readonly #session: ReceiverSessionRuntime
  readonly #share: ShareDescriptor
  readonly #readSecret: Uint8Array<ArrayBuffer>
  readonly #revisions: RevisionService
  readonly #responses = new BlockResponseQueue()
  readonly #lifetime = new AbortController()

  constructor(
    id: number,
    session: ReceiverSessionRuntime,
    share: ShareDescriptor,
    readSecret: Uint8Array,
    revisions: RevisionService,
  ) {
    this.id = id
    this.#session = session
    this.#share = share
    this.#readSecret = readSecret.slice()
    this.#revisions = revisions
  }

  async fetchBlock(demand: BlockDemand, signal: AbortSignal, observer?: BlockReadObserver): Promise<BlockRecord> {
    const linked = linkAbortSignals(signal, this.#lifetime.signal)
    try {
      linked.signal.throwIfAborted()
      const leaseFailure = this.#revisions.leaseError(demand.leaseId)
      if (leaseFailure !== undefined) throw leaseFailure
      const operation = await this.#session.beginOperation(
        MESSAGE_KIND.requestBlocks,
        encodeBlockRequest(demand.leaseId, [demand.localBlockIndex]),
        { laneId: this.id, signal: linked.signal },
      )
      const assembler = new FragmentAssembler(operation.id)
      return await this.#receiveBlock(operation, assembler, demand, linked.signal, bytes => {
        try { observer?.received(bytes) } catch { /* Receipt observers cannot grant content authority. */ }
      })
    } finally {
      linked.close()
    }
  }

  close(): void {
    this.#lifetime.abort(new DOMException('Content lane closed', 'AbortError'))
    this.#readSecret.fill(0)
  }

  async #receiveBlock(
    operation: SessionOperation,
    assembler: FragmentAssembler,
    demand: BlockDemand,
    signal: AbortSignal,
    onReceive: (objectBytes: number) => void,
  ): Promise<BlockRecord> {
    let object: Uint8Array<ArrayBuffer> | undefined
    const wait = this.#responses.begin(signal)
    try {
      while (true) {
        wait.resume()
        const message = await operation.next(wait.signal)
        wait.suspend()
        if (message.kind === MESSAGE_KIND.blockFragment) {
          const assembly = await assembler.accept(message.body)
          if (assembly.status === 'accepted' || assembly.status === 'complete') {
            wait.progress()
            onReceive(assembly.receivedBytes)
          }
          if (assembly.status === 'complete') object = assembly.object
          continue
        }
        if (message.kind === MESSAGE_KIND.operationError) {
          throw remoteOperationErrorFor(this.#session, message)
        }
        return await openBlockRecord(
          requireCompletedBlockObject(message, object),
          this.#share,
          this.#readSecret,
          demand.descriptor,
          demand.localBlockIndex,
        )
      }
    } catch (error) {
      this.#failBlockReceive(operation, assembler,
        error instanceof FragmentInactivityError ? wait.inactivityError({ cause: error }) : error)
    } finally {
      wait.close()
    }
  }

  #failBlockReceive(
    operation: SessionOperation,
    assembler: FragmentAssembler,
    error: unknown,
  ): never {
    assembler.cancel()
    this.#session.cancelOperation(
      operation,
      {
        protocolReason: error instanceof BlockInactivityTimeoutError
          ? OPERATION_CANCEL_REASON.timeout
          : OPERATION_CANCEL_REASON.laneRace,
        cause: error,
        ...(error instanceof BlockInactivityTimeoutError ? { blockWait: error } : {}),
        laneId: this.id,
      },
    ).catch(() => undefined)
    if (error instanceof SenderObjectError) {
      throw new BlockOperationError(
        'object-auth',
        'Block sender object failed authentication',
        { cause: error },
      )
    }
    if (error instanceof CborError) {
      throw new BlockOperationError(
        'fragment-conflict',
        'Authenticated block fragments violated their operation geometry',
        { cause: error },
      )
    }
    throw error
  }
}

function remoteRevisionErrorFor(
  failure: RevisionFailure,
  correlation: ReceivedProtocolError['correlation'],
): RemoteRevisionError {
  const protocolFailure = createReceivedProtocolError({
    requestKind: 'open_revisions', correlation, content: {
      scope: 'revision', code: failure.code, retryable: failure.retryable, ...(failure.retryAfterMilliseconds === undefined
        ? {}
        : { retryAfterMilliseconds: failure.retryAfterMilliseconds })
    }
  })
  return isRevisionCapacityBusyFailure(failure)
    ? new RevisionCapacityBusyError(protocolFailure)
    : new RemoteRevisionError(protocolFailure)
}

function isRevisionCapacityBusyFailure(
  failure: RevisionFailure,
): failure is RevisionFailure & Readonly<{
  retryable: true
  retryAfterMilliseconds: number
}> {
  const retryAfterMilliseconds = failure.retryAfterMilliseconds
  return failure.code === REVISION_CODE_QUOTA &&
    failure.retryable &&
    retryAfterMilliseconds !== undefined &&
    Number.isInteger(retryAfterMilliseconds) &&
    retryAfterMilliseconds >= REVISION_RETRY_MINIMUM_MILLISECONDS &&
    retryAfterMilliseconds <= REVISION_RETRY_MAXIMUM_MILLISECONDS
}

function requireCompletedBlockObject(
  message: SessionMessage,
  object: Uint8Array<ArrayBuffer> | undefined,
): Uint8Array<ArrayBuffer> {
  if (message.kind !== MESSAGE_KIND.operationComplete) {
    throw new Error('Block operation received an unexpected response')
  }
  if (decodeOperationComplete(message.body) !== 1) {
    throw new Error('Block operation completed without exactly one result')
  }
  if (object === undefined) {
    // A final can replay on a replacement lane after the original fragment
    // send became physically ambiguous. The authenticated count preserves
    // result authority; only the missing bytes need another bounded lane try.
    throw new IncompleteBlockDeliveryError()
  }
  return object
}

function leaseKey(leaseId: Uint8Array): string {
  const normalized = new Uint8Array(leaseId)
  if (normalized.byteLength !== 16 || !normalized.some((item) => item !== 0)) {
    throw new TypeError('Revision lease ID must be a nonzero 16-byte identity')
  }
  return Array.from(normalized, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export function sameLease(left: Uint8Array, right: Uint8Array): boolean {
  return equalBytes(left, right)
}

function leaseDeadlineSignal(parent: AbortSignal, remainingMilliseconds: number): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  return operationDeadlineSignal(
    parent,
    remainingMilliseconds,
    new RevisionLeaseExpiredError(),
  )
}

function awaitLeaseRelease(cleanup: Promise<void>, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
    cleanup.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort))
  })
}

function isRetryableLeaseLaneFailure(error: unknown): boolean {
  return error instanceof SessionRuntimeError && error.scope === 'lane'
}

function isProtocolContentFailure(error: unknown): boolean {
  return error instanceof SenderObjectError || error instanceof CborError
}

class IncompleteBlockDeliveryError extends SessionRuntimeError {
  constructor() {
    super('lane', 'Block completion arrived without its promised fragment record')
    this.name = 'IncompleteBlockDeliveryError'
  }
}
