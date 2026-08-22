import { encodeBase64Url } from '../../crypto/bytes'
import type {
  ArtifactChoiceReconcileOutcome,
  MaterializationRouteIdentity,
  OfferedArtifactChoice,
  ResolvedArtifactAction,
} from '../../output/planning'
import type { ReceiveIntent } from '../../transfer/intent'
import type { ArtifactChoiceID } from '../../transfer/intent'
import type { ProjectionEpoch } from '../../transfer/projection'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
  V2ReceiveCompositionPort,
} from '../v2-receive-runtime'
import type { ActiveReceiveCoordinator } from './active-receive'
import type {
  V2AuthorityActivationSnapshot,
  V2ActivationCleanupStage,
  V2AuthorityActivationTerminalOutcome,
  V2LiveAuthorityActivationSnapshot,
} from './activation-model'
import {
  advanceActivationCleanup,
  type ActivationCleanupOwner,
  type AuthorityCommitTransaction,
  type ReceiveIntentBinder,
} from './authority-commit'
import type {
  V2AuthorityActivationTraceEvent,
  V2ControllerWorkflowTraceEvent,
} from './contracts'
import type { V2ControllerObservability } from './controller-observability'
import type { V2PresentationAttempt } from './presentation-attempt'
import type { V2ActiveProjection } from './projection-observation'
import type {
  ArtifactChoiceReconciler,
  ArtifactOfferPlanner,
  V2AuthorityProjectionPublication,
} from './authority-planning'

const ACTIVATION_ID_BYTES = 16

export interface AuthorityActivationOptions {
  readonly receive: V2ReceiveCompositionPort
  readonly activeReceive: Pick<ActiveReceiveCoordinator, 'prepareAdoption'>
  readonly observability: V2ControllerObservability
  readonly currentProjection: () => V2ActiveProjection | undefined
  readonly currentJoinedShare: () => V2JoinedBrowserShare | undefined
  readonly choiceBlocked: () => boolean
  readonly retryProjection: (projection: V2ActiveProjection) => void
  readonly publishProjection: (publication: V2AuthorityProjectionPublication) => void
  readonly adoptReceiveIntent: (
    choice: OfferedArtifactChoice['choice'],
    intent: ReceiveIntent,
    runtime: V2BoundReceiveOperation,
    commitOwnership: () => void,
  ) => boolean
  readonly refreshRetainedInventory: () => void
  readonly publishActionError: (error: unknown) => void
  readonly planner?: ArtifactOfferPlanner
  readonly reconciler?: ArtifactChoiceReconciler
  readonly binder?: ReceiveIntentBinder
  readonly createActivationId?: () => string
}

export type AuthorityAttemptExclusion =
  | 'success'
  | 'cancelled'
  | 'picker_refused'
  | 'authority_invalidated'

export interface AuthorityActivationRecord {
  readonly activationId: string
  readonly authenticatedShareInstanceId: string
  readonly selectionDigest: string
  readonly choice: OfferedArtifactChoice['choice']
  readonly installedRoute: MaterializationRouteIdentity
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly controller: AbortController
  readonly attempt: V2PresentationAttempt
  joined: V2JoinedBrowserShare
  observationRevision: number
  protocolSessionId: string
  projectionEpoch: ProjectionEpoch
  observationReplacementPending: boolean
  authority?: V2ArtifactPresentationAuthority
  authorityReady: boolean
  resolution?: ResolvedArtifactAction
  retryReason?: Extract<ArtifactChoiceReconcileOutcome, { kind: 'retry-required' }>['reason']
  commitAttempt?: AuthorityCommitTransaction
  cleanup?: ActivationCleanupOwner
  terminal?: V2AuthorityActivationTerminalOutcome
  failureReported: boolean
  released: boolean
  attemptClosed: boolean
}

type AuthorityTraceContextKey =
  | 'name'
  | 'activationId'
  | 'authenticatedShareInstanceId'
  | 'selectionDigest'
  | 'observedProtocolSessionId'
  | 'projectionEpoch'
  | 'observationRevision'
  | 'artifactKind'
  | 'planKind'

type WithoutAuthorityTraceContext<Event> = Event extends unknown
  ? Omit<Event, AuthorityTraceContextKey>
  : never

export type AuthorityTraceDetail = WithoutAuthorityTraceContext<V2AuthorityActivationTraceEvent>

export interface ActivationCleanupHooks {
  readonly completed: () => void
  readonly pending: (stage: V2ActivationCleanupStage) => void
}

export function createAuthorityActivationRecord(input: Readonly<{
  activationId: string
  joined: V2JoinedBrowserShare
  choice: OfferedArtifactChoice['choice']
  installedRoute: MaterializationRouteIdentity
  preClickRanking: readonly ArtifactChoiceID[]
  authenticatedShareInstanceId: string
  selectionDigest: string
  observationRevision: number
  protocolSessionId: string
  projectionEpoch: ProjectionEpoch
  attempt: V2PresentationAttempt
}>): AuthorityActivationRecord {
  return {
    ...input,
    controller: new AbortController(),
    observationReplacementPending: false,
    authorityReady: false,
    failureReported: false,
    released: false,
    attemptClosed: false,
  }
}

export function observationMatchesActivation(
  record: AuthorityActivationRecord,
  active: V2ActiveProjection | undefined,
  joined: V2JoinedBrowserShare | undefined,
): active is V2ActiveProjection {
  return active !== undefined && joined !== undefined && active.joined === record.joined &&
    joined === record.joined &&
    active.selection.shareInstance === record.authenticatedShareInstanceId &&
    active.selection.digest === record.selectionDigest &&
    active.protocolSessionId === record.protocolSessionId && active.epoch === record.projectionEpoch
}

