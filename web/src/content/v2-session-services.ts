import type { V2ShareDescriptor } from '../catalog/v2-records'
import { SenderObjectError } from '../crypto/sender-object'
import { equalBytes } from '../crypto/bytes'
import { V2CborError } from '../protocol/cbor'
import { V2_MESSAGE_KIND } from '../session/v2-message'
import type { V2ReceiverSessionRuntime, V2SessionOperation } from '../session/v2-runtime'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import type {
  V2BlockDemand,
  V2BlockLane,
  V2BlockRouteEligibility,
  V2LaneSet,
} from './v2-broker'
import {
  decodeV2LeaseResult,
  decodeV2OpenResults,
  decodeV2OperationComplete,
  encodeV2BlockRequest,
  encodeV2LeaseRequest,
  encodeV2OpenRequest,
  V2_FRAGMENT_TIMEOUT_MILLISECONDS,
  V2FragmentAssembler,
  type V2RemoteLease,
  type V2RevisionFailure,
} from './v2-flow'
import {
  openV2BlockRecord,
  openV2RevisionObject,
  type V2BlockRecord,
  type V2FileRevisionDescriptor,
} from './v2-records'
import { remoteOperationErrorFor } from './v2-session-operations'

export {
  V2CatalogSessionOperations,
  V2RemoteOperationError,
} from './v2-session-operations'

const V2_LEASE_RETRY_INITIAL_MILLISECONDS = 250
const V2_LEASE_RETRY_MAXIMUM_MILLISECONDS = 2_000
const V2_LEASE_RELEASE_TIMEOUT_MILLISECONDS = 30_000

export class V2RemoteRevisionError extends Error {
  readonly failure: V2RevisionFailure

  constructor(failure: V2RevisionFailure) {
    super(`Sender could not open the file revision (0x${failure.code.toString(16)})`)
    this.name = 'V2RemoteRevisionError'
    this.failure = failure
  }
}

export class V2RevisionLeaseExpiredError extends Error {
  constructor(options?: ErrorOptions) {
    super('Revision lease expired before an authenticated renewal completed', options)
    this.name = 'V2RevisionLeaseExpiredError'
  }
}

export class V2RevisionChangedDuringRecoveryError extends Error {
  constructor() {
    super('File revision changed while recovering a ProtocolSession')
    this.name = 'V2RevisionChangedDuringRecoveryError'
  }
}

export class V2BlockOperationError extends Error {
  readonly code: 'object-auth' | 'fragment-conflict'

  constructor(
    code: 'object-auth' | 'fragment-conflict',
    message: string,
    options: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'V2BlockOperationError'
    this.code = code
  }
}

interface RemoteLeaseState {
  lease: V2RemoteLease
  expiresAt: number
  readonly lifetime: AbortController
  timer?: ReturnType<typeof setTimeout>
  error?: unknown
  released: boolean
}

export interface V2OpenedRevision {
  readonly descriptor: V2FileRevisionDescriptor
  readonly leaseId: Uint8Array<ArrayBuffer>
  release(): Promise<void>
}

export interface V2RevisionReader {
  open(fileId: Uint8Array, signal?: AbortSignal): Promise<V2OpenedRevision>
}

export class V2RevisionService {
  readonly #session: V2ReceiverSessionRuntime
  readonly #share: V2ShareDescriptor
  readonly #readSecret: Uint8Array<ArrayBuffer>
  readonly #lanes: V2LaneSet
  readonly #beforeLeaseRelease: (leaseId: Uint8Array<ArrayBuffer>) => Promise<void>
  readonly #now: () => number
  readonly #lifetime = new AbortController()
  readonly #leases = new Map<string, RemoteLeaseState>()
  #closed = false

  constructor(
    session: V2ReceiverSessionRuntime,
    share: V2ShareDescriptor,
    readSecret: Uint8Array,
    lanes: V2LaneSet,
    options: {
      readonly beforeLeaseRelease?: (leaseId: Uint8Array<ArrayBuffer>) => Promise<void>
      readonly now?: () => number
    } = {},
  ) {
    this.#session = session
    this.#share = share
    this.#readSecret = readSecret.slice()
    this.#lanes = lanes
    this.#beforeLeaseRelease = options.beforeLeaseRelease ?? (() => Promise.resolve())
    this.#now = options.now ?? (() => performance.now())
  }

