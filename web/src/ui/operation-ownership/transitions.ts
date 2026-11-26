import type { ReceiverFailure, ReceiverSnapshot } from '../model'
import type { JoinedBrowserShare } from '../../application/browser-share/gateway'
import type { ReceiveAction, RetainedTaskSnapshot } from '../../application/receive/contracts'
import type { ReceiveCoordinator, RetainedActionCallbacks } from '../../application/receive/coordinator'
import type { AsyncActionCallbacks } from '../controller/failures'
import type { BrowserNavigationCoordinator } from '../controller/navigation'
import type { PreviewController } from '../preview-controller'
import type { ReceiverExperienceObservability } from '../experience/observability'
import { findReplacementFile } from '../../application/browser-share/source-replacement'
import type { SourceRevisionFailure } from '../../transfer/state'
import { isAbortError } from '../../transfer/fault'
import { projectDraft } from '../draft/model'

interface OperationTransitionOptions {
  readonly coordinator: ReceiveCoordinator
  readonly currentJoinedShare: () => JoinedBrowserShare | undefined
  readonly isDisposed: () => boolean
  readonly snapshot: () => ReceiverSnapshot
  readonly publish: (snapshot: ReceiverSnapshot) => void
  readonly mapFailure: (error: unknown) => ReceiverFailure
  readonly onActionError: (error: unknown) => void
  readonly recordIntent: (action: string) => void
  readonly beginProjection: (joined: JoinedBrowserShare) => void
  readonly browse: Pick<BrowserNavigationCoordinator, 'loadPage'>
  readonly previews: Pick<PreviewController, 'yieldToReceiving'>
  readonly experienceTrace: Pick<ReceiverExperienceObservability, 'intent'>
}

/** Adds navigation and display effects to application commands; it owns no task admission. */
export class ReceiveOperationTransitions {
  readonly #options: OperationTransitionOptions
  #replacementAbort: AbortController | undefined

  constructor(options: OperationTransitionOptions) { this.#options = options }

  performLifecycleAction(action: ReceiveAction, callbacks?: AsyncActionCallbacks): void {
    this.#options.recordIntent('task-' + action)
    if (action === 'continue' && this.#options.coordinator.activeActionAdmission(action).allowed) {
      this.#options.previews.yieldToReceiving()
    }
    this.#options.coordinator.performActiveAction(action, this.#actionCallbacks(callbacks))
  }

  retainCurrentOperation(): Promise<boolean> {
    this.#options.recordIntent('retain-current-download')
    if (!this.#options.coordinator.canRetainCurrent) return Promise.resolve(false)
    this.#options.coordinator.startNewDownload()
    return Promise.resolve(true)
  }

  performRetainedAction(
    task: RetainedTaskSnapshot,
    action: ReceiveAction,
    callbacks?: AsyncActionCallbacks,
  ): void {
    this.#options.experienceTrace.intent('retained-' + action, this.#options.snapshot(), {
      operationId: task.taskId,
      generation: task.lifecycle.generation,
    })
    if (action === 'continue') this.#options.previews.yieldToReceiving()
    this.#options.coordinator.performRetainedAction(task, action, this.#actionCallbacks(callbacks))
  }

  #actionCallbacks(callbacks?: AsyncActionCallbacks): RetainedActionCallbacks | undefined {
    const onAsyncFailure = callbacks?.onAsyncFailure
    if (onAsyncFailure === undefined) return undefined
    return {
      onError: error => {
        if (this.#options.isDisposed()) return
        // Fall back to the global share banner when a modal closed before the error arrived.
        if (!onAsyncFailure(this.#options.mapFailure(error))) {
          this.#options.onActionError(error)
        }
      },
    }
  }

  prepareReplacementDownload(task: RetainedTaskSnapshot, failure: SourceRevisionFailure): void {
    const joined = this.#options.currentJoinedShare()
    if (joined === undefined || this.#options.coordinator.active) return
    this.#replacementAbort?.abort(new DOMException('A newer replacement lookup started', 'AbortError'))
    const abort = new AbortController()
    this.#replacementAbort = abort
    void this.#runReplacement(joined, task, failure, abort)
  }

  startNewReceiveOperation(): void {
    this.#options.recordIntent('start-another-download')
    this.#options.coordinator.startNewDownload()
  }

  cancelPendingReplacement(): void {
    this.#replacementAbort?.abort(new DOMException('Replacement lookup cancelled', 'AbortError'))
    this.#replacementAbort = undefined
  }

  async #runReplacement(
    joined: JoinedBrowserShare,
    task: RetainedTaskSnapshot,
    failure: SourceRevisionFailure,
    abort: AbortController,
  ): Promise<void> {
    const isCurrent = () =>
      !this.#options.isDisposed() &&
      this.#replacementAbort === abort &&
      !abort.signal.aborted &&
      this.#options.currentJoinedShare() === joined
    try {
      this.#options.publish({
        ...this.#options.snapshot(),
        error: null,
        status: { kind: 'finding-replacement', path: failure.path.join('/') },
      })
      const found = await findReplacementFile(joined, task, failure, abort.signal)
      if (!isCurrent()) return
      this.#options.coordinator.startNewDownload()
      joined.selectOnlyFile(found.entry, found.page.directory.ancestry)
      this.#options.publish({
        ...this.#options.snapshot(),
        draft: projectDraft('selection', found.page.directory, joined.selection, this.#options.snapshot().share),
      })
      await this.#options.browse.loadPage(found.page.directory, found.page.pageIndex, found.directories)
      if (!isCurrent()) return
      this.#options.publish({
        ...this.#options.snapshot(),
        error: null,
        status: 'replacement-selected',
      })
      this.#options.beginProjection(joined)
    } catch (error) {
      if (!isCurrent() || isAbortError(error)) return
      this.#options.onActionError(error)
    } finally {
      if (this.#replacementAbort === abort) this.#replacementAbort = undefined
    }
  }
}
