import {
  sameArtifactChoiceSemantics,
  type ArtifactChoice,
  type ArtifactSizeProjection,
  type ArtifactOffers,
  type OfferedArtifactChoice,
  type ResolvedArtifactAction,
} from '../output/planning'
import {
  compatibleNameRepairSummary,
  type CompatibleNameRepairSummary,
} from '../output/file-system-access/compatible-name/model'
import {
  isTerminalLifecycleState,
  lifecycleDeadline,
  type ReceiveLifecycleState,
} from '../output/workspace'
import type {
  ArtifactSpec,
  MaterializationPlan,
  ReceiveIntent,
} from '../transfer/intent'
import type { SelectionProjectionState } from '../transfer/projection'
import type { TransferJobResult } from '../transfer/job/contract'
import type { V2AuthorityActivationSnapshot } from './controller/activation-model'
import {
  activationPresentsChoice,
  presentArtifactActivation,
  presentArtifactOffers,
  type ArtifactActivationPresentation,
  type ArtifactOfferPresentation,
} from './v2-artifact-presentation'
import {
  presentReceiveLifecycle,
  type ReceiveLifecyclePresentation,
  type V2ActiveReceiveControl,
  type WorkspaceUsage,
} from './v2-lifecycle-presentation'
import {
  presentTransferResult,
  type TransferResultPresentation,
} from './v2-transfer-result'
import type { V2DirectZipProgressSnapshot } from './v2-receive-runtime'

const INACTIVE_ACTIVATION: V2AuthorityActivationSnapshot = Object.freeze({ kind: 'inactive' })

export interface V2OutputPresentationSnapshot {
  readonly projectionRevision: number | null
  readonly projection: SelectionProjectionState | null
  readonly offers: ArtifactOffers | null
  readonly offerPresentation: ArtifactOfferPresentation | null
  readonly activation: V2AuthorityActivationSnapshot
  readonly activationPresentation: ArtifactActivationPresentation | null
  readonly chosenChoice: ArtifactChoice | null
  readonly chosenSizeProjection: ArtifactSizeProjection | null
  readonly resolvedArtifact: ArtifactSpec | null
  readonly receiveIntent: ReceiveIntent | null
  readonly plan: MaterializationPlan | null
  readonly lifecycle: ReceiveLifecycleState | null
  readonly repairSummary: CompatibleNameRepairSummary | null
  readonly lifecyclePresentation: ReceiveLifecyclePresentation | null
  readonly expiresAt: number | null
  readonly workspaceUsage: WorkspaceUsage | null
  readonly activeControls: readonly V2ActiveReceiveControl[]
  readonly directZipProgress: V2DirectZipProgressSnapshot | null
  readonly transferResultPresentation: TransferResultPresentation | null
}

export const EMPTY_V2_OUTPUT_PRESENTATION: V2OutputPresentationSnapshot = Object.freeze({
  projectionRevision: null,
  projection: null,
  offers: null,
  offerPresentation: null,
  activation: INACTIVE_ACTIVATION,
  activationPresentation: null,
  chosenChoice: null,
  chosenSizeProjection: null,
  resolvedArtifact: null,
  receiveIntent: null,
  plan: null,
  lifecycle: null,
  repairSummary: null,
  lifecyclePresentation: null,
  expiresAt: null,
  workspaceUsage: null,
  activeControls: Object.freeze([]),
  directZipProgress: null,
  transferResultPresentation: null,
})

export type TransferResultProjection = Pick<
  TransferJobResult,
  'worker' | 'intent' | 'transferJobId' | 'lifecycle' | 'repairSummary'
>

/**
 * Presentation receives already-fenced coordinator facts. It never starts,
 * retries, releases, or decides admission for output authority.
 */
export class V2OutputPresentationController {
  readonly #listeners = new Set<() => void>()
  #snapshot = EMPTY_V2_OUTPUT_PRESENTATION
  #activeOfferedChoice: OfferedArtifactChoice | null = null
  #resolvedAction: ResolvedArtifactAction | null = null

  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  readonly getSnapshot = (): V2OutputPresentationSnapshot => this.#snapshot

