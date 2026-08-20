import {
  bindReceiveIntent,
  type BoundReceiveIntent,
  type CandidateMaterializationBinding,
  type ResolvedArtifactAction,
} from '../../output/planning'
import { validateReceiveIntent, type ReceiveIntent, type SelectionSpec } from '../../transfer/intent'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
  V2RouteCommitResult,
} from '../v2-receive-runtime'
import { V2ActivationStateContractError } from './activation-model'
import type {
  V2ActivationCleanupOwnerKind,
  V2ActivationCleanupStage,
  V2AuthorityActivationTerminalOutcome,
} from './activation-model'

export type ReceiveIntentBinder = typeof bindReceiveIntent

export type AuthorityCommitOutcome =
  | Extract<V2RouteCommitResult, { kind: 'retryable-precut' | 'owned-effects' }>
  | Readonly<{
      kind: 'bound-operation'
      operation: V2BoundReceiveOperation
      intent: ReceiveIntent
    }>

export interface AuthorityCommitTransactionOptions {
  readonly action: ResolvedArtifactAction
  readonly observationRevision: number
  readonly authority: V2ArtifactPresentationAuthority
  readonly assertFinalFence: (transaction: AuthorityCommitTransaction) => SelectionSpec
  readonly binder?: ReceiveIntentBinder
}

/**
 * Encapsulates the only operation allowed to cross the route's durable cut.
 * The coordinator supplies every authority decision through the final fence;
 * this object only binds once and proves the returned runtime matches that cut.
 */
export class AuthorityCommitTransaction {
  readonly action: ResolvedArtifactAction
  readonly observationRevision: number
  readonly #authority: V2ArtifactPresentationAuthority
  readonly #assertFinalFence: AuthorityCommitTransactionOptions['assertFinalFence']
  readonly #binder: ReceiveIntentBinder
  readonly #controller = new AbortController()
  #freezeRequested = false
  #frozen: BoundReceiveIntent | undefined
  #started = false
  #outcome: AuthorityCommitOutcome | undefined

  constructor(options: AuthorityCommitTransactionOptions) {
    this.action = options.action
    this.observationRevision = options.observationRevision
    this.#authority = options.authority
    this.#assertFinalFence = options.assertFinalFence
    this.#binder = options.binder ?? bindReceiveIntent
  }

  get fenced(): boolean {
    return this.#frozen !== undefined
  }

  get outcome(): AuthorityCommitOutcome | undefined {
    return this.#outcome
  }

  abortPreFence(reason: unknown): void {
    if (!this.fenced) this.#controller.abort(reason)
  }

  async run(): Promise<AuthorityCommitOutcome> {
    if (this.#started) throw new V2ActivationStateContractError('commit transaction started twice')
    this.#started = true
    const result = await this.#authority.commit({
      action: this.action,
      signal: this.#controller.signal,
      freezeAtFence: candidate => this.#freeze(candidate),
    })
    this.#outcome = await this.#validateResult(result)
    return this.#outcome
  }

  async #freeze(candidate: CandidateMaterializationBinding): Promise<BoundReceiveIntent> {
    if (this.#freezeRequested) {
      throw new V2ActivationStateContractError('route requested the final intent fence twice')
    }
    this.#freezeRequested = true
    const selection = this.#assertFinalFence(this)
    const frozen = await this.#binder({ selection, action: this.action, candidate })
    this.#assertFinalFence(this)
    this.#frozen = frozen
    return frozen
  }

  async #validateResult(result: V2RouteCommitResult): Promise<AuthorityCommitOutcome> {
    if (result.kind !== 'bound-operation') return result
    const frozen = this.#frozen
    if (frozen === undefined) {
      throw new V2ActivationStateContractError('route committed before the final intent fence')
    }
    const runtime = result.operation
    const intent = await validateReceiveIntent(runtime.intent)
    if (intent.digest !== frozen.intent.digest ||
        runtime.lifecycle.operationId !== intent.operationId ||
        runtime.lifecycle.receiveIntentDigest !== intent.digest) {
      throw new V2ActivationStateContractError(
        'bound receive operation does not match the coordinator-frozen intent',
      )
    }
    return Object.freeze({ kind: 'bound-operation', operation: runtime, intent })
  }
}

export interface ActivationCleanupOwner {
  readonly kind: V2ActivationCleanupOwnerKind
  readonly operationId: string
  readonly settle: (reason: unknown) => PromiseLike<unknown>
  readonly detach: () => void | PromiseLike<void>
  readonly reason: unknown
  readonly outcome: V2AuthorityActivationTerminalOutcome
  settlementComplete: boolean
  detachComplete: boolean
  running: boolean
}

export interface ActivationCleanupProgress {
  readonly failedStages: readonly V2ActivationCleanupStage[]
  readonly detachedNow: boolean
  readonly complete: boolean
}

export function ownedEffectsCleanup(
  result: Extract<AuthorityCommitOutcome, { kind: 'owned-effects' }>,
  outcome: V2AuthorityActivationTerminalOutcome,
): ActivationCleanupOwner {
  return {
    kind: 'owned-effects',
    operationId: result.authority.intent.operationId,
    settle: reason => result.authority.settleActivationFailure(reason),
    detach: () => result.authority.detach(),
    reason: result.cause,
    outcome,
    settlementComplete: false,
    detachComplete: false,
    running: false,
  }
}

export function boundOperationCleanup(
  runtime: V2BoundReceiveOperation,
  reason: unknown,
  outcome: V2AuthorityActivationTerminalOutcome,
): ActivationCleanupOwner {
  return {
    kind: 'bound-operation',
    operationId: runtime.intent.operationId,
    settle: failure => Promise.resolve(runtime.settleTransferAdmissionFailure(failure)),
    detach: () => runtime.detach(),
    reason,
    outcome,
    settlementComplete: false,
    detachComplete: false,
    running: false,
  }
}

/** Advances only unfinished stages so a coordinator retry cannot repeat durable settlement. */
export async function advanceActivationCleanup(
  cleanup: ActivationCleanupOwner,
): Promise<ActivationCleanupProgress> {
  const failedStages: V2ActivationCleanupStage[] = []
  if (!cleanup.settlementComplete) {
    try {
      await cleanup.settle(cleanup.reason)
      cleanup.settlementComplete = true
    } catch {
      failedStages.push('settlement')
    }
  }
  let detachedNow = false
  if (!cleanup.detachComplete) {
    try {
      await Promise.resolve(cleanup.detach())
      cleanup.detachComplete = true
      detachedNow = true
    } catch {
      failedStages.push('detach')
    }
  }
  return Object.freeze({
    failedStages: Object.freeze(failedStages),
    detachedNow,
    complete: cleanup.settlementComplete && cleanup.detachComplete,
  })
}
