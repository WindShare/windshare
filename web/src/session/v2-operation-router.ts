import { encodeBase64Url } from '../crypto/bytes'
import {
  type V2MessageKind,
  V2_MESSAGE_KIND,
  type V2SessionMessage,
} from './v2-message'
import {
  V2OperationContinuationAuthority,
} from './v2-operation-continuation'
import { type V2SessionOperation, V2SessionRuntimeError } from './v2-runtime-types'

export const V2_SESSION_OPERATION_RESPONSE_QUEUE = 512
export const V2_SESSION_CONTROL_BACKLOG = 256
export const V2_SESSION_DATA_BACKLOG = 1_024
export const V2_SESSION_PLAINTEXT_BUDGET_BYTES = 128 * 1024 * 1024
export const V2_SESSION_BLOCK_CACHE_RESERVATION_BYTES = 64 * 1024 * 1024
export const V2_SESSION_REASSEMBLY_RESERVATION_BYTES = 8 * 4 * 1024 * 1024
// Queue admission receives the remainder after the same session's cache and
// bounded upstream reassemblies reserve their worst-case plaintext ownership.
export const V2_SESSION_PLAINTEXT_BACKLOG_BYTES = V2_SESSION_PLAINTEXT_BUDGET_BYTES -
  V2_SESSION_BLOCK_CACHE_RESERVATION_BYTES - V2_SESSION_REASSEMBLY_RESERVATION_BYTES
export const V2_MAXIMUM_ACTIVE_OPERATIONS = 256
export const V2_OPERATION_TOMBSTONE_MILLISECONDS = 30_000
export const V2_MAXIMUM_OPERATION_TOMBSTONES = 4_096

interface PendingRead {
  resolve(message: V2SessionMessage): void
  reject(reason: unknown): void
}

export class V2OperationQueue implements V2SessionOperation {
  readonly id: Uint8Array<ArrayBuffer>
  readonly requestKind: V2MessageKind
  readonly #messages: V2SessionMessage[] = []
  readonly #readers: PendingRead[] = []
  readonly #settlementCleanups = new Set<() => void>()
  readonly #admission: V2SessionQueueAdmission
  readonly #authority: V2OperationContinuationAuthority
  readonly #onClose: (authority: V2OperationContinuationAuthority) => void
  readonly #onConsumerSettled: () => void
  #pushTail: Promise<void> = Promise.resolve()
  #failure: unknown
  #closed = false
  #consumerSettled = false

  constructor(
    id: Uint8Array,
    requestKind: V2MessageKind,
    canonicalRequestBody: Uint8Array,
    admission: V2SessionQueueAdmission,
    onClose: (authority: V2OperationContinuationAuthority) => void,
    onConsumerSettled: () => void,
  ) {
    this.id = id.slice()
    this.requestKind = requestKind
    this.#admission = admission
    this.#authority = new V2OperationContinuationAuthority(requestKind, canonicalRequestBody)
    this.#onClose = onClose
    this.#onConsumerSettled = onConsumerSettled
  }

