import type { ReceiveOperationDisplay } from '../../output/workspace/operation-display'
import type { BrowserRecoveryPreference } from '../../output/browser-delivery/model'
import type { ArtifactChoiceID, SelectionSpec } from '../../transfer/intent'
import type { TransferProgress } from '../../transfer/job'
import { AuthorityActivationCoordinator } from './activation/coordinator'
import { ActiveReceiveCoordinator } from './active/coordinator'
import type { ReceiveCompositionPort } from './contracts'
import { SelectionProjectionRuntime, type ProjectionReplacementKind } from './discovery'
import type { ReceivePlanningShare } from './joined-share'
import type { ReceiveObservationOptions } from './observation/contracts'
import { ControllerObservability } from './observation/coordinator'
import { RetainedInventoryCoordinator } from './retained/coordinator'
import type { RetainedReceiveInventorySnapshot } from './retained/model'
import { BrowserReceiveRuntime } from './runtime'
import { ReceiveRuntimeState } from './state'

export interface BrowserReceiveApplicationOptions<Share extends ReceivePlanningShare> extends ReceiveObservationOptions {
  readonly receive: ReceiveCompositionPort
  readonly currentJoinedShare: () => Share | undefined
  readonly isDisposed: () => boolean
  readonly onProgress: (progress: TransferProgress) => void
  readonly onRetainedChanged: (retained: RetainedReceiveInventorySnapshot) => void
  readonly onTaskAdopted: (display: ReceiveOperationDisplay | undefined) => void
  readonly onTaskCleared: () => void
  readonly onChanged: () => void
  readonly onBrowsingReady: (joined: Share) => void
  readonly onActionError: (error: unknown) => void
}

/**
 * Owns the receive coordinators as one lifetime. Presentation observes facts and
 * submits commands; replacing a screen cannot create a second output owner.
 */
export class BrowserReceiveApplication<Share extends ReceivePlanningShare> {
  readonly tasks: BrowserReceiveRuntime<Share>
  readonly diagnostics: ControllerObservability
  readonly #state = new ReceiveRuntimeState()
  readonly observations: Pick<ReceiveRuntimeState, 'getSnapshot' | 'subscribe'> = this.#state
  readonly #active: ActiveReceiveCoordinator
  readonly #activation: AuthorityActivationCoordinator
  readonly #retained: RetainedInventoryCoordinator
  readonly #projection: SelectionProjectionRuntime
  readonly #unsubscribeActivation: () => void
  #closing: Promise<void> | undefined

