import {
  bindReceiveIntent,
  materializationRouteIdentity,
  sameMaterializationRouteIdentity,
  type BoundReceiveIntent,
  type CandidateMaterializationBinding,
  type MaterializationRouteIdentity,
  type ResolvedArtifactAction,
} from '../../../output/planning'
import { validateReceiveIntent, type ReceiveIntent, type SelectionSpec } from '../../../transfer/intent'
import type {
  ArtifactPresentationAuthority,
  BoundReceiveOperation,
  RouteCommitInput,
  RouteCommitResult,
} from '../contracts'
import { ActivationStateContractError } from './model'
import type {
  ActivationCleanupOwnerKind,
  ActivationCleanupStage,
  AuthorityActivationTerminalOutcome,
} from './model'

export type ReceiveIntentBinder = typeof bindReceiveIntent

export interface ProvisionalOwnedEffectAuthority {
  readonly operationId: string
  readonly choiceId: ResolvedArtifactAction['choiceId']
  readonly installedRoute: MaterializationRouteIdentity
  settleActivationFailure(reason: unknown): PromiseLike<unknown>
  detach(): void | PromiseLike<void>
}

export interface ProvisionalOwnedEffectsResult {
  readonly kind: 'provisional-owned-effects'
  readonly cause: unknown
  readonly authority: ProvisionalOwnedEffectAuthority
}

export interface AuthorityCommitRouteInput extends RouteCommitInput {
  /**
   * Bootstrap records can own filesystem effects before an intent exists. Registering
   * that authority keeps failures recoverable without manufacturing a canonical intent.
   */
  readonly registerProvisionalOwnedEffects: (
    authority: ProvisionalOwnedEffectAuthority,
  ) => void
}

export interface AuthorityCommitRoute {
  commit(input: AuthorityCommitRouteInput): Promise<RouteCommitResult>
}

export type AuthorityCommitOutcome =
  | Extract<RouteCommitResult, { kind: 'retryable-precut' | 'owned-effects' }>
  | Readonly<{
      kind: 'bound-operation'
      operation: BoundReceiveOperation
      intent: ReceiveIntent
    }>

export interface AuthorityCommitTransactionOptions {
  readonly action: ResolvedArtifactAction
  readonly observationRevision: number
  readonly authority: ArtifactPresentationAuthority | AuthorityCommitRoute
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
  readonly #authority: ArtifactPresentationAuthority | AuthorityCommitRoute
  readonly #assertFinalFence: AuthorityCommitTransactionOptions['assertFinalFence']
  readonly #binder: ReceiveIntentBinder
  readonly #controller = new AbortController()
  #freezeRequested = false
  #frozen: BoundReceiveIntent | undefined
  #provisionalOwnedEffects: ProvisionalOwnedEffectAuthority | undefined
  #started = false
  #finished = false
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

  get hasProvisionalOwnedEffects(): boolean {
    return this.#provisionalOwnedEffects !== undefined
  }

  /** Transfers cleanup ownership exactly once when the route cannot reach a canonical cut. */
  takeProvisionalOwnedEffects(cause: unknown): ProvisionalOwnedEffectsResult | undefined {
    if (!this.#finished) {
      throw new ActivationStateContractError(
        'provisional cleanup ownership cannot transfer before the commit attempt finishes',
      )
    }
    const authority = this.#provisionalOwnedEffects
    if (authority === undefined) return undefined
    this.#provisionalOwnedEffects = undefined
    return Object.freeze({ kind: 'provisional-owned-effects', cause, authority })
  }

  abortPreFence(reason: unknown): void {
    if (!this.fenced) this.#controller.abort(reason)
  }

