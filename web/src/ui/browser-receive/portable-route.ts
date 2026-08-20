import type { OutputDiagnosticsPorts } from '../../output/diagnostics'
import {
  materializationPlanSemantics,
  materializationRouteIdentity,
  sameArtifactChoiceSemantics,
  sameMaterializationPlanSemantics,
  sameMaterializationRouteIdentity,
  type ArtifactChoice,
  type BoundReceiveIntent,
  type MaterializationRouteIdentity,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../../output/planning'
import {
  createOperationID,
  createPortableBinding,
  createPortablePlanID,
} from '../../transfer/intent'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
  V2RouteCommitInput,
  V2RouteCommitResult,
} from '../v2-receive-runtime'
import { V2PresentationSourceError } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import {
  createPortableReceiveOperation,
  type PortableResolvedArtifactAction,
} from './portable'

const PORTABLE_ROUTE_MISMATCH = 'Resolved artifact no longer matches the installed portable route'

export interface PortableRouteDependencies {
  readonly createOperationId: () => string
  readonly createPortablePlanId: () => string
  readonly createReceiveOperation: (
    windowPort: BrowserReceiveWindow,
    action: PortableResolvedArtifactAction,
    intent: BoundReceiveIntent['intent'],
    diagnostics?: OutputDiagnosticsPorts,
  ) => Promise<V2BoundReceiveOperation>
}

export interface PortableArtifactPresentationAuthorityOptions {
  readonly windowPort: BrowserReceiveWindow
  readonly offered: OfferedArtifactChoice
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly dependencies?: Partial<PortableRouteDependencies>
}

type PortableActivationState = 'open' | 'committing' | 'transferred' | 'released'

export function startPortableArtifactAuthority(
  windowPort: BrowserReceiveWindow,
  offered: OfferedArtifactChoice,
  diagnostics?: OutputDiagnosticsPorts,
): V2ArtifactPresentationAuthority {
  return new PortableArtifactPresentationAuthority({
    windowPort,
    offered,
    ...(diagnostics === undefined ? {} : { diagnostics }),
  })
}

export class PortableArtifactPresentationAuthority implements V2ArtifactPresentationAuthority {
  readonly ready = Promise.resolve()
  readonly #window: BrowserReceiveWindow
  readonly #choice: ArtifactChoice
  readonly #routeIdentity: MaterializationRouteIdentity
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #dependencies: PortableRouteDependencies
  #state: PortableActivationState = 'open'
  #releaseReason: unknown

  constructor(options: PortableArtifactPresentationAuthorityOptions) {
    assertPortableOffer(options.offered)
    this.#window = options.windowPort
    this.#choice = options.offered.choice
    this.#routeIdentity = materializationRouteIdentity(options.offered.route)
    this.#diagnostics = options.diagnostics
    this.#dependencies = portableRouteDependencies(options.dependencies)
  }

  async commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult> {
    this.#claimCommit()
    let receiverOperationId: string | undefined
    let operation: V2BoundReceiveOperation | undefined
    try {
      this.#requireCommitting(input.signal)
      const action = this.#requireCurrentAction(input.action)
      receiverOperationId = this.#dependencies.createOperationId()
      const portable = await createPortableBinding({
        operationId: receiverOperationId,
        portablePlanId: this.#dependencies.createPortablePlanId(),
        artifact: action.artifact,
      })
      this.#requireCommitting(input.signal)

      const bound = await input.freezeAtFence(Object.freeze({
        kind: 'portable-binding',
        portableRouteId: action.route.portable.routeId,
        handoffTargetRouteId: action.route.handoffTarget.routeId,
        portable,
      }))
      this.#requireCommitting(input.signal)
      const fencedAction = this.#requireCurrentAction(input.action)
      requireBoundPortableCandidate(bound, fencedAction, portable.digest)

      operation = await this.#dependencies.createReceiveOperation(
        this.#window,
        fencedAction,
        bound.intent,
        this.#diagnostics,
      )
      this.#requireCommitting(input.signal)
      this.#state = 'transferred'
      return Object.freeze({ kind: 'bound-operation', operation })
    } catch (error) {
      const cleanupFailure = await detachPreReturnOperation(operation)
      if (cleanupFailure !== undefined) {
        this.#state = 'released'
        throw new AggregateError(
          [error, cleanupFailure],
          'Portable activation failed before transfer and could not detach its inert operation',
          { cause: error },
        )
      }
      if (this.#state === 'committing' && input.signal.aborted) {
        this.#state = 'open'
        return Object.freeze({
          kind: 'retryable-precut',
          ...(receiverOperationId === undefined ? {} : { receiverOperationId }),
        })
      }
      if (this.#state === 'committing') this.#state = 'released'
      throw error
    }
  }

