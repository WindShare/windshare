import { activationOwnsChoice } from './activation/model'
import { sameReceiveLifecycleState } from '../../output/workspace/lifecycle/observation'
import { sameArtifactChoiceSemantics } from '../../output/planning/guarantees'
import { type ArtifactChoice, type OfferedArtifactChoice } from '../../output/model/artifacts/choices'
import { type ArtifactSizeProjection } from '../../output/model/artifacts/projections'
import { type ArtifactOffers } from '../../output/model/artifacts/offers'
import { type ResolvedArtifactAction } from '../../output/planning/contracts'
import {
  safeRenamedSummary,
  type SafeRenamedSummary,
} from '../../transfer/lifecycle/safe-names'
import type {
  RecoverySummary,
} from '../../transfer/lifecycle/recovery-summary'
import type { ReceiveLifecycleState } from '../../output/workspace'
import type {
  ArtifactSpec,
  MaterializationPlan,
  ReceiveIntent,
} from '../../transfer/intent'
import type { SelectionProjectionState } from '../../transfer/projection/contracts'
import type { TransferJobResult } from '../../transfer/job/contract'
import { PersistentWriterOpenPauseRequestedError } from '../../output/execution/persistent-file-transaction'
import type { AuthorityActivationSnapshot } from './activation/model'
import { type ActiveReceiveControl, type ReceiveSettlementWait, type WorkspaceUsage, type PersistentWriterOpenPauseFact } from './actions'
import type { ProgressChange } from '../../transfer/progress/observation'
import { folderProgressChange } from './observation/progress-change'

const INACTIVE_ACTIVATION: AuthorityActivationSnapshot = Object.freeze({ kind: 'inactive' })

export interface ReceiveRuntimeSnapshot {
  readonly transferResult: TransferResultProjection | null
  readonly activeOfferedChoice: OfferedArtifactChoice | null
  readonly projectionRevision: number | null
  readonly projection: SelectionProjectionState | null
  readonly offers: ArtifactOffers | null
  readonly activation: AuthorityActivationSnapshot
  readonly chosenChoice: ArtifactChoice | null
  readonly chosenSizeProjection: ArtifactSizeProjection | null
  readonly resolvedArtifact: ArtifactSpec | null
  readonly receiveIntent: ReceiveIntent | null
  readonly plan: MaterializationPlan | null
  readonly lifecycle: ReceiveLifecycleState | null
  readonly safeRenamed: SafeRenamedSummary | null
  readonly recoverySummary: RecoverySummary | null
  readonly writerOpenPause: PersistentWriterOpenPauseFact | null
  readonly workspaceUsage: WorkspaceUsage | null
  readonly activeControls: readonly ActiveReceiveControl[]
  readonly receiveInterruption: ReceiveSettlementWait | null
  readonly browserFolderProgress?: import('./contracts').BrowserFolderProgressSnapshot | null
}

export const EMPTY_RECEIVE_RUNTIME: ReceiveRuntimeSnapshot = Object.freeze({
  transferResult: null,
  activeOfferedChoice: null,
  projectionRevision: null,
  projection: null,
  offers: null,
  activation: INACTIVE_ACTIVATION,
  chosenChoice: null,
  chosenSizeProjection: null,
  resolvedArtifact: null,
  receiveIntent: null,
  plan: null,
  lifecycle: null,
  safeRenamed: null,
  recoverySummary: null,
  writerOpenPause: null,
  workspaceUsage: null,
  activeControls: Object.freeze([]),
  receiveInterruption: null,
  browserFolderProgress: null,
})

export type TransferResultProjection = Pick<
  TransferJobResult,
  'worker' | 'intent' | 'transferJobId' | 'lifecycle' | 'safeRenamed' | 'recoverySummary' |
    'abortReason' | 'measure'
>

/**
 * Owns the observed task facts and their generation fences. Durable transitions
 * still belong to output providers; rendering is a passive subscriber.
 */