  constructor(options: BrowserReceiveApplicationOptions<Share>) {
    const disposed = () => this.#closing !== undefined || options.isDisposed()
    this.diagnostics = new ControllerObservability(options)
    this.#active = new ActiveReceiveCoordinator({
      state: this.#state,
      ownsJoinedShare: joined => !disposed() && options.currentJoinedShare() === joined,
      onProgress: options.onProgress,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
      onActionError: options.onActionError,
      onFailure: options.onActionError,
      onOwnershipReleased: () => this.tasks.ownershipReleased(),
      onRetainedFileFailure: () => this.tasks.releaseFailedReceive(),
    })
    this.#activation = new AuthorityActivationCoordinator({
      receive: options.receive,
      activeReceive: this.#active,
      observability: this.diagnostics,
      currentProjection: () => this.#projection.current,
      currentJoinedShare: options.currentJoinedShare,
      choiceBlocked: () => this.tasks.startBlockedReason() !== null,
      retryProjection: projection => { this.#projection.retry(projection).catch(() => undefined) },
      publishProjection: ({ observationRevision, state, offers }) => {
        this.#state.updateProjection(observationRevision, state, offers)
      },
      adoptReceiveIntent: (choice, intent, operation, commitOwnership) =>
        this.#state.adoptReceiveIntentAtomically(
          choice, intent,
          () => {
            commitOwnership()
            options.onTaskAdopted(operation.display)
          },
          operation.lifecycle, operation.initialWorkspaceUsage, operation.activeControls,
        ),
      refreshRetainedInventory: () => { this.#retained.load().catch(() => undefined) },
      publishActionError: options.onActionError,
    })
    this.#projection = new SelectionProjectionRuntime({
      receive: options.receive,
      authority: this.#activation,
      observability: this.diagnostics,
      currentJoinedShare: options.currentJoinedShare,
      isDisposed: disposed,
      onFailure: options.onActionError,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    const readRepairSummary = options.receive.retained.readRepairSummary
    this.#retained = new RetainedInventoryCoordinator({
      receive: options.receive,
      isDisposed: disposed,
      currentJoinedShare: options.currentJoinedShare,
      continuationBlocked: () => this.tasks.continuationBlocked,
      remoteContinuationUnavailable: () => this.tasks.remoteContinuationUnavailable(),
      localFinalizationBlocked: () => this.tasks.continuationBlocked,
      adoptContinuation: input => this.tasks.adoptContinuation(input),
      ownsRuntime: operation => this.#active.ownsRuntime(operation),
      publish: options.onRetainedChanged,
      onActionCompleted: (operationId, action) => this.tasks.retainedActionCompleted(operationId, action),
      ...(readRepairSummary === undefined ? {} : {
        repairSource: { readRepairSummary: (operationId: string, signal: AbortSignal) =>
          readRepairSummary.call(options.receive.retained, operationId, signal) },
      }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
      onActionError: options.onActionError,
      ...(options.incidents === undefined ? {} : { incidents: options.incidents }),
    })
    this.tasks = new BrowserReceiveRuntime({
      state: this.#state,
      active: this.#active,
      retained: this.#retained,
      activation: this.#activation,
      joined: options.currentJoinedShare,
      disposed,
      selectionIsEmpty: () => options.currentJoinedShare()?.selection.intentSummary().empty ?? true,
      onChanged: options.onChanged,
      projection: this.#projection,
      onTaskAdopted: options.onTaskAdopted,
      onTaskCleared: options.onTaskCleared,
      onBrowsingReady: options.onBrowsingReady,
      onActionError: options.onActionError,
    })
    this.#unsubscribeActivation = this.#activation.subscribe(() => {
      this.#state.updateActivation(this.#activation.getSnapshot())
    })
  }

  get active(): boolean { return this.#active.active }
  get operationId(): string | null { return this.#active.operationId }
  get canPreviewAutomatically(): boolean {
    return !this.#active.active && !this.#activation.pending && !this.#retained.pending
  }

  loadRetained(): Promise<void> {
    return this.#closing === undefined ? this.#retained.load() : Promise.resolve()
  }

  chooseArtifact(
    choiceId: ArtifactChoiceID,
    display: ReceiveOperationDisplay,
    recoveryPreference: BrowserRecoveryPreference,
  ): void {
    if (this.tasks.startBlockedReason() !== null) return
    // A picker must run in this call stack, before any storage or network await.
    this.#activation.choose(choiceId, display, recoveryPreference)
  }

  retryOutputConfirmation(): void {
    if (this.#closing === undefined) this.#activation.retry()
  }

  cancelPreparing(reason: unknown): void {
    this.#activation.invalidate(reason, 'caller-cancelled')
    this.#state.resetDraft()
  }

  beginProjection(joined: Share, replacement: ProjectionReplacementKind): void {
    if (this.#closing !== undefined) return
    if (replacement === 'selection-change') this.#state.resetDraft()
    this.#projection.start(joined, replacement)
  }

  suspendForJoin(reason: unknown): Promise<void> {
    this.#retained.cancelPending(reason)
    const detached = this.#active.reset(reason)
    if (this.#state.getSnapshot().receiveIntent !== null) this.#state.reset()
    this.#activation.suspendForJoin()
    this.#projection.stop(reason)
    return detached
  }

  completeJoin(joined: Share, selection: SelectionSpec): void {
    this.#activation.completeJoin(joined, selection)
  }

  cancelJoin(): void { this.#activation.cancelJoin() }

  close(reason: unknown): Promise<void> {
    if (this.#closing !== undefined) return this.#closing
    let settled!: () => void
    let failed!: (error: unknown) => void
    // Fence reentrant observers before any coordinator can publish its final state.
    this.#closing = new Promise<void>((resolve, reject) => { settled = resolve; failed = reject })
    try {
      this.#retained.close(reason)
      const detached = this.tasks.resetOwnership(reason)
      this.#unsubscribeActivation()
      this.#activation.close(reason)
      this.#state.close()
      detached.then(settled, failed)
    } catch (error) { failed(error) }
    return this.#closing
  }
}