export function projectLiveActivation(record: AuthorityActivationRecord): Readonly<{
  snapshot: V2AuthorityActivationSnapshot
  waitingFor?: 'authority' | 'resolution' | 'authority-and-resolution'
}> {
  const live = liveAuthoritySnapshot(record)
  const attempt = record.commitAttempt
  if (attempt !== undefined) {
    return Object.freeze({
      snapshot: Object.freeze({ ...live, kind: 'committing', action: attempt.action }),
    })
  }
  if (record.retryReason !== undefined) {
    return Object.freeze({
      snapshot: Object.freeze({
        ...live,
        kind: 'retry-required',
        authorityReady: record.authorityReady,
        reason: record.retryReason,
      }),
    })
  }
  if (!record.authorityReady) {
    return Object.freeze({
      snapshot: Object.freeze({
        ...live,
        kind: 'waiting-authority',
        resolution: record.resolution === undefined
          ? Object.freeze({ kind: 'waiting' })
          : Object.freeze({ kind: 'resolved', action: record.resolution }),
      }),
      waitingFor: record.resolution === undefined ? 'authority-and-resolution' : 'authority',
    })
  }
  return Object.freeze({
    snapshot: Object.freeze({ ...live, kind: 'waiting-resolution' }),
    waitingFor: 'resolution',
  })
}

export function terminalAuthoritySnapshot(
  record: AuthorityActivationRecord,
): V2AuthorityActivationSnapshot {
  if (record.terminal === undefined) throw new TypeError('terminal activation lacks an outcome')
  return Object.freeze({
    ...liveAuthoritySnapshot(record),
    kind: 'terminal',
    outcome: record.terminal,
  })
}

export function cleanupRequiredSnapshot(
  record: AuthorityActivationRecord,
  failedStage: V2ActivationCleanupStage,
): V2AuthorityActivationSnapshot {
  const cleanup = record.cleanup
  if (cleanup === undefined) throw new TypeError('cleanup projection lacks an owner')
  return Object.freeze({
    ...liveAuthoritySnapshot(record),
    kind: 'cleanup-required',
    operationId: cleanup.operationId,
    ownerKind: cleanup.kind,
    failedStage,
    settlementComplete: cleanup.settlementComplete,
    detachComplete: cleanup.detachComplete,
  })
}

export function runActivationCleanup(
  record: AuthorityActivationRecord,
  cleanup: ActivationCleanupOwner,
  observability: V2ControllerObservability,
  refreshRetainedInventory: () => void,
  hooks: ActivationCleanupHooks,
): boolean {
  if (cleanup.running) return false
  cleanup.running = true
  advanceActivationCleanup(cleanup).then(progress => {
    cleanup.running = false
    for (const stage of progress.failedStages) {
      observability.recordConsequence(record.attempt, stage)
      traceAuthorityTransition(observability, record, {
        transition: 'cleanup_failed',
        receiverOperationId: cleanup.operationId,
        failedStage: stage,
      })
    }
    if (progress.detachedNow) {
      try {
        refreshRetainedInventory()
      } catch {
        observability.recordConsequence(record.attempt, 'cleanup')
      }
    }
    if (progress.complete) {
      hooks.completed()
      return
    }
    hooks.pending(cleanup.settlementComplete ? 'detach' : 'settlement')
  }).catch(() => undefined)
  return true
}

export async function releaseActivationAuthority(
  record: AuthorityActivationRecord,
  observability: V2ControllerObservability,
  reason: unknown,
): Promise<void> {
  if (record.released) return
  record.released = true
  if (record.authority === undefined) return
  try {
    await Promise.resolve(record.authority.release(reason))
  } catch {
    observability.recordConsequence(record.attempt, 'cleanup')
  }
}

export function closeActivationAttempt(
  record: AuthorityActivationRecord,
  observability: V2ControllerObservability,
): void {
  if (record.attemptClosed) return
  record.attemptClosed = true
  if (!record.attempt.decisionSettled) {
    observability.exclude(record.attempt, 'projection_authority', 'stale_replacement')
  }
  record.attempt.close()
}

export function traceAuthorityTransition(
  observability: V2ControllerObservability,
  record: AuthorityActivationRecord,
  detail: AuthorityTraceDetail,
): void {
  observability.trace(() => Object.freeze({
    name: 'authority_transition',
    activationId: record.activationId,
    authenticatedShareInstanceId: record.authenticatedShareInstanceId,
    selectionDigest: record.selectionDigest,
    observedProtocolSessionId: record.protocolSessionId,
    projectionEpoch: record.projectionEpoch,
    observationRevision: record.observationRevision,
    artifactKind: record.choice.artifactKind,
    planKind: record.choice.plan.kind,
    ...detail,
  }) as Extract<V2ControllerWorkflowTraceEvent, { name: 'authority_transition' }>)
}

export function createActivationId(): string {
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new DOMException('Secure activation identity generation is unavailable', 'NotSupportedError')
  }
  const value = new Uint8Array(ACTIVATION_ID_BYTES)
  globalThis.crypto.getRandomValues(value)
  if (value.every(byte => byte === 0)) throw new Error('Generated activation identity was all zeroes')
  return encodeBase64Url(value)
}

function liveAuthoritySnapshot(record: AuthorityActivationRecord): V2LiveAuthorityActivationSnapshot {
  return Object.freeze({
    activationId: record.activationId,
    authenticatedShareInstanceId: record.authenticatedShareInstanceId,
    selectionDigest: record.selectionDigest,
    choice: record.choice,
    installedRoute: record.installedRoute,
    preClickRanking: record.preClickRanking,
    observation: Object.freeze({
      revision: record.observationRevision,
      protocolSessionId: record.protocolSessionId,
      projectionEpoch: record.projectionEpoch,
    }),
  })
}
