import { EMPTY_V2_PROGRESS, type V2ReceiverSnapshot } from '../v2-model'
import type { V2JoinedBrowserShare } from '../v2-gateway'
import type { V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../v2-receive-runtime'
import { presentNewReceiveOperation, type LifecycleUserAction } from '../v2-lifecycle-presentation'
import type { ActiveReceiveCoordinator } from '../controller/active-receive'
import type { V2AuthorityActivationCoordinator } from '../controller/authority-activation'
import type { RetainedInventoryCoordinator } from '../controller/retained-inventory'
import type { BrowserNavigationCoordinator } from '../controller/navigation'
import type { V2OutputPresentationController } from '../v2-output'
import type { V2PreviewController } from '../v2-preview-controller'
import type { ReceiverExperienceObservability } from '../experience/observability'
import { findReplacementFile } from '../source-replacement/selection'
import type { SourceRevisionFailure } from '../../output/resume/source-revision-failures'
import { projectDraft } from '../draft/model'
import { isReceiveOutputDelivered } from './completion'

interface OperationTransitionOptions {
  readonly snapshot: () => V2ReceiverSnapshot
  readonly joined: () => V2JoinedBrowserShare | undefined
  readonly disposed: () => boolean
  readonly publish: (snapshot: V2ReceiverSnapshot) => void
  readonly actionError: (error: unknown) => void
  readonly recordIntent: (action: string) => void
  readonly resetOwnership: (reason: unknown) => Promise<void>
  readonly beginProjection: (joined: V2JoinedBrowserShare, reason?: 'selection-change' | 'observation-replacement') => void
  readonly activeReceive: Pick<ActiveReceiveCoordinator, 'active' | 'canRelease' | 'canRetainForLocalOutput' | 'reset' | 'performLifecycleAction'>
  readonly authority: Pick<V2AuthorityActivationCoordinator, 'pending'>
  readonly retained: Pick<RetainedInventoryCoordinator, 'pending' | 'load' | 'actionAdmission' | 'perform'>
  readonly outputs: Pick<V2OutputPresentationController, 'clearTask'>
  readonly browse: Pick<BrowserNavigationCoordinator, 'loadPage'>
  readonly previews: Pick<V2PreviewController, 'yieldToReceiving'>
  readonly experienceTrace: Pick<ReceiverExperienceObservability, 'intent'>
}

/** Owns destination handoff admission through detach, inventory reload and next-task intent. */
export class ReceiveOperationTransitions {
  readonly #options: OperationTransitionOptions
  #pending = false

  constructor(options: OperationTransitionOptions) { this.#options = options }

  get pending(): boolean { return this.#pending }

  activeLifecycleActionAdmission(action: LifecycleUserAction): Readonly<{ allowed: boolean; reason: string | null }> {
    if (!this.#options.activeReceive.active || this.#pending ||
        isReceiveOutputDelivered(this.#options.snapshot().output.lifecycle)) {
      return { allowed: false, reason: 'This download is managed in Downloads.' }
    }
    if (action === 'continue' && this.#options.snapshot().output.lifecycle?.kind === 'resumable-receive') {
      const reason = this.remoteContinuationUnavailable()
      if (reason !== null) return { allowed: false, reason }
    }
    return { allowed: true, reason: null }
  }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#options.recordIntent(`task-${action}`)
    if (!this.activeLifecycleActionAdmission(action).allowed) return
    if (action === 'continue') this.#options.previews.yieldToReceiving()
    this.#options.activeReceive.performLifecycleAction(action)
  }

  get canRetainCurrentOperation(): boolean {
    return !this.#pending && !this.#options.retained.pending && !this.#options.authority.pending &&
      this.#options.activeReceive.canRetainForLocalOutput
  }

  retainCurrentOperation(): Promise<boolean> {
    if (!this.canRetainCurrentOperation) return Promise.resolve(false)
    this.#options.recordIntent('retain-current-download')
    this.#pending = true
    const operationId = this.#options.snapshot().output.receiveIntent?.operationId
    this.#options.publish(this.#options.snapshot())
    return this.#options.activeReceive.reset(new DOMException('Keep paused progress in Downloads', 'AbortError'))
      .then(async () => {
        await this.#options.retained.load()
        if (this.#options.disposed() || this.#options.snapshot().output.receiveIntent?.operationId !== operationId) return false
        if (!this.#options.snapshot().retained.operations.some(operation => operation.operationId === operationId)) {
          throw new Error('Paused progress was retained, but Downloads could not reload its operation.')
        }
        this.#options.outputs.clearTask()
        this.#options.publish({ ...this.#options.snapshot(), taskDisplay: null, progress: EMPTY_V2_PROGRESS })
        return true
      })
      .catch(error => { this.#options.actionError(error); return false })
      .finally(() => {
        this.#pending = false
        if (!this.#options.disposed()) this.#options.publish(this.#options.snapshot())
      })
  }

  retainedActionAdmission(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction) {
    return this.#options.retained.actionAdmission(operation, action)
  }

  performRetainedAction(
    operation: V2RetainedReceiveOperation,
    action: V2RetainedReceiveAction,
  ): void {
    this.#options.experienceTrace.intent(`retained-${action}`, this.#options.snapshot(),
      { operationId: operation.operationId, generation: operation.lifecycleGeneration })
    if (action === 'continue') this.#options.previews.yieldToReceiving()
    this.#options.retained.perform(operation, action)
  }

  prepareReplacementDownload(operation: V2RetainedReceiveOperation, failure: SourceRevisionFailure): void {
    const joined = this.#options.joined()
    if (this.#options.disposed() || this.#pending || this.#options.retained.pending ||
        this.#options.activeReceive.active || this.#options.authority.pending ||
        !this.#options.snapshot().retained.operations.includes(operation)) return
    if (joined === undefined) {
      this.#options.actionError(new DOMException(
        'Open the matching share before downloading the current version', 'InvalidStateError'))
      return
    }
    this.#pending = true
    const controller = new AbortController()
    const protocolSessionId = joined.protocolSessionId
    this.#options.publish({ ...this.#options.snapshot(), error: null, status: `Finding the current version of ${failure.path.join('/')}…` })
    const current = () => !this.#options.disposed() && this.#options.joined() === joined && joined.protocolSessionId === protocolSessionId &&
      this.#options.snapshot().retained.operations.includes(operation) && !this.#options.activeReceive.active && !this.#options.retained.pending
    findReplacementFile(joined, operation, failure, controller.signal).then(async found => {
      if (!current()) return
      await this.#options.resetOwnership(new DOMException('Preparing a separate replacement download', 'AbortError'))
      if (!current()) return
      joined.selectOnlyFile(found.entry, found.page.directory.ancestry)
      this.#options.publish({ ...this.#options.snapshot(), draft: projectDraft('selection', found.page, joined.selection, this.#options.snapshot().share) })
      await this.#options.browse.loadPage(found.page.directory, found.page.pageIndex, found.directories)
      if (!current()) return
      this.#options.publish({ ...this.#options.snapshot(), error: null,
        status: 'Current version selected. Choose Download to create a separate task; the original ZIP progress is retained.' })
      this.#options.beginProjection(joined)
    }).catch(error => {
      if (current()) this.#options.actionError(error)
    }).finally(() => {
      this.#pending = false
      if (!this.#options.disposed()) this.#options.publish(this.#options.snapshot())
    })
  }

  catchUpStoppedCompatibleNames(): void {
    const output = this.#options.snapshot().output
    const repair = output.lifecyclePresentation?.compatibleNameRepair
    if (this.#options.disposed() || this.#options.retained.pending || output.lifecycle === null ||
        !this.#options.activeReceive.active || repair?.actionMode !== 'catch-up-required' ||
        repair.visibility === 'notice') return
    const operationId = output.lifecycle.operationId
    // Local replay reacquires exclusive output authority. Release the stopped
    // receiver first, then use the same durable action path as a fresh page.
    this.#options.resetOwnership(new DOMException(
      'Stopped receive is handing output authority to local restoration catch-up',
      'AbortError',
    )).then(async () => {
      if (this.#options.disposed()) return
      await this.#options.retained.load()
      if (this.#options.disposed()) return
      const operation = this.#options.snapshot().retained.operations.find(candidate =>
        candidate.operationId === operationId)
      if (operation?.actions.includes('catch-up')) this.#options.retained.perform(operation, 'catch-up')
    }).catch(error => this.#options.actionError(error))
  }

  startNewReceiveOperation(): void {
    this.#options.recordIntent('start-another-download')
    const joined = this.#options.joined()
    const output = this.#options.snapshot().output
    const presentation = presentNewReceiveOperation({
      lifecycle: output.lifecycle,
      plan: output.plan,
    })
    if (this.#options.disposed() || this.#pending || joined === undefined ||
        this.#options.retained.pending || this.#options.authority.pending ||
        (presentation === null && !this.#options.activeReceive.canRelease)) return
    this.#pending = true
    const boundary = new DOMException(
      presentation?.kind === 'direct-tree-to-zip'
        ? 'The completed DirectTree receive is being replaced by a new ZIP operation'
        : 'The settled task is being released for a new receive operation',
      'AbortError',
    )
    this.#options.resetOwnership(boundary).then(() => {
      if (!this.#options.disposed() && this.#options.joined() === joined) {
        this.#options.publish({ ...this.#options.snapshot(), progress: EMPTY_V2_PROGRESS, error: null })
        this.#options.beginProjection(joined, 'observation-replacement')
      }
    }, error => this.#options.actionError(error)).finally(() => {
      this.#pending = false
      if (!this.#options.disposed()) {
        this.#options.publish(this.#options.snapshot())
        this.#options.retained.load().catch(() => undefined)
      }
    })
  }

  remoteContinuationUnavailable(): string | null {
    const connection = this.#options.snapshot().connection
    if (connection.kind === 'ended') return 'This share has ended. Open a new share link to receive more content.'
    if (connection.kind === 'unavailable') return 'Reopen the share link to reconnect before continuing this download.'
    return null
  }

  startBlockedReason(snapshot = this.#options.snapshot()): string | null {
    if (this.#options.disposed() || this.#options.joined() === undefined) return 'Connect to the share first.'
    if (this.#pending || this.#options.retained.pending) return 'Another download is using the saving destination.'
    if (this.#options.activeReceive.active) {
      if (isReceiveOutputDelivered(snapshot.output.lifecycle)) return 'Finishing the current download.'
      if (this.#options.activeReceive.canRelease) return 'Choose Start another download to try again.'
      const kind = snapshot.output.lifecycle?.kind
      if (kind === 'resumable-receive') return 'The paused download still owns its destination. Continue or stop it first.'
      if (kind === 'waiting-to-save') return 'Save or discard the prepared result before starting another download.'
      return 'A download is in progress.'
    }
    if (this.#options.authority.pending) return 'Finish or cancel the current saving choice.'
    if (snapshot.connection.kind === 'ended') return 'This share has ended. Open a new share link to download more.'
    if (snapshot.connection.kind === 'unavailable') return 'Reconnect using the share link before starting another download.'
    if (snapshot.draft.empty) return 'Select items to download.'
    return null
  }

}