  release(reason: unknown): void {
    if (this.#state === 'transferred' || this.#state === 'released') return
    this.#releaseReason = reason
    this.#state = 'released'
  }

  #claimCommit(): void {
    if (this.#state !== 'open') {
      throw new DOMException('Portable presentation authority is no longer available', 'InvalidStateError')
    }
    this.#state = 'committing'
  }

  #requireCommitting(signal: AbortSignal): void {
    signal.throwIfAborted()
    if (this.#state !== 'committing') {
      throw new V2PresentationSourceError(
        'stale_replacement',
        this.#releaseReason ?? new DOMException(
          'Portable presentation authority was released',
          'AbortError',
        ),
      )
    }
  }

  #requireCurrentAction(action: ResolvedArtifactAction): PortableResolvedArtifactAction {
    if (!isPortableResolvedAction(action) ||
        !sameArtifactChoiceSemantics(this.#choice, action.choice) ||
        !sameMaterializationPlanSemantics(
          action.choice.plan,
          materializationPlanSemantics(action.route),
        ) ||
        !sameMaterializationRouteIdentity(
          this.#routeIdentity,
          materializationRouteIdentity(action.route),
        )) {
      throw new DOMException(PORTABLE_ROUTE_MISMATCH, 'NotSupportedError')
    }
    return action
  }
}

function portableRouteDependencies(
  dependencies: Partial<PortableRouteDependencies> | undefined,
): PortableRouteDependencies {
  return Object.freeze({
    createOperationId: dependencies?.createOperationId ?? createOperationID,
    createPortablePlanId: dependencies?.createPortablePlanId ?? createPortablePlanID,
    createReceiveOperation: dependencies?.createReceiveOperation ?? createPortableReceiveOperation,
  })
}

async function detachPreReturnOperation(
  operation: V2BoundReceiveOperation | undefined,
): Promise<unknown | undefined> {
  if (operation === undefined) return undefined
  try {
    // Portable operations remain inert until coordinator adoption, so detach proves
    // a cancelled attempt has no authority or browser publication left behind.
    await operation.detach()
    return undefined
  } catch (error) {
    return error
  }
}

function requireBoundPortableCandidate(
  bound: BoundReceiveIntent,
  action: PortableResolvedArtifactAction,
  portableBindingDigest: string,
): void {
  if (bound.intent.operationId !== bound.decision.operation_id ||
      bound.intent.digest !== bound.decision.receive_intent_digest ||
      bound.intent.selection.digest !== action.selectionDigest ||
      bound.intent.artifact.digest !== action.artifact.digest ||
      bound.intent.artifact.kind !== bound.decision.artifact_kind ||
      bound.intent.plan.kind !== 'portable-handoff' ||
      bound.decision.plan_kind !== 'portable-handoff' ||
      bound.intent.plan.portable.operationId !== bound.intent.operationId ||
      bound.intent.plan.portable.artifactDigest !== action.artifact.digest ||
      bound.intent.plan.portable.digest !== portableBindingDigest) {
    throw new TypeError('Frozen ReceiveIntent does not bind the prepared portable candidate')
  }
}

function assertPortableOffer(offered: OfferedArtifactChoice): void {
  if (offered.route.kind !== 'portable-handoff' ||
      offered.choice.plan.kind !== 'portable-handoff' ||
      offered.choice.operation !== 'check-then-download' ||
      offered.choice.artifactKind === 'directory-tree' ||
      offered.choice.recovery !== 'none' ||
      offered.choice.preparation.manifest !== 'exact-artifact' ||
      offered.choice.preparation.hardAdmission !== 'portable-artifact' ||
      !offered.route.handoffTarget.supportsPortableArtifact ||
      !sameMaterializationPlanSemantics(
        offered.choice.plan,
        materializationPlanSemantics(offered.route),
      )) {
    throw new DOMException(PORTABLE_ROUTE_MISMATCH, 'NotSupportedError')
  }
}

function isPortableResolvedAction(
  action: ResolvedArtifactAction,
): action is PortableResolvedArtifactAction {
  return action.kind === 'resolved-artifact-action' &&
    action.route.kind === 'portable-handoff' &&
    action.choice.plan.kind === 'portable-handoff' &&
    action.choice.operation === 'check-then-download' &&
    action.choice.artifactKind !== 'directory-tree' &&
    action.artifact.kind !== 'directory-tree' &&
    action.resolvedArtifactDigest === action.artifact.digest &&
    action.choice.artifactKind === action.artifact.kind &&
    action.route.handoffTarget.supportsPortableArtifact
}
