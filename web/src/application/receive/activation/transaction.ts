import {
  materializationRouteIdentity,
  sameArtifactChoiceSemantics,
  sameMaterializationRouteIdentity,
  type ArtifactActionsOffer,
  type ArtifactChoice,
  type OfferedArtifactChoice,
} from '../../../output/planning'
import type { ArtifactChoiceID, ReceiveIntent, SelectionSpec } from '../../../transfer/intent'
import type { ReceivePlanningShare } from '../joined-share'
import type { BoundReceiveOperation } from '../contracts'
import type { ActiveReceiveAdoption } from '../active/coordinator'
import { ActivationStateContractError } from './model'
import {
  AuthorityCommitTransaction,
  type AuthorityCommitOutcome,
  type ReceiveIntentBinder,
} from './commit'
import {
  createAuthorityActivationRecord,
  observationMatchesActivation,
  type AuthorityActivationRecord,
} from './lifecycle'
import { planningSubject, type AuthorityPlanningPipeline } from './planning'
import { StaleReceiveBoundaryError } from '../observation/contracts'
import type { ActiveProjection } from '../discovery'
import type { PresentationAttempt } from '../observation/attempt'

export interface FrozenAuthorityCommitContext {
  readonly currentActivation: () => AuthorityActivationRecord | undefined
  readonly currentProjection: () => ActiveProjection | undefined
  readonly currentJoinedShare: () => ReceivePlanningShare | undefined
  readonly joinSuspended: () => boolean
  readonly planning: AuthorityPlanningPipeline
  readonly binder?: ReceiveIntentBinder
}

export interface FrozenBoundOperation {
  readonly runtime: BoundReceiveOperation
  readonly intent: ReceiveIntent
  readonly adoption: ActiveReceiveAdoption
}

/** Constructs the record the coordinator installs before invoking reentrant presentation authority. */
export function createProvisionalAuthorityActivation(input: Readonly<{
  activationId: string
  active: ActiveProjection
  offered: OfferedArtifactChoice
  preClickRanking: readonly ArtifactChoiceID[]
  observationRevision: number
  attempt: PresentationAttempt
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
export function offeredArtifactChoiceRanking(
  offers: ArtifactActionsOffer,
  selected: ArtifactChoice,
): readonly ArtifactChoiceID[] {
  const displayed = selected.artifactKind === 'zip-archive' && offers.zip !== null
    ? [offers.zip.primary, ...(offers.zip.secondary === null ? [] : [offers.zip.secondary])]
    : [offers.primary, ...offers.alternatives]
        .filter(candidate => candidate.choice.artifactKind !== 'zip-archive')
  const ranking = displayed.map(candidate => candidate.choice.choiceId)
  if (!ranking.includes(selected.choiceId) || new Set(ranking).size !== ranking.length) {
    throw new ActivationStateContractError(
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
  active: ActiveProjection,
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