export class ReceiveRuntimeState {
  readonly #listeners = new Set<() => void>()
  #snapshot = EMPTY_RECEIVE_RUNTIME
  #activeOfferedChoice: OfferedArtifactChoice | null = null
  #resolvedAction: ResolvedArtifactAction | null = null
  readonly #changed: (change: ProgressChange) => void

  constructor(changed?: (change: ProgressChange) => void) {
    this.#changed = changed ?? (() => this.notifyPresentation())
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): ReceiveRuntimeSnapshot => this.#snapshot

  updateProjection(
    revision: number,
    state: SelectionProjectionState,
    offers: ArtifactOffers,
  ): boolean {
    assertObservationRevision(revision)
    const currentRevision = this.#snapshot.projectionRevision
    if (currentRevision !== null && revision <= currentRevision) return false
    assertOffersBelongToProjection(state, offers)

    if (activationOwnsChoice(this.#snapshot.activation)) {
      this.#activeOfferedChoice =
        findOfferedChoice(offers, this.#snapshot.activation.choice) ?? this.#activeOfferedChoice
    }

    this.#publish(Object.freeze({
      ...this.#snapshot,
      activeOfferedChoice: this.#activeOfferedChoice,
      projectionRevision: revision,
      projection: state,
      offers,
    }))
    return true
  }

  updateActivation(activation: AuthorityActivationSnapshot): void {
    const previous = this.#snapshot.activation
    if (activationOwnsChoice(activation)) {
      const activationChanged = !activationOwnsChoice(previous) ||
        previous.activationId !== activation.activationId
      if (activationChanged) {
        this.#activeOfferedChoice = findOfferedChoice(this.#snapshot.offers, activation.choice)
      } else {
        this.#activeOfferedChoice =
          findOfferedChoice(this.#snapshot.offers, activation.choice) ?? this.#activeOfferedChoice
      }
    } else {
      this.#activeOfferedChoice = null
    }

    const resolvedAction = resolvedActionFromActivation(activation)
    if (resolvedAction !== null) {
      this.#resolvedAction = resolvedAction
    } else if (activation.kind !== 'terminal' || activation.outcome.kind !== 'bound-operation') {
      this.#resolvedAction = null
    }
    const choicePresented = activationOwnsChoice(activation) ||
      (activation.kind === 'terminal' && activation.outcome.kind === 'bound-operation')
    let chosenChoice = this.#snapshot.chosenChoice
    let chosenSizeProjection = this.#snapshot.chosenSizeProjection
    if (this.#snapshot.receiveIntent === null) {
      chosenChoice = choicePresented ? activation.choice : null
      chosenSizeProjection = choicePresented
        ? this.#activeOfferedChoice?.sizeProjection ?? chosenSizeProjection : null
    }
    this.#publish(Object.freeze({
      ...this.#snapshot,
      activation,
      activeOfferedChoice: this.#activeOfferedChoice,
      chosenChoice,
      chosenSizeProjection,
      resolvedArtifact: this.#snapshot.receiveIntent?.artifact ?? this.#resolvedAction?.artifact ?? null,
    }))
  }

  adoptReceiveIntent(
    choice: ArtifactChoice,
    intent: ReceiveIntent,
    lifecycle?: ReceiveLifecycleState,
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly ActiveReceiveControl[] = Object.freeze([]),
  ): boolean {
    return this.adoptReceiveIntentAtomically(
      choice,
      intent,
      () => undefined,
      lifecycle,
      workspaceUsage,
      activeControls,
    )
  }

  adoptReceiveIntentAtomically(
    choice: ArtifactChoice,
    intent: ReceiveIntent,
    commitOwnership: () => void,
    lifecycle?: ReceiveLifecycleState,
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly ActiveReceiveControl[] = Object.freeze([]),
  ): boolean {
    const activation = this.#snapshot.activation
    if (activation.kind === 'inactive' ||
        !sameArtifactChoiceSemantics(activation.choice, choice)) return false
    assertIntentMatchesChoice(choice, intent)
    if (this.#resolvedAction !== null &&
        (!sameArtifactChoiceSemantics(this.#resolvedAction.choice, choice) ||
         this.#resolvedAction.artifact.digest !== intent.artifact.digest)) {
      throw new TypeError('bound receive intent does not match the coordinator-owned resolved action')
    }
    const snapshot = this.#buildLifecycleSnapshot({
      ...this.#snapshot,
      chosenChoice: choice,
      resolvedArtifact: intent.artifact,
      receiveIntent: intent,
      transferResult: null,
      plan: intent.plan,
      browserFolderProgress: null,
      safeRenamed: null,
      recoverySummary: null,
      writerOpenPause: null,
      activeControls: Object.freeze([...activeControls]),
      receiveInterruption: null,
    }, lifecycle ?? null, workspaceUsage)
    this.#publishWithOwnership(snapshot, commitOwnership)
    return true
  }

  adoptRetainedReceiveIntent(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly ActiveReceiveControl[] = Object.freeze([]),
  ): void {
    this.adoptRetainedReceiveIntentAtomically(
      intent,
      lifecycle,
      () => undefined,
      workspaceUsage,
      activeControls,
    )
  }

  adoptRetainedReceiveIntentAtomically(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    commitOwnership: () => void,
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly ActiveReceiveControl[] = Object.freeze([]),
  ): void {
    if (lifecycle.operationId !== intent.operationId ||
        lifecycle.receiveIntentDigest !== intent.digest) {
      throw new TypeError('retained lifecycle does not belong to its validated receive intent')
    }
    const snapshot = this.#buildLifecycleSnapshot({
      ...this.#snapshot,
      activation: INACTIVE_ACTIVATION,
      activeOfferedChoice: null,
      chosenChoice: null,
      chosenSizeProjection: null,
      resolvedArtifact: intent.artifact,
      receiveIntent: intent,
      transferResult: null,
      plan: intent.plan,
      browserFolderProgress: null,
      safeRenamed: null,
      recoverySummary: null,
      writerOpenPause: null,
      activeControls: Object.freeze([...activeControls]),
      receiveInterruption: null,
    }, lifecycle, workspaceUsage)
    this.#activeOfferedChoice = null
    this.#resolvedAction = null
    this.#publishWithOwnership(snapshot, commitOwnership)
  }

  observeLifecycle(state: ReceiveLifecycleState): boolean {
    const intent = this.#snapshot.receiveIntent
    const current = this.#snapshot.lifecycle
    if (intent === null || state.operationId !== intent.operationId ||
        state.receiveIntentDigest !== intent.digest ||
        (current !== null && state.generation <= current.generation)) return false
    // A committed state is a durable observation, not permission to clear interruption
    // controls or release the still-running writer and its connectivity lease.
    this.#publishLifecycleSnapshot(this.#snapshot, state, this.#snapshot.workspaceUsage)
    return true
  }

  settleLifecycle(
    state: ReceiveLifecycleState,
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly ActiveReceiveControl[] = Object.freeze([]),
    safeRenamed?: SafeRenamedSummary | null,
    recoverySummary?: RecoverySummary | null,
  ): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || state.operationId !== intent.operationId ||
        state.receiveIntentDigest !== intent.digest) return false
    const current = this.#snapshot.lifecycle
    if (current !== null && (state.generation < current.generation ||
        (state.generation === current.generation && !sameReceiveLifecycleState(state, current)))) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      safeRenamed: nextSafeRenamed(this.#snapshot.safeRenamed, safeRenamed),
      // Recovery costs authenticate one exact stable generation and cannot flow
      // through a later lifecycle observation without a new checkpoint snapshot.
      recoverySummary: recoverySummary ?? null,
      writerOpenPause: null,
      activeControls: Object.freeze([...activeControls]),
      receiveInterruption: null,
    }, state, workspaceUsage)
    return true
  }

  updateActiveControls(controls: readonly ActiveReceiveControl[]): boolean {
    const lifecycle = this.#snapshot.lifecycle
    if (lifecycle === null) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      activeControls: Object.freeze([...controls]),
    }, lifecycle, this.#snapshot.workspaceUsage)
    return true
  }

  updateReceiveInterruption(interruption: ReceiveSettlementWait | null): boolean {
    const lifecycle = this.#snapshot.lifecycle
    if (lifecycle === null) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      receiveInterruption: interruption === null ? null : Object.freeze({ ...interruption }),
    }, lifecycle, this.#snapshot.workspaceUsage)
    return true
  }

  updateBrowserFolderProgress(progress: import('./contracts').BrowserFolderProgressSnapshot): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || intent.plan.kind !== 'direct-tree' ||
        progress.operationId !== intent.operationId ||
        progress.summary.operationId !== intent.operationId ||
        progress.summary.receiveIntentDigest !== intent.digest) return false
    const current = this.#snapshot.browserFolderProgress
    if (current != null && progress.generation <= current.generation) return false
    this.#publish({
      ...this.#snapshot, browserFolderProgress: Object.freeze({ ...progress }),
    }, folderProgressChange(current, progress))
    return true
  }

  adoptTransferResult(result: TransferResultProjection): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || result.intent.operationId !== intent.operationId ||
        result.intent.digest !== intent.digest ||
        result.lifecycle.operationId !== intent.operationId ||
        result.lifecycle.receiveIntentDigest !== intent.digest) return false
    const currentLifecycle = this.#snapshot.lifecycle
    if (currentLifecycle !== null && result.lifecycle.generation < currentLifecycle.generation) {
      return false
    }
    const safeRenamed = result.safeRenamed === undefined
      ? null
      : safeRenamedSummary(result.safeRenamed)
    const recoverySummary = result.recoverySummary ?? null
    const writerOpenPause = persistentWriterOpenPauseFact(result.abortReason)
    const snapshot = this.#buildLifecycleSnapshot({
      ...this.#snapshot,
      safeRenamed,
      recoverySummary,
      writerOpenPause,
      receiveInterruption: null,
    }, result.lifecycle, this.#snapshot.workspaceUsage)
    this.#publish(Object.freeze({
      ...snapshot,
      transferResult: Object.freeze({ ...result }),
    }))
    return true
  }

  clearTask(): void {
    const { projectionRevision, projection, offers } = this.#snapshot
    this.#activeOfferedChoice = null
    this.#resolvedAction = null
    this.#publish(Object.freeze({ ...EMPTY_RECEIVE_RUNTIME,
      projectionRevision, projection, offers }))
  }

  resetDraft(): void {
    // A replaceable catalog estimate owns no committed output or progress.
    this.#publish(Object.freeze({
      ...this.#snapshot,
      projectionRevision: null,
      projection: null,
      offers: null,
    }))
  }

  reset(): void {
    this.#activeOfferedChoice = null
    this.#resolvedAction = null
    this.#publish(EMPTY_RECEIVE_RUNTIME)
  }

  close(): void {
    this.reset()
    this.#listeners.clear()
  }

  #publishLifecycleSnapshot(
    base: ReceiveRuntimeSnapshot,
    lifecycle: ReceiveLifecycleState | null,
    workspaceUsage: WorkspaceUsage | null | undefined,
  ): void {
    this.#publish(this.#buildLifecycleSnapshot(base, lifecycle, workspaceUsage))
  }

  #buildLifecycleSnapshot(
    base: ReceiveRuntimeSnapshot,
    lifecycle: ReceiveLifecycleState | null,
    workspaceUsage: WorkspaceUsage | null | undefined,
  ): ReceiveRuntimeSnapshot {
    const artifact = base.resolvedArtifact
    const plan = base.plan
    if (lifecycle === null) {
      return Object.freeze({
        ...base,
        lifecycle: null,
        recoverySummary: null,
        writerOpenPause: null,
        workspaceUsage: null,
        activeControls: Object.freeze([]),
        receiveInterruption: null,
      })
    }
    if (artifact === null || plan === null) {
      throw new TypeError('lifecycle facts require a bound artifact and plan')
    }
    return Object.freeze({
      ...base,
      lifecycle,
      recoverySummary: matchingRecoverySummary(lifecycle, base.recoverySummary),
      workspaceUsage: workspaceUsage ?? null,
    })
  }

  #publishWithOwnership(snapshot: ReceiveRuntimeSnapshot, commitOwnership: () => void): void {
    // Ownership is installed before observers can see the corresponding intent.
    commitOwnership()
    this.#publish(snapshot)
  }

  #publish(snapshot: ReceiveRuntimeSnapshot, change: ProgressChange = 'transition'): void {
    this.#snapshot = Object.freeze(snapshot)
    try { this.#changed(change) } catch {
      // Presentation failure cannot roll back facts used by output commands.
    }
  }

  notifyPresentation(): void {
    for (const listener of this.#listeners) {
      try { listener() } catch {
        // A view or diagnostics subscriber cannot invalidate accepted task facts.
      }
    }
  }
}