  next(signal?: AbortSignal): Promise<V2SessionMessage> {
    signal?.throwIfAborted()
    const available = this.#messages.shift()
    if (available !== undefined) {
      this.#admission.release(available)
      if (this.#closed && this.#messages.length === 0) this.#settleConsumer()
      return Promise.resolve(available)
    }
    if (this.#failure !== undefined) return Promise.reject(this.#failure)
    if (this.#closed) {
      return Promise.reject(new V2SessionRuntimeError('operation', 'Operation is complete'))
    }
    return new Promise<V2SessionMessage>((resolve, reject) => {
      const clear = () => signal?.removeEventListener('abort', abort)
      const abort = () => {
        const index = this.#readers.indexOf(pending)
        if (index >= 0) this.#readers.splice(index, 1)
        clear()
        reject(signal?.reason ?? new DOMException('Operation aborted', 'AbortError'))
      }
      const pending: PendingRead = {
        resolve: (value) => {
          clear()
          resolve(value)
        },
        reject: (reason) => {
          clear()
          reject(reason)
        },
      }
      this.#readers.push(pending)
      signal?.addEventListener('abort', abort, { once: true })
    })
  }

  push(message: V2SessionMessage): Promise<void> {
    const pushed = this.#pushTail.then(() => this.#push(message))
    this.#pushTail = pushed.catch(() => undefined)
    return pushed
  }

  async #push(message: V2SessionMessage): Promise<void> {
    if (this.#failure !== undefined) return
    if (this.#closed) {
      await this.#authority.acceptLate(message)
      return
    }
    const reservation = await this.#authority.reserve(message)
    if (reservation.disposition === 'drop') return
    if (this.#closed || this.#failure !== undefined) {
      reservation.rollback()
      if (this.#failure === undefined) await this.#authority.acceptLate(message)
      return
    }
    try {
      const reader = this.#readers.shift()
      if (reader === undefined) {
        if (this.#messages.length >= V2_SESSION_OPERATION_RESPONSE_QUEUE) {
          throw new V2SessionRuntimeError('session', 'Operation response queue is full')
        }
        this.#admission.charge(message)
        this.#messages.push(message)
      } else {
        reader.resolve(message)
      }
      reservation.accept()
      if (reservation.final) this.#finish(false)
    } catch (error) {
      reservation.rollback()
      throw error
    }
  }

  fail(reason: unknown): void {
    if (this.#failure !== undefined) return
    if (isSessionFailure(reason)) this.#authority.close()
    else this.#authority.retire('local-cancel')
    if (this.#closed) {
      this.#failure = reason
      this.#clearMessages()
      for (const reader of this.#readers.splice(0)) reader.reject(reason)
      this.#settleConsumer()
      return
    }
    this.#failure = reason
    this.#closed = true
    this.#clearMessages()
    for (const reader of this.#readers.splice(0)) reader.reject(reason)
    this.#settle(true)
  }

  close(): void {
    if (this.#closed) {
      this.#clearMessages()
      this.#settleConsumer()
      return
    }
    this.#finish(true)
  }

  onSettled(cleanup: () => void): void {
    if (this.#consumerSettled) {
      cleanup()
      return
    }
    this.#settlementCleanups.add(cleanup)
  }

  #finish(acceptLateNonterminal: boolean): void {
    if (this.#closed) return
    this.#closed = true
    this.#authority.retire(acceptLateNonterminal ? 'local-cancel' : 'remote-final')
    if (acceptLateNonterminal) this.#clearMessages()
    for (const reader of this.#readers.splice(0)) {
      reader.reject(new V2SessionRuntimeError('operation', 'Operation is complete'))
    }
    this.#settle(acceptLateNonterminal)
  }

  #settle(acceptLateNonterminal: boolean): void {
    this.#onClose(this.#authority)
    if (acceptLateNonterminal || this.#messages.length === 0) this.#settleConsumer()
  }

  #clearMessages(): void {
    for (const message of this.#messages.splice(0)) this.#admission.release(message)
  }

  #settleConsumer(): void {
    if (this.#consumerSettled) return
    this.#consumerSettled = true
    for (const cleanup of this.#settlementCleanups) cleanup()
    this.#settlementCleanups.clear()
    this.#onConsumerSettled()
  }

}

export class V2OperationRouter {
  readonly #operations = new Map<string, V2OperationQueue>()
  readonly #draining = new Set<V2OperationQueue>()
  readonly #onTerminal: (reason: unknown) => void
  readonly #now: () => number
  readonly #tombstones = new Map<string, V2OperationTombstone>()
  readonly #admission = new V2SessionQueueAdmission()
  #terminal: unknown

  constructor(onTerminal: (reason: unknown) => void, now: () => number = () => Date.now()) {
    this.#onTerminal = onTerminal
    this.#now = now
  }

  create(
    id: Uint8Array,
    requestKind: V2MessageKind,
    canonicalRequestBody: Uint8Array,
  ): V2OperationQueue {
    if (this.#terminal !== undefined) {
      throw new V2SessionRuntimeError('session', 'Protocol session is terminal')
    }
    this.#pruneTombstones()
    const key = encodeBase64Url(id)
    if (this.#operations.has(key) || this.#tombstones.has(key)) {
      throw new V2SessionRuntimeError('operation', 'Operation ID was reused')
    }
    if (this.#operations.size >= V2_MAXIMUM_ACTIVE_OPERATIONS) {
      throw new V2SessionRuntimeError('session', 'Active operation budget is exhausted')
    }
    if (this.#operations.size + this.#tombstones.size >= V2_MAXIMUM_OPERATION_TOMBSTONES) {
      throw new V2SessionRuntimeError('session', 'Operation tombstone budget is exhausted')
    }
    const operation = new V2OperationQueue(
      id,
      requestKind,
      canonicalRequestBody,
      this.#admission,
      (authority) => {
        this.#operations.delete(key)
        this.#draining.add(operation)
        this.#tombstones.set(key, new V2OperationTombstone(
          this.#now() + V2_OPERATION_TOMBSTONE_MILLISECONDS,
          authority,
        ))
      },
      () => this.#draining.delete(operation),
    )
    this.#operations.set(key, operation)
    return operation
  }

  owns(operation: V2SessionOperation): operation is V2OperationQueue {
    return this.#operations.get(encodeBase64Url(operation.id)) === operation
  }

  active(): readonly V2OperationQueue[] {
    return Object.freeze([...this.#operations.values()])
  }

  async route(message: V2SessionMessage): Promise<void> {
    if (this.#terminal !== undefined) return
    if (message.kind === V2_MESSAGE_KIND.sessionTerminal) {
      const reason = new V2SessionRuntimeError('session', 'Sender ended the protocol session')
      this.terminate(reason)
      this.#onTerminal(reason)
      return
    }
    const operationId = message.operationId
    if (operationId === undefined) {
      throw new V2SessionRuntimeError('session', 'Inbound operation message has no identity')
    }
    const key = encodeBase64Url(operationId)
    const operation = this.#operations.get(key)
    if (operation !== undefined) {
      await operation.push(message)
      return
    }
    this.#pruneTombstones()
    const tombstone = this.#tombstones.get(key)
    if (tombstone === undefined) {
      throw new V2SessionRuntimeError('session', 'Inbound message uses an unknown operation ID')
    }
    await tombstone.accept(message)
  }

  terminate(reason: unknown): void {
    if (this.#terminal !== undefined) return
    this.#terminal = reason
    for (const operation of [...this.#operations.values()]) operation.fail(reason)
    for (const operation of [...this.#draining]) operation.fail(reason)
    for (const tombstone of this.#tombstones.values()) tombstone.close()
    this.#operations.clear()
    this.#draining.clear()
    this.#tombstones.clear()
  }

  #pruneTombstones(): void {
    const now = this.#now()
    for (const [key, tombstone] of this.#tombstones) {
      if (tombstone.expiresAt <= now) {
        tombstone.close()
        this.#tombstones.delete(key)
      }
    }
  }
}

class V2SessionQueueAdmission {
  #control = 0
  #data = 0
  #plaintextBytes = 0

  charge(message: V2SessionMessage): void {
    const nextControl = this.#control + (message.data ? 0 : 1)
    const nextData = this.#data + (message.data ? 1 : 0)
    const nextBytes = this.#plaintextBytes + message.plaintext.byteLength
    if (
      nextControl > V2_SESSION_CONTROL_BACKLOG ||
      nextData > V2_SESSION_DATA_BACKLOG ||
      nextBytes > V2_SESSION_PLAINTEXT_BACKLOG_BYTES
    ) {
      throw new V2SessionRuntimeError('session', 'Protocol session response backlog is full')
    }
    this.#control = nextControl
    this.#data = nextData
    this.#plaintextBytes = nextBytes
  }

  release(message: V2SessionMessage): void {
    if (message.data) this.#data -= 1
    else this.#control -= 1
    this.#plaintextBytes -= message.plaintext.byteLength
    if (this.#control < 0 || this.#data < 0 || this.#plaintextBytes < 0) {
      throw new V2SessionRuntimeError('session', 'Protocol session response admission underflowed')
    }
  }
}

class V2OperationTombstone {
  readonly expiresAt: number
  readonly #authority: V2OperationContinuationAuthority

  constructor(
    expiresAt: number,
    authority: V2OperationContinuationAuthority,
  ) {
    this.expiresAt = expiresAt
    this.#authority = authority
  }

  accept(message: V2SessionMessage): Promise<void> {
    return this.#authority.acceptLate(message)
  }

  close(): void {
    this.#authority.close()
  }
}

function isSessionFailure(reason: unknown): boolean {
  return reason instanceof V2SessionRuntimeError && reason.scope === 'session'
}