  async open(
    fileId: Uint8Array,
    routes: V2BlockRouteEligibility,
    signal?: AbortSignal,
  ): Promise<V2OpenedRevision> {
    this.#requireOpen()
    const laneId = await this.#lanes.waitForEligibleLane(routes, signal)
    const operation = await this.#session.beginOperation(
      V2_MESSAGE_KIND.openRevisions,
      encodeV2OpenRequest(fileId),
      { laneId, ...(signal === undefined ? {} : { signal }) },
    )
    const message = await operation.next(signal)
    if (message.kind === V2_MESSAGE_KIND.operationError) {
      throw await remoteOperationErrorFor(this.#session, message.body, 'revision')
    }
    if (message.kind !== V2_MESSAGE_KIND.openResults) {
      throw new Error('Revision open received an unexpected response')
    }
    let result
    try {
      result = decodeV2OpenResults(message.body, fileId)
    } catch (error) {
      if (isProtocolContentFailure(error)) {
        return this.#failProtocol(error, 'Authenticated revision result is invalid')
      }
      throw error
    }
    if (result.failure !== undefined) throw new V2RemoteRevisionError(result.failure)
    if (result.revisionObject === undefined || result.lease === undefined) {
      throw new Error('Revision open returned no authenticated outcome')
    }
    let descriptor
    try {
      descriptor = await openV2RevisionObject(
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
        releaseTask ??= this.#beforeLeaseRelease(lease.id).then(() => this.#release(lease.id))
        return releaseTask
      },
    })
  }

  leaseError(leaseId: Uint8Array): unknown {
    const state = this.#leases.get(leaseKey(leaseId))
    if (state === undefined || state.released) return new Error('Revision lease is not active')
    if (state.error === undefined && this.#now() >= state.expiresAt) {
      state.error = new V2RevisionLeaseExpiredError()
    }
    return state.error
  }

  async #release(leaseId: Uint8Array): Promise<void> {
    const key = leaseKey(leaseId)
    const state = this.#leases.get(key)
    if (state === undefined || state.released) return
    state.released = true
    state.lifetime.abort(new DOMException('Revision lease released', 'AbortError'))
    if (state.timer !== undefined) clearTimeout(state.timer)
    this.#leases.delete(key)
    const deadline = operationDeadlineSignal(
      this.#lifetime.signal,
      V2_LEASE_RELEASE_TIMEOUT_MILLISECONDS,
      new V2SessionRuntimeError('lane', 'Revision lease release timed out'),
    )
    try {
      const laneId = await this.#lanes.waitForLane(deadline.signal)
      const operation = await this.#session.beginOperation(
        V2_MESSAGE_KIND.releaseLease,
        encodeV2LeaseRequest(leaseId),
        { laneId, signal: deadline.signal },
      )
      const message = await operation.next(deadline.signal)
      if (message.kind === V2_MESSAGE_KIND.operationError) {
        throw await remoteOperationErrorFor(this.#session, message.body, 'revision')
      }
      if (
        message.kind !== V2_MESSAGE_KIND.operationComplete ||
        decodeV2OperationComplete(message.body) !== 0
      ) {
        throw new Error('Revision release received an invalid completion')
      }
    } finally {
      deadline.close()
    }
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
    let retryDelay = V2_LEASE_RETRY_INITIAL_MILLISECONDS
    while (this.#activeLease(key, state)) {
      const remaining = state.expiresAt - this.#now()
      if (remaining <= 0) {
        state.error = new V2RevisionLeaseExpiredError()
        return
      }
      const deadline = leaseDeadlineSignal(state.lifetime.signal, remaining)
      try {
        await this.#renewOnce(state, deadline.signal)
      } catch (error) {
        deadline.close()
        if (!(await this.#waitForRenewalRetry(key, state, error, retryDelay))) return
        retryDelay = Math.min(retryDelay * 2, V2_LEASE_RETRY_MAXIMUM_MILLISECONDS)
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
      state.error = new V2RevisionLeaseExpiredError({ cause: error })
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
    const laneId = await this.#lanes.waitForLane(signal)
    const operation = await this.#session.beginOperation(
      V2_MESSAGE_KIND.renewLease,
      encodeV2LeaseRequest(state.lease.id),
      { laneId, signal },
    )
    const message = await operation.next(signal)
    if (message.kind === V2_MESSAGE_KIND.operationError) {
      throw await remoteOperationErrorFor(this.#session, message.body, 'revision')
    }
    if (message.kind !== V2_MESSAGE_KIND.leaseResult) {
      throw new Error('Lease renewal received an unexpected response')
    }
    try {
      state.lease = decodeV2LeaseResult(message.body, state.lease.id)
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
    throw new V2SessionRuntimeError('session', message, { cause: error })
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Revision service is closed')
  }
}

export class V2SessionBlockLane implements V2BlockLane {
  readonly id: number
  readonly #session: V2ReceiverSessionRuntime
  readonly #share: V2ShareDescriptor
  readonly #readSecret: Uint8Array<ArrayBuffer>
  readonly #revisions: V2RevisionService
  readonly #lifetime = new AbortController()

  constructor(
    id: number,
    session: V2ReceiverSessionRuntime,
    share: V2ShareDescriptor,
    readSecret: Uint8Array,
    revisions: V2RevisionService,
  ) {
    this.id = id
    this.#session = session
    this.#share = share
    this.#readSecret = readSecret.slice()
    this.#revisions = revisions
  }

  async fetchBlock(demand: V2BlockDemand, signal: AbortSignal): Promise<V2BlockRecord> {
    const linked = linkAbortSignals(signal, this.#lifetime.signal)
    try {
      linked.signal.throwIfAborted()
      const leaseFailure = this.#revisions.leaseError(demand.leaseId)
      if (leaseFailure !== undefined) throw leaseFailure
      const operation = await this.#session.beginOperation(
        V2_MESSAGE_KIND.requestBlocks,
        encodeV2BlockRequest(demand.leaseId, [demand.localBlockIndex]),
        { laneId: this.id, signal: linked.signal },
      )
      const assembler = new V2FragmentAssembler(operation.id)
      return await this.#receiveBlock(operation, assembler, demand, linked.signal)
    } finally {
      linked.close()
    }
  }

  close(): void {
    this.#lifetime.abort(new DOMException('Content lane closed', 'AbortError'))
    this.#readSecret.fill(0)
  }

  async #receiveBlock(
    operation: V2SessionOperation,
    assembler: V2FragmentAssembler,
    demand: V2BlockDemand,
    signal: AbortSignal,
  ): Promise<V2BlockRecord> {
    let object: Uint8Array<ArrayBuffer> | undefined
    const fragmentDeadline = Date.now() + V2_FRAGMENT_TIMEOUT_MILLISECONDS
    try {
      while (true) {
        const message = await nextBlockMessage(operation, signal, fragmentDeadline)
        if (message.kind === V2_MESSAGE_KIND.blockFragment) {
          object = await assembler.accept(message.body) ?? object
          continue
        }
        if (message.kind === V2_MESSAGE_KIND.operationError) {
          throw await remoteOperationErrorFor(this.#session, message.body, 'block')
        }
        if (
          message.kind !== V2_MESSAGE_KIND.operationComplete ||
          decodeV2OperationComplete(message.body) !== 1 ||
          object === undefined
        ) {
          throw new Error('Block operation completed without exactly one record')
        }
        return await openV2BlockRecord(
          object,
          this.#share,
          this.#readSecret,
          demand.descriptor,
          demand.localBlockIndex,
        )
      }
    } catch (error) {
      assembler.cancel()
      this.#session.cancelOperation(
        operation,
        error instanceof V2FragmentTimeoutError ? 4 : 5,
        this.id,
      ).catch(() => undefined)
      if (error instanceof SenderObjectError) {
        throw new V2BlockOperationError(
          'object-auth',
          'Block sender object failed authentication',
          { cause: error },
        )
      }
      if (error instanceof V2CborError) {
        throw new V2BlockOperationError(
          'fragment-conflict',
          'Authenticated block fragments violated their operation geometry',
          { cause: error },
        )
      }
      throw error
    }
  }
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

function linkAbortSignals(...sources: readonly AbortSignal[]): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const listeners = sources.map((source) => {
    const abort = () => controller.abort(
      source.reason ?? new DOMException('Content operation aborted', 'AbortError'),
    )
    source.addEventListener('abort', abort, { once: true })
    if (source.aborted) abort()
    return { source, abort }
  })
  return {
    signal: controller.signal,
    close: () => {
      for (const { source, abort } of listeners) source.removeEventListener('abort', abort)
    },
  }
}

function leaseDeadlineSignal(parent: AbortSignal, remainingMilliseconds: number): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  return operationDeadlineSignal(
    parent,
    remainingMilliseconds,
    new V2RevisionLeaseExpiredError(),
  )
}

function operationDeadlineSignal(
  parent: AbortSignal,
  remainingMilliseconds: number,
  timeoutReason: unknown,
): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent.reason ?? new DOMException('Revision lease renewal aborted', 'AbortError'),
  )
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(timeoutReason)
  }, Math.max(0, Math.ceil(remainingMilliseconds)))
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent.removeEventListener('abort', abort)
    },
  }
}