  updateProjection(
    revision: number,
    state: SelectionProjectionState,
    offers: ArtifactOffers,
  ): boolean {
    assertObservationRevision(revision)
    const currentRevision = this.#snapshot.projectionRevision
    if (currentRevision !== null && revision <= currentRevision) return false
    assertOffersBelongToProjection(state, offers)

    if (activationPresentsChoice(this.#snapshot.activation)) {
      this.#activeOfferedChoice =
        findOfferedChoice(offers, this.#snapshot.activation.choice) ?? this.#activeOfferedChoice
    }

    this.#publish(Object.freeze({
      ...this.#snapshot,
      projectionRevision: revision,
      projection: state,
      offers,
      offerPresentation: presentArtifactOffers(offers),
      activationPresentation: presentArtifactActivation(
        this.#snapshot.activation,
        this.#activeOfferedChoice,
      ),
    }))
    return true
  }

  updateActivation(activation: V2AuthorityActivationSnapshot): void {
    const previous = this.#snapshot.activation
    if (activationPresentsChoice(activation)) {
      const activationChanged = !activationPresentsChoice(previous) ||
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
    this.#publish(Object.freeze({
      ...this.#snapshot,
      activation,
      activationPresentation: presentArtifactActivation(activation, this.#activeOfferedChoice),
      chosenChoice: activationPresentsChoice(activation) ||
        (activation.kind === 'terminal' && activation.outcome.kind === 'bound-operation')
        ? activation.choice
        : null,
      chosenSizeProjection: activationPresentsChoice(activation) ||
        (activation.kind === 'terminal' && activation.outcome.kind === 'bound-operation')
        ? this.#activeOfferedChoice?.sizeProjection ?? this.#snapshot.chosenSizeProjection
        : null,
      resolvedArtifact: this.#snapshot.receiveIntent?.artifact ?? this.#resolvedAction?.artifact ?? null,
    }))
  }

  adoptReceiveIntent(
    choice: ArtifactChoice,
    intent: ReceiveIntent,
    lifecycle?: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
    repairSummary?: CompatibleNameRepairSummary,
  ): boolean {
    return this.adoptReceiveIntentAtomically(
      choice,
      intent,
      () => undefined,
      lifecycle,
      nowMilliseconds,
      workspaceUsage,
      activeControls,
      repairSummary,
    )
  }

  adoptReceiveIntentAtomically(
    choice: ArtifactChoice,
    intent: ReceiveIntent,
    commitOwnership: () => void,
    lifecycle?: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
    repairSummary?: CompatibleNameRepairSummary,
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
      plan: intent.plan,
      repairSummary: snapshotCompatibleNameRepair(repairSummary),
      activeControls: Object.freeze([...activeControls]),
    }, lifecycle ?? null, nowMilliseconds, workspaceUsage)
    this.#publishWithOwnership(snapshot, commitOwnership)
    return true
  }

  adoptRetainedReceiveIntent(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
    repairSummary?: CompatibleNameRepairSummary,
  ): void {
    this.adoptRetainedReceiveIntentAtomically(
      intent,
      lifecycle,
      () => undefined,
      nowMilliseconds,
      workspaceUsage,
      activeControls,
      repairSummary,
    )
  }

  adoptRetainedReceiveIntentAtomically(
    intent: ReceiveIntent,
    lifecycle: ReceiveLifecycleState,
    commitOwnership: () => void,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
    repairSummary?: CompatibleNameRepairSummary,
  ): void {
    if (lifecycle.operationId !== intent.operationId ||
        lifecycle.receiveIntentDigest !== intent.digest) {
      throw new TypeError('retained lifecycle does not belong to its validated receive intent')
    }
    const snapshot = this.#buildLifecycleSnapshot({
      ...EMPTY_V2_OUTPUT_PRESENTATION,
      resolvedArtifact: intent.artifact,
      receiveIntent: intent,
      plan: intent.plan,
      repairSummary: snapshotCompatibleNameRepair(repairSummary),
      activeControls: Object.freeze([...activeControls]),
    }, lifecycle, nowMilliseconds, workspaceUsage)
    this.#activeOfferedChoice = null
    this.#resolvedAction = null
    this.#publishWithOwnership(snapshot, commitOwnership)
  }

  updateLifecycle(
    state: ReceiveLifecycleState,
    nowMilliseconds = Date.now(),
    workspaceUsage?: WorkspaceUsage | null,
    activeControls: readonly V2ActiveReceiveControl[] = Object.freeze([]),
    repairSummary?: CompatibleNameRepairSummary | null,
  ): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || state.operationId !== intent.operationId ||
        state.receiveIntentDigest !== intent.digest) return false
    const current = this.#snapshot.lifecycle
    if (current !== null && state.generation <= current.generation) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      repairSummary: nextCompatibleNameRepair(
        this.#snapshot.repairSummary,
        repairSummary,
      ),
      activeControls: Object.freeze([...activeControls]),
    }, state, nowMilliseconds, workspaceUsage)
    return true
  }

  updateActiveControls(controls: readonly V2ActiveReceiveControl[]): boolean {
    const lifecycle = this.#snapshot.lifecycle
    if (lifecycle === null) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      activeControls: Object.freeze([...controls]),
    }, lifecycle, Date.now(), this.#snapshot.workspaceUsage)
    return true
  }

  updateDirectZipProgress(progress: V2DirectZipProgressSnapshot): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || intent.plan.kind !== 'direct-resumable-zip' ||
        progress.operationId !== intent.operationId) return false
    requireDirectZipProgress(progress)
    const current = this.#snapshot.directZipProgress
    if (current !== null && progress.generation <= current.generation) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      directZipProgress: Object.freeze({ ...progress }),
    }, this.#snapshot.lifecycle, Date.now(), this.#snapshot.workspaceUsage)
    return true
  }

  updateRepairSummary(
    operationId: string,
    summary: CompatibleNameRepairSummary,
    nowMilliseconds = Date.now(),
  ): boolean {
    const intent = this.#snapshot.receiveIntent
    if (intent === null || intent.operationId !== operationId) return false
    this.#publishLifecycleSnapshot({
      ...this.#snapshot,
      repairSummary: nextCompatibleNameRepair(this.#snapshot.repairSummary, summary),
    }, this.#snapshot.lifecycle, nowMilliseconds, this.#snapshot.workspaceUsage)
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
    // An absent terminal summary is authoritative only after the output layer has
    // verified and removed an empty repair pair. Carrying the earlier live notice
    // would falsely qualify an ordinary terminal result.
    const terminalRepairSummary = result.repairSummary ??
      (isTerminalLifecycleState(result.lifecycle) ? null : undefined)
    const repairSummary = nextCompatibleNameRepair(
      this.#snapshot.repairSummary,
      terminalRepairSummary,
    )
    const snapshot = this.#buildLifecycleSnapshot({
      ...this.#snapshot,
      repairSummary,
    }, result.lifecycle, Date.now(), this.#snapshot.workspaceUsage)
    this.#publish(Object.freeze({
      ...snapshot,
      transferResultPresentation: presentTransferResult(
        result.worker,
        repairSummary,
        result.lifecycle,
      ),
    }))
    return true
  }

  reset(): void {
    this.#activeOfferedChoice = null
    this.#resolvedAction = null
    this.#publish(EMPTY_V2_OUTPUT_PRESENTATION)
  }

  close(): void {
    this.reset()
    this.#listeners.clear()
  }

  #publishLifecycleSnapshot(
    base: V2OutputPresentationSnapshot,
    lifecycle: ReceiveLifecycleState | null,
    nowMilliseconds: number,
    workspaceUsage: WorkspaceUsage | null | undefined,
  ): void {
    this.#publish(this.#buildLifecycleSnapshot(base, lifecycle, nowMilliseconds, workspaceUsage))
  }

  #buildLifecycleSnapshot(
    base: V2OutputPresentationSnapshot,
    lifecycle: ReceiveLifecycleState | null,
    nowMilliseconds: number,
    workspaceUsage: WorkspaceUsage | null | undefined,
  ): V2OutputPresentationSnapshot {
    const artifact = base.resolvedArtifact
    const plan = base.plan
    if (lifecycle === null) {
      return Object.freeze({
        ...base,
        lifecycle: null,
        lifecyclePresentation: null,
        expiresAt: null,
        workspaceUsage: null,
        activeControls: Object.freeze([]),
      })
    }
    if (artifact === null || plan === null) {
      throw new TypeError('lifecycle presentation requires a bound artifact and plan')
    }
    const lifecyclePresentation = presentReceiveLifecycle({
      state: lifecycle,
      artifact,
      plan,
      nowMilliseconds,
      ...(workspaceUsage === undefined ? {} : { workspaceUsage }),
      activeControls: base.activeControls,
      repairSummary: base.repairSummary,
      directZipProgress: base.directZipProgress,
    })
    return Object.freeze({
      ...base,
      lifecycle,
      lifecyclePresentation,
      expiresAt: lifecycleDeadline(lifecycle) ??
        (lifecycle.kind === 'expired' ? lifecycle.expiresAt : null),
      workspaceUsage: lifecyclePresentation.usage === null
        ? null
        : Object.freeze({
            ownedBytes: lifecyclePresentation.usage.ownedBytes,
            ...(lifecyclePresentation.usage.maximumBytes === undefined
              ? {}
              : { maximumBytes: lifecyclePresentation.usage.maximumBytes }),
          }),
    })
  }

  #publishWithOwnership(snapshot: V2OutputPresentationSnapshot, commitOwnership: () => void): void {
    // Ownership is installed before observers can see the corresponding intent.
    commitOwnership()
    this.#publish(snapshot)
  }

  #publish(snapshot: V2OutputPresentationSnapshot): void {
    this.#snapshot = snapshot
    for (const listener of this.#listeners) listener()
  }
}

