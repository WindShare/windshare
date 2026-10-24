import type { SelectionProjectionRuntime } from './discovery'
import { validateReceiveIntent } from '../../transfer/intent'
import type { ReceiveOperationDisplay } from '../../output/workspace/operation-display'
import { StaleReceiveBoundaryError } from './observation/contracts'
import { selectionPolicyFromIntent } from './selection'
import type { RetainedContinuationAdoption } from './retained/coordinator'
import type { ReceiverConnectionSnapshot } from '../../receiver/connection-state'
import type { ReceiveRuntimeState } from './state'
import type { ActiveReceiveCoordinator } from './active/coordinator'
import type { RetainedInventoryCoordinator } from './retained/coordinator'
import type { LifecycleUserAction } from './actions'
import type { RetainedReceiveAction, RetainedReceiveOperation } from './contracts'
import type { ReceiveJoinedShare } from './joined-share'
import { isReceiveOutputDelivered } from './completion'
import { newReceiveOperationKind } from './policy'
import { canCatchUpReceiveRepair } from './repair'

interface ReceiveRuntimeOptions<Share extends ReceiveJoinedShare> {
  readonly state: ReceiveRuntimeState
  readonly active: ActiveReceiveCoordinator
  readonly retained: RetainedInventoryCoordinator
  readonly activation: {
    readonly pending: boolean
    invalidate(reason: unknown, invalidation: 'caller-cancelled'): void
  }
  readonly joined: () => Share | undefined
  readonly disposed: () => boolean
  readonly selectionIsEmpty: () => boolean
  readonly onChanged: () => void
  readonly projection: Pick<SelectionProjectionRuntime, 'stop'>
  readonly onTaskCleared: () => void
  readonly onTaskAdopted: (display: ReceiveOperationDisplay | undefined) => void
  readonly onBrowsingReady: (joined: Share) => void
  readonly onActionError: (error: unknown) => void
}

/**
 * Owns application admission and handoffs. Durable output facts and running
 * execution lifetimes remain separate: an observed checkpoint never releases a writer.
 */
export class BrowserReceiveRuntime<Share extends ReceiveJoinedShare> {
  readonly #options: ReceiveRuntimeOptions<Share>
  #pending = false
  #connection: ReceiverConnectionSnapshot['kind'] | 'idle' = 'idle'