function persistentWriterOpenPauseFact(reason: unknown): PersistentWriterOpenPauseFact | null {
  if (!(reason instanceof PersistentWriterOpenPauseRequestedError)) return null
  return Object.freeze({
    materializationRelativePath: Object.freeze([...reason.materializationRelativePath]),
    cost: Object.freeze({ ...reason.cost }),
    purpose: reason.purpose,
  })
}

function matchingRecoverySummary(
  lifecycle: ReceiveLifecycleState,
  summary: RecoverySummary | null,
): RecoverySummary | null {
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') return null
  if (summary === null) return null
  if (summary.lifecycleGeneration !== lifecycle.generation ||
      summary.checkpointSetDigest !== lifecycle.checkpointSetDigest ||
      summary.completedFileCount !== lifecycle.completedFileCount ||
      summary.completedBytes !== lifecycle.completedBytes) {
    throw new TypeError('recovery summary escaped its lifecycle generation')
  }
  return summary
}

function assertObservationRevision(revision: number): void {
  if (!Number.isSafeInteger(revision) || revision < 0) {
    throw new TypeError('projection observation revision must be a non-negative safe integer')
  }
}

function assertOffersBelongToProjection(
  state: SelectionProjectionState,
  offers: ArtifactOffers,
): void {
  if (offers.projectionEpoch !== state.projection.epoch ||
      offers.selectionDigest !== state.projection.selectionDigest) {
    throw new TypeError('artifact offers do not belong to the supplied projection observation')
  }
}