  async run(): Promise<AuthorityCommitOutcome> {
    if (this.#started) throw new ActivationStateContractError('commit transaction started twice')
    this.#started = true
    try {
      const result = await this.#authority.commit({
        action: this.action,
        signal: this.#controller.signal,
        freezeAtFence: candidate => this.#freeze(candidate),
        registerProvisionalOwnedEffects: authority => this.#registerProvisionalOwnedEffects(authority),
      })
      this.#outcome = await this.#validateResult(result)
      return this.#outcome
    } finally {
      this.#finished = true
    }
  }

  async #freeze(candidate: CandidateMaterializationBinding): Promise<BoundReceiveIntent> {
    if (this.#freezeRequested) {
      throw new ActivationStateContractError('route requested the final intent fence twice')
    }
    this.#freezeRequested = true
    const selection = this.#assertFinalFence(this)
    const frozen = await this.#binder({ selection, action: this.action, candidate })
    const provisional = this.#provisionalOwnedEffects
    if (provisional !== undefined && provisional.operationId !== frozen.intent.operationId) {
      throw new ActivationStateContractError(
        'provisional effects do not belong to the coordinator-frozen operation',
      )
    }
    this.#assertFinalFence(this)
    this.#frozen = frozen
    return frozen
  }

  async #validateResult(result: RouteCommitResult): Promise<AuthorityCommitOutcome> {
    if (result.kind === 'retryable-precut') {
      if (this.#provisionalOwnedEffects !== undefined) {
        throw new ActivationStateContractError(
          'route reported a pre-cut retry while provisional effects remain owned',
        )
      }
      return result
    }
    const frozen = this.#frozen
    if (frozen === undefined) {
      throw new ActivationStateContractError('route committed before the final intent fence')
    }
    const authority = result.kind === 'bound-operation' ? result.operation : result.authority
    const intent = await validateReceiveIntent(authority.intent)
    if (intent.digest !== frozen.intent.digest ||
        authority.lifecycle.operationId !== intent.operationId ||
        authority.lifecycle.receiveIntentDigest !== intent.digest) {
      throw new ActivationStateContractError(
        'bound receive operation does not match the coordinator-frozen intent',
      )
    }
    this.#promoteProvisionalOwnedEffects(intent.operationId)
    return result.kind === 'bound-operation'
      ? Object.freeze({ kind: 'bound-operation', operation: result.operation, intent })
      : result
  }

  #registerProvisionalOwnedEffects(authority: ProvisionalOwnedEffectAuthority): void {
    if (this.#frozen !== undefined || this.#freezeRequested) {
      throw new ActivationStateContractError(
        'route registered provisional effects after requesting the final intent fence',
      )
    }
    if (this.#provisionalOwnedEffects !== undefined) {
      throw new ActivationStateContractError('route registered provisional effects twice')
    }
    if (authority.choiceId !== this.action.choiceId) {
      throw new ActivationStateContractError(
        'provisional effects do not belong to the selected artifact choice',
      )
    }
    if (!sameMaterializationRouteIdentity(
      authority.installedRoute,
      materializationRouteIdentity(this.action.route),
    )) {
      throw new ActivationStateContractError(
        'provisional effects do not belong to the installed materialization route',
      )
    }
    this.#provisionalOwnedEffects = authority
  }

  #promoteProvisionalOwnedEffects(operationId: string): void {
    const authority = this.#provisionalOwnedEffects
    if (authority === undefined) return
    if (authority.operationId !== operationId) {
      throw new ActivationStateContractError(
        'committed operation does not own the provisional effects',
      )
    }
    this.#provisionalOwnedEffects = undefined
  }
}

export interface ActivationCleanupOwner {
  readonly kind: ActivationCleanupOwnerKind
  readonly operationId: string
  readonly settle: (reason: unknown) => PromiseLike<unknown>
  readonly detach: () => void | PromiseLike<void>
  readonly reason: unknown
  readonly outcome: AuthorityActivationTerminalOutcome
  settlementComplete: boolean
  detachComplete: boolean
  running: boolean
}

export interface ActivationCleanupProgress {
  readonly failedStages: readonly ActivationCleanupStage[]
  readonly detachedNow: boolean
  readonly complete: boolean
}

export function ownedEffectsCleanup(
  result: Extract<AuthorityCommitOutcome, { kind: 'owned-effects' }>,
  outcome: AuthorityActivationTerminalOutcome,
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

export function provisionalOwnedEffectsCleanup(
  result: ProvisionalOwnedEffectsResult,
  outcome: AuthorityActivationTerminalOutcome,
): ActivationCleanupOwner {
  return {
    kind: 'owned-effects',
    operationId: result.authority.operationId,
    settle: reason => Promise.resolve(result.authority.settleActivationFailure(reason)),
    detach: () => result.authority.detach(),
    reason: result.cause,
    outcome,
    settlementComplete: false,
    detachComplete: false,
    running: false,
  }
}

export function boundOperationCleanup(
  runtime: BoundReceiveOperation,
  reason: unknown,
  outcome: AuthorityActivationTerminalOutcome,
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
  const failedStages: ActivationCleanupStage[] = []
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