function requireDirectZipProgress(progress: V2DirectZipProgressSnapshot): void {
  if (progress.operationId.length === 0 || progress.generation < 0n ||
      progress.receivedSelectedBytes < 0n || progress.safeResumeBytes < 0n ||
      progress.safeResumeBytes > progress.receivedSelectedBytes ||
      (progress.resumeTemporarySpaceUpperBound !== undefined &&
       progress.resumeTemporarySpaceUpperBound < 0n)) {
    throw new TypeError('direct ZIP progress snapshot is invalid')
  }
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

function snapshotCompatibleNameRepair(
  summary: CompatibleNameRepairSummary | undefined,
): CompatibleNameRepairSummary | null {
  return summary === undefined ? null : compatibleNameRepairSummary(summary)
}

function nextCompatibleNameRepair(
  current: CompatibleNameRepairSummary | null,
  next: CompatibleNameRepairSummary | null | undefined,
): CompatibleNameRepairSummary | null {
  if (next === undefined) return current
  if (next === null) return null
  const snapshot = compatibleNameRepairSummary(next)
  if (current !== null) assertCompatibleNameRepairProgression(current, snapshot)
  return snapshot
}

function assertCompatibleNameRepairProgression(
  current: CompatibleNameRepairSummary,
  next: CompatibleNameRepairSummary,
): void {
  if (next.committedCount < current.committedCount) {
    throw new TypeError('compatible-name repair count cannot move backward')
  }
  if (next.placement !== current.placement ||
      next.pairDisplayNames.script !== current.pairDisplayNames.script ||
      next.pairDisplayNames.sidecar !== current.pairDisplayNames.sidecar ||
      next.runCommand !== current.runCommand) {
    throw new TypeError('compatible-name restoration identity cannot change')
  }
  for (let index = 0; index < current.logicalPathSample.length; index += 1) {
    if (!sameLogicalPath(current.logicalPathSample[index], next.logicalPathSample[index])) {
      throw new TypeError('compatible-name logical path sample cannot be rewritten')
    }
  }
  const currentFooter = current.latestObservedFooter
  const nextFooter = next.latestObservedFooter
  if (currentFooter === undefined) return
  if (nextFooter === undefined || nextFooter.committedCount < currentFooter.committedCount) {
    throw new TypeError('compatible-name sidecar checkpoint cannot move backward')
  }
  if (currentFooter.state !== 'active' &&
      (nextFooter.state !== currentFooter.state ||
       nextFooter.committedCount !== currentFooter.committedCount ||
       next.committedCount !== current.committedCount ||
       next.logicalPathSample.length !== current.logicalPathSample.length)) {
    throw new TypeError('compatible-name terminal sidecar checkpoint is immutable')
  }
  if (!current.pendingCatchUp && currentFooter.state !== 'active' && next.pendingCatchUp) {
    throw new TypeError('completed compatible-name catch-up cannot become pending')
  }
}

function sameLogicalPath(
  first: readonly string[] | undefined,
  second: readonly string[] | undefined,
): boolean {
  return first !== undefined && second !== undefined && first.length === second.length &&
    first.every((component, index) => component === second[index])
}

function resolvedActionFromActivation(
  activation: V2AuthorityActivationSnapshot,
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