function assertIntentMatchesChoice(choice: ArtifactChoice, intent: ReceiveIntent): void {
  if (choice.artifactKind !== intent.artifact.kind || choice.plan.kind !== intent.plan.kind) {
    throw new TypeError('bound receive intent does not match the coordinator-owned artifact choice')
  }
}

function findOfferedChoice(
  offers: ArtifactOffers | null,
  choice: ArtifactChoice,
): OfferedArtifactChoice | null {
  if (offers?.kind !== 'artifact-actions') return null
  return [offers.primary, ...offers.alternatives].find((candidate) =>
    sameArtifactChoiceSemantics(candidate.choice, choice)) ?? null
}

function nextSafeRenamed(
  current: SafeRenamedSummary | null,
  next: SafeRenamedSummary | null | undefined,
): SafeRenamedSummary | null {
  if (next === undefined) return current
  return next === null ? null : safeRenamedSummary(next)
}

function resolvedActionFromActivation(
  activation: AuthorityActivationSnapshot,
): ResolvedArtifactAction | null {
  switch (activation.kind) {
    case 'waiting-authority':
      return activation.resolution.kind === 'resolved'
        ? activation.resolution.action
        : null
    case 'committing':
      return activation.action
    case 'inactive':
    case 'waiting-resolution':
    case 'retry-required':
    case 'cleanup-required':
    case 'terminal':
      return null
  }
}