  constructor(options: ReceiveRuntimeOptions<Share>) { this.#options = options }

  get pending(): boolean { return this.#pending }

  get continuationBlocked(): boolean {
    return this.#options.active.active || this.#options.activation.pending || this.#pending
  }

  observeConnection(connection: ReceiverConnectionSnapshot): void { this.#connection = connection.kind }

  get canReleaseCurrent(): boolean {
    return !this.#pending && !this.#options.retained.pending && !this.#options.activation.pending &&
      this.#options.active.canRelease
  }

  get canRetainCurrentOperation(): boolean {
    return !this.#pending && !this.#options.retained.pending && !this.#options.activation.pending &&
      this.#options.active.canRetainForLocalOutput
  }

  activeLifecycleActionAdmission(action: LifecycleUserAction): Readonly<{ allowed: boolean; reason: string | null }> {
    if (!this.#options.active.active || this.#pending ||
        isReceiveOutputDelivered(this.#options.state.getSnapshot().lifecycle)) {
      return { allowed: false, reason: 'This download is managed in Downloads.' }
    }
    if (action === 'continue' && this.#options.state.getSnapshot().lifecycle?.kind === 'resumable-receive') {
      const reason = this.remoteContinuationUnavailable()
      if (reason !== null) return { allowed: false, reason }
    }
    return this.#options.active.canPerformLifecycleAction(action)
      ? { allowed: true, reason: null }
      : { allowed: false, reason: 'This download action is no longer available.' }
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    if (this.#options.disposed() || !this.activeLifecycleActionAdmission(action).allowed) return
    this.#options.active.performLifecycleAction(action)
  }

  retainedActionAdmission(operation: RetainedReceiveOperation, action: RetainedReceiveAction) {
    return this.#options.retained.actionAdmission(operation, action)
  }

  performRetainedAction(operation: RetainedReceiveOperation, action: RetainedReceiveAction): void {
    this.#options.retained.perform(operation, action)
  }

  retainedActionCompleted(operationId: string, action: RetainedReceiveAction): void {
    if (this.#options.disposed() || this.#options.active.active ||
        this.#options.state.getSnapshot().lifecycle?.operationId !== operationId ||
        (action !== 'forget' && action !== 'delete' && action !== 'discard')) return
    this.#options.state.clearTask()
    this.#notify(this.#options.onTaskCleared)
  }

  async adoptContinuation(input: RetainedContinuationAdoption): Promise<void> {
    const joined = this.#options.joined()
    if (this.#options.disposed() || joined === undefined || joined !== input.joined) throw new StaleReceiveBoundaryError()
    const { runtime } = input
    const intent = await validateReceiveIntent(runtime.intent)
    if (!this.#ownsJoined(joined)) throw new StaleReceiveBoundaryError()
    const selection = selectionPolicyFromIntent(intent)
    this.#options.activation.invalidate(new StaleReceiveBoundaryError(), 'caller-cancelled')
    const prepared = this.#options.active.prepareAdoption({
      joined, selection, runtime,
      ...(runtime.repairProjection === undefined ? {} : { repairProjection: runtime.repairProjection }),
    })
    this.#options.state.adoptRetainedReceiveIntentAtomically(
      intent, runtime.lifecycle,
      () => {
        prepared.commit()
        this.#notify(() => this.#options.onTaskAdopted(runtime.display ?? input.retained.display))
      },
      runtime.initialWorkspaceUsage, runtime.activeControls, input.retained.repairSummary,
    )
    prepared.start()
  }

  retainCurrentOperation(): Promise<boolean> {
    if (this.#options.disposed() || !this.canRetainCurrentOperation) return Promise.resolve(false)
    this.#pending = true
    const operationId = this.#options.state.getSnapshot().receiveIntent?.operationId
    this.#notify(this.#options.onChanged)
    return this.#options.active.reset(new DOMException('Keep paused progress in Downloads', 'AbortError'))
      .then(async () => {
        await this.#options.retained.load()
        if (this.#options.disposed() || this.#options.state.getSnapshot().receiveIntent?.operationId !== operationId) return false
        if (!this.#options.retained.getSnapshot().operations.some(operation => operation.operationId === operationId)) {
          throw new Error('Paused progress was retained, but Downloads could not reload its operation.')
        }
        this.#options.state.clearTask()
        this.#notify(this.#options.onTaskCleared)
        return true
      })
      .catch(error => { this.#options.onActionError(error); return false })
      .finally(() => { this.#pending = false; this.#notify(this.#options.onChanged) })
  }

  ownershipReleased(): void {
    if (!this.#resumeBrowsing(this.#options.joined())) return
    this.#options.retained.load().catch(() => undefined)
  }

  releaseFailedReceive(): void {
    if (this.#options.disposed() || this.#pending || !this.#options.active.active) return
    this.#releaseToBrowsing(new DOMException('Retained file failures require a recovery choice', 'AbortError'))
      .catch(() => undefined)
  }

  catchUpStoppedCompatibleNames(): void {
    const facts = this.#options.state.getSnapshot()
    if (this.#options.disposed() || this.#pending || this.#options.retained.pending ||
        facts.lifecycle === null || !this.#options.active.active ||
        !canCatchUpReceiveRepair(facts.lifecycle, facts.repairSummary)) return
    const operationId = facts.lifecycle.operationId
    this.#releaseToBrowsing(new DOMException(
      'Stopped receive is handing output authority to local restoration catch-up', 'AbortError',
    )).then(released => {
      if (!released) return
      const operation = this.#options.retained.getSnapshot().operations.find(candidate => candidate.operationId === operationId)
      if (operation?.actions.includes('catch-up')) this.#options.retained.perform(operation, 'catch-up')
    }).catch(error => this.#options.onActionError(error))
  }

  startNewReceiveOperation(): void {
    const facts = this.#options.state.getSnapshot()
    const kind = newReceiveOperationKind(facts.lifecycle, facts.plan)
    if (this.#options.disposed() || this.#pending || this.#options.joined() === undefined ||
        this.#options.retained.pending || this.#options.activation.pending ||
        (kind === null && !this.#options.active.canRelease)) return
    this.#releaseToBrowsing(new DOMException(
      kind === 'direct-tree-to-zip'
        ? 'The completed DirectTree receive is being replaced by a new ZIP operation'
        : 'The settled task is being released for a new receive operation',
      'AbortError',
    )).catch(() => undefined)
  }

  prepareReplacement(
    operation: RetainedReceiveOperation,
    prepare: (joined: Share, signal: AbortSignal, current: () => boolean) => Promise<void>,
  ): void {
    if (this.#options.disposed() || this.#pending || this.#options.retained.pending ||
        this.#options.active.active || this.#options.activation.pending ||
        !this.#options.retained.getSnapshot().operations.includes(operation)) return
    const joined = this.#options.joined()
    if (joined === undefined) {
      this.#options.onActionError(new DOMException(
        'Open the matching share before downloading the current version', 'InvalidStateError'))
      return
    }
    this.#pending = true
    this.#notify(this.#options.onChanged)
    const protocolSessionId = joined.protocolSessionId
    const current = () => this.#ownsJoined(joined) && joined.protocolSessionId === protocolSessionId &&
      this.#options.retained.getSnapshot().operations.includes(operation) &&
      !this.#options.active.active && !this.#options.retained.pending
    prepare(joined, new AbortController().signal, current).catch(error => {
      if (current()) this.#options.onActionError(error)
    }).finally(() => { this.#pending = false; this.#notify(this.#options.onChanged) })
  }

  resetOwnership(reason: unknown): Promise<void> {
    this.#options.projection.stop(reason)
    this.#options.activation.invalidate(reason, 'caller-cancelled')
    const detached = this.#options.active.reset(reason)
    this.#options.state.reset()
    this.#notify(this.#options.onTaskCleared)
    return detached
  }

  shareNavigationBlockedReason(): string | null {
    if (this.#options.retained.pending || this.#pending) {
      return 'Wait for the current download action to finish before opening another share.'
    }
    if (this.#options.active.active && !isReceiveOutputDelivered(this.#options.state.getSnapshot().lifecycle)) {
      return 'Finish this download, or pause it and keep it in Downloads, before opening another share.'
    }
    return null
  }

  remoteContinuationUnavailable(): string | null {
    if (this.#connection === 'ended') return 'This share has ended. Open a new share link to receive more content.'
    if (this.#connection === 'unavailable') return 'Reopen the share link to reconnect before continuing this download.'
    return null
  }

  startBlockedReason(): string | null {
    if (this.#options.disposed() || this.#options.joined() === undefined) return 'Connect to the share first.'
    if (this.#pending || this.#options.retained.pending) return 'Another download is using the saving destination.'
    if (this.#options.active.active) {
      const state = this.#options.state.getSnapshot().lifecycle
      if (isReceiveOutputDelivered(state)) return 'Finishing the current download.'
      if (this.#options.active.canRelease) return 'Choose Start another download to try again.'
      if (state?.kind === 'resumable-receive') return 'The paused download still owns its destination. Continue or stop it first.'
      if (state?.kind === 'waiting-to-save') return 'Save or discard the prepared result before starting another download.'
      return 'A download is in progress.'
    }
    if (this.#options.activation.pending) return 'Finish or cancel the current saving choice.'
    if (this.#connection === 'ended') return 'This share has ended. Open a new share link to download more.'
    if (this.#connection === 'unavailable') return 'Reconnect using the share link before starting another download.'
    if (this.#options.selectionIsEmpty()) return 'Select items to download.'
    return null
  }

  async #releaseToBrowsing(reason: unknown): Promise<boolean> {
    const joined = this.#options.joined()
    this.#pending = true
    this.#notify(this.#options.onChanged)
    try {
      await this.resetOwnership(reason)
      if (!this.#resumeBrowsing(joined)) return false
    } catch (error) {
      if (this.#ownsJoined(joined)) this.#options.onActionError(error)
      return false
    } finally {
      this.#pending = false
      this.#notify(this.#options.onChanged)
    }
    if (!this.#ownsJoined(joined)) return false
    // Loading inventory may start local finalization under a new output lease.
    try {
      await this.#options.retained.load()
      return this.#ownsJoined(joined)
    } catch (error) {
      if (this.#ownsJoined(joined)) this.#options.onActionError(error)
      return false
    }
  }

  #resumeBrowsing(joined: Share | undefined): boolean {
    if (!this.#ownsJoined(joined)) return false
    this.#options.state.resetDraft()
    this.#notify(() => this.#options.onBrowsingReady(joined))
    return true
  }

  #ownsJoined(joined: Share | undefined): joined is Share {
    return joined !== undefined && !this.#options.disposed() && this.#options.joined() === joined
  }

  #notify(observer: () => void): void {
    try { observer() } catch {
      // A screen update cannot change resource ownership or command admission.
    }
  }
}