function delayWithAbort(milliseconds: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      signal.removeEventListener('abort', abort)
      resolve()
    }, Math.max(0, Math.ceil(milliseconds)))
    const abort = () => {
      globalThis.clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Lease renewal retry aborted', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
  })
}

function isRetryableLeaseLaneFailure(error: unknown): boolean {
  return error instanceof V2SessionRuntimeError && error.scope === 'lane'
}

function isProtocolContentFailure(error: unknown): boolean {
  return error instanceof SenderObjectError || error instanceof V2CborError
}

class V2FragmentTimeoutError extends V2SessionRuntimeError {
  constructor() {
    super('lane', 'Block fragment reassembly timed out')
    this.name = 'V2FragmentTimeoutError'
  }
}

async function nextBlockMessage(
  operation: V2SessionOperation,
  signal: AbortSignal,
  deadline: number | undefined,
) {
  if (deadline === undefined) return operation.next(signal)
  const remaining = deadline - Date.now()
  if (remaining <= 0) throw new V2FragmentTimeoutError()
  const timeout = new AbortController()
  const linked = linkAbortSignals(signal, timeout.signal)
  const timer = globalThis.setTimeout(() => timeout.abort(new V2FragmentTimeoutError()), remaining)
  try {
    return await operation.next(linked.signal)
  } finally {
    globalThis.clearTimeout(timer)
    linked.close()
  }
}
