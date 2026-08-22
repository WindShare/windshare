import {
  materializationRouteIdentity,
  sameArtifactChoiceSemantics,
  sameMaterializationRouteIdentity,
  type ArtifactActionsOffer,
  type ArtifactChoice,
  type OfferedArtifactChoice,
} from '../../output/planning'
import type { ArtifactChoiceID, ReceiveIntent, SelectionSpec } from '../../transfer/intent'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type { V2BoundReceiveOperation } from '../v2-receive-runtime'
import type { ActiveReceiveAdoption } from './active-receive'
import { V2ActivationStateContractError } from './activation-model'
import {
  AuthorityCommitTransaction,
  type AuthorityCommitOutcome,
  type ReceiveIntentBinder,
} from './authority-commit'
import {
  createAuthorityActivationRecord,
  observationMatchesActivation,
  type AuthorityActivationRecord,
} from './authority-lifecycle'
import { planningSubject, type AuthorityPlanningPipeline } from './authority-planning'
import { StaleReceiveBoundaryError } from './contracts'
import type { V2ActiveProjection } from './projection-observation'
import type { V2PresentationAttempt } from './presentation-attempt'

export interface FrozenAuthorityCommitContext {
  readonly currentActivation: () => AuthorityActivationRecord | undefined
  readonly currentProjection: () => V2ActiveProjection | undefined
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly joinSuspended: () => boolean
  readonly planning: AuthorityPlanningPipeline
  readonly binder?: ReceiveIntentBinder
}

export interface FrozenBoundOperation {
  readonly runtime: V2BoundReceiveOperation
  readonly intent: ReceiveIntent
  readonly adoption: ActiveReceiveAdoption
}

/** Constructs the record the coordinator installs before invoking reentrant presentation authority. */
export function createProvisionalAuthorityActivation(input: Readonly<{
  activationId: string
  active: V2ActiveProjection
  offered: OfferedArtifactChoice
  preClickRanking: readonly ArtifactChoiceID[]
  observationRevision: number
  attempt: V2PresentationAttempt
}>): AuthorityActivationRecord {
  return createAuthorityActivationRecord({
    activationId: input.activationId,
    authenticatedShareInstanceId: input.active.selection.shareInstance,
    selectionDigest: input.active.selection.digest,
    choice: input.offered.choice,
    installedRoute: materializationRouteIdentity(input.offered.route),
    preClickRanking: input.preClickRanking,
    attempt: input.attempt,
    joined: input.active.joined,
    observationRevision: input.observationRevision,
    protocolSessionId: input.active.protocolSessionId,
    projectionEpoch: input.active.epoch,
  })
}

/** Snapshots exactly the ranking visible for the selected artifact before trusted UI work. */
export function displayedArtifactChoiceRanking(
  offers: ArtifactActionsOffer,
  selected: ArtifactChoice,
): readonly ArtifactChoiceID[] {
  const displayed = selected.artifactKind === 'zip-archive' && offers.zip !== null
    ? [offers.zip.primary, ...(offers.zip.secondary === null ? [] : [offers.zip.secondary])]
    : [offers.primary, ...offers.alternatives]
        .filter(candidate => candidate.choice.artifactKind !== 'zip-archive')
  const ranking = displayed.map(candidate => candidate.choice.choiceId)
  if (!ranking.includes(selected.choiceId) || new Set(ranking).size !== ranking.length) {
    throw new V2ActivationStateContractError(
      'displayed artifact ranking does not own the selected choice',
    )
  }
  return Object.freeze(ranking)
}

/** Creates the one transaction permitted to consume the resolved route's final intent fence. */
export function beginFrozenAuthorityCommit(
  record: AuthorityActivationRecord,
  context: FrozenAuthorityCommitContext,
): AuthorityCommitTransaction | undefined {
  if (context.currentActivation() !== record || record.terminal !== undefined ||
      record.commitAttempt !== undefined || record.cleanup !== undefined ||
      record.observationReplacementPending || context.joinSuspended() ||
      !record.authorityReady || record.resolution === undefined || record.authority === undefined ||
      !context.planning.readyForCommit(
        planningSubject(record),
        record.projectionEpoch,
        record.observationRevision,
      )) return undefined

  const attempt = new AuthorityCommitTransaction({
    action: record.resolution,
    observationRevision: record.observationRevision,
    authority: record.authority,
    assertFinalFence: transaction => assertFrozenAuthorityFence(record, transaction, context),
    ...(context.binder === undefined ? {} : { binder: context.binder }),
  })
  record.commitAttempt = attempt
  return attempt
}

/** Returns the exact current selection only while every pre-click and observation fence still holds. */
function assertFrozenAuthorityFence(
  record: AuthorityActivationRecord,
  attempt: AuthorityCommitTransaction,
  context: FrozenAuthorityCommitContext,
): SelectionSpec {
  const active = context.currentProjection()
  const joined = context.currentJoinedShare()
  if (context.currentActivation() !== record || record.terminal !== undefined ||
      record.commitAttempt !== attempt || record.observationReplacementPending ||
      context.joinSuspended() || !observationMatchesActivation(record, active, joined) ||
      !context.planning.readyForCommit(
        planningSubject(record),
        record.projectionEpoch,
        record.observationRevision,
      ) || record.resolution !== attempt.action ||
      !sameArtifactChoiceSemantics(record.choice, attempt.action.choice) ||
      !sameMaterializationRouteIdentity(
        record.installedRoute,
        materializationRouteIdentity(attempt.action.route),
      )) {
    throw new StaleReceiveBoundaryError()
  }
  return active.selection
}

/**
 * Revalidates the bound operation against the frozen route before constructing
 * adoption state; a physical runtime cannot substitute for semantic identity.
 */
export function frozenBoundOperation(
  record: AuthorityActivationRecord,
  attempt: AuthorityCommitTransaction,
  result: Extract<AuthorityCommitOutcome, { kind: 'bound-operation' }>,
  active: V2ActiveProjection,
): FrozenBoundOperation | undefined {
  const resolution = record.resolution
  if (resolution === undefined ||
      !sameArtifactChoiceSemantics(record.choice, resolution.choice) ||
      !sameMaterializationRouteIdentity(
        record.installedRoute,
        materializationRouteIdentity(resolution.route),
      ) || resolution.artifact.digest !== attempt.action.artifact.digest ||
      result.intent.artifact.digest !== resolution.artifact.digest) return undefined

  const runtime = result.operation
  return Object.freeze({
    runtime,
    intent: result.intent,
    adoption: {
      joined: record.joined,
      selection: active.frozenSelection,
      runtime,
      ...(runtime.repairProjection === undefined
        ? {}
        : { repairProjection: runtime.repairProjection }),
    },
  })
}
