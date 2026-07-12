import type { FrameChannel, TransferPlan } from '../contracts'
import type { BlockSink } from '../contracts/sink'
import { combineSinkCleanupFailure } from './cleanup-failure'
import type {
  BlockOpener,
  ReceiveSessionOptions,
  ReceiveSessionState,
} from './model'
import { SessionClosedError } from './model'
import { ReceiveSession } from './receive'

type OwnershipState = 'owned' | 'settled' | 'settling' | 'transferred'

export interface StartedReceiveSession {
  readonly session: ReceiveSession
  readonly completion: Promise<void>
}

/**
 * Holds sink cleanup authority while connectivity policy runs. Transfer and
 * failure settlement are synchronous state changes, so cancellation cannot fall
 * into a gap where both the gateway and ReceiveSession assume the other owns it.
 */
export class ReceiveSessionPreparation {
  readonly #sink: BlockSink
  #state: OwnershipState = 'owned'
  #session: ReceiveSession | undefined
  #completion: Promise<void> | undefined
  #settlement: Promise<unknown> | undefined
  #prepared = false

  constructor(sink: BlockSink) {
    this.#sink = sink
  }

  get state(): ReceiveSessionState {
    if (this.#state === 'settled' || this.#state === 'settling') {
      return 'closed'
    }
    return this.#session?.state ?? 'idle'
  }

  prepare(
    plan: TransferPlan,
    opener: BlockOpener,
    options: ReceiveSessionOptions,
  ): void {
    if (this.#state !== 'owned' || this.#prepared) {
      throw new Error('receive session preparation can only be configured once')
    }
    this.#prepared = true
    this.#session = new ReceiveSession(plan, this.#sink, opener, options)
  }

  addChannel(channel: FrameChannel): void {
    if (this.#state !== 'owned' && this.#state !== 'transferred') {
      throw new SessionClosedError('cannot add a channel after sink settlement begins')
    }
    this.#requireSession().addChannel(channel)
  }

  transfer(signal?: AbortSignal): StartedReceiveSession {
    if (this.#state !== 'owned') {
      throw new Error('receive sink ownership can only be transferred once')
    }
    const session = this.#requireSession()
    // ReceiveSession observes an already-aborted signal synchronously inside
    // start(), closing the final race at the ownership handoff.
    this.#state = 'transferred'
    const completion = session.start(signal)
    this.#completion = completion
    return Object.freeze({ session, completion })
  }

  settleFailure(reason: unknown): Promise<unknown> {
    if (this.#settlement !== undefined) {
      return this.#settlement
    }
    if (this.#state === 'owned') {
      this.#state = 'settling'
      this.#settlement = this.#settleOwnedSink(reason)
      return this.#settlement
    }
    if (this.#state === 'transferred') {
      this.#state = 'settling'
      this.#settlement = this.#settleTransferredSession(reason)
      return this.#settlement
    }
    throw new Error('receive sink settlement state is inconsistent')
  }

  async #settleOwnedSink(reason: unknown): Promise<unknown> {
    try {
      await this.#sink.abort(reason)
      return reason
    } catch (cleanupError) {
      return combineSinkCleanupFailure(
        reason,
        cleanupError,
        'Receive session preparation failed and sink cleanup also failed',
      )
    } finally {
      this.#state = 'settled'
    }
  }

  async #settleTransferredSession(reason: unknown): Promise<unknown> {
    const session = this.#requireSession()
    const completion = this.#completion
    if (completion === undefined) {
      throw new Error('transferred receive session has no completion task')
    }
    try {
      await session.close(reason)
      try {
        await completion
        return reason
      } catch (sessionFailure) {
        return sessionFailure
      }
    } finally {
      this.#state = 'settled'
    }
  }

  #requireSession(): ReceiveSession {
    if (this.#session === undefined) {
      throw new Error('receive session preparation is not configured')
    }
    return this.#session
  }
}
