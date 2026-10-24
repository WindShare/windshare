import type { V2ReceiverSnapshot } from '../v2-model'
import type { V2JoinedBrowserShare } from '../../application/browser-share/gateway'
import type { V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../../application/receive/contracts'
import type { LifecycleUserAction } from '../../application/receive/actions'
import type { BrowserReceiveRuntime } from '../../application/receive/runtime'
import type { BrowserNavigationCoordinator } from '../controller/navigation'
import type { V2PreviewController } from '../v2-preview-controller'
import type { ReceiverExperienceObservability } from '../experience/observability'
import { findReplacementFile } from '../../application/browser-share/source-replacement'
import type { SourceRevisionFailure } from '../../transfer/lifecycle/source-revision'
import { projectDraft } from '../draft/model'

interface OperationTransitionOptions {
  readonly runtime: BrowserReceiveRuntime<V2JoinedBrowserShare>
  readonly snapshot: () => V2ReceiverSnapshot
  readonly publish: (snapshot: V2ReceiverSnapshot) => void
  readonly recordIntent: (action: string) => void
  readonly beginProjection: (joined: V2JoinedBrowserShare) => void
  readonly browse: Pick<BrowserNavigationCoordinator, 'loadPage'>
  readonly previews: Pick<V2PreviewController, 'yieldToReceiving'>
  readonly experienceTrace: Pick<ReceiverExperienceObservability, 'intent'>
}

/** Adds navigation and display effects to application commands; it owns no task admission. */
export class ReceiveOperationTransitions {
  readonly #options: OperationTransitionOptions

  constructor(options: OperationTransitionOptions) { this.#options = options }

  performLifecycleAction(action: LifecycleUserAction): void {
    this.#options.recordIntent('task-' + action)
    if (action === 'continue' && this.#options.runtime.activeLifecycleActionAdmission(action).allowed) {
      this.#options.previews.yieldToReceiving()
    }
    this.#options.runtime.performLifecycleAction(action)
  }

  retainCurrentOperation(): Promise<boolean> {
    this.#options.recordIntent('retain-current-download')
    return this.#options.runtime.retainCurrentOperation()
  }

  performRetainedAction(operation: V2RetainedReceiveOperation, action: V2RetainedReceiveAction): void {
    this.#options.experienceTrace.intent('retained-' + action, this.#options.snapshot(),
      { operationId: operation.operationId, generation: operation.lifecycleGeneration })
    if (action === 'continue') this.#options.previews.yieldToReceiving()
    this.#options.runtime.performRetainedAction(operation, action)
  }

  prepareReplacementDownload(operation: V2RetainedReceiveOperation, failure: SourceRevisionFailure): void {
    this.#options.runtime.prepareReplacement(operation, async (joined, signal, current) => {
      this.#options.publish({ ...this.#options.snapshot(), error: null,
        status: 'Finding the current version of ' + failure.path.join('/') + '…' })
      const found = await findReplacementFile(joined, operation, failure, signal)
      if (!current()) return
      await this.#options.runtime.resetOwnership(new DOMException('Preparing a separate replacement download', 'AbortError'))
      if (!current()) return
      joined.selectOnlyFile(found.entry, found.page.directory.ancestry)
      this.#options.publish({ ...this.#options.snapshot(),
        draft: projectDraft('selection', found.page.directory, joined.selection, this.#options.snapshot().share) })
      await this.#options.browse.loadPage(found.page.directory, found.page.pageIndex, found.directories)
      if (!current()) return
      this.#options.publish({ ...this.#options.snapshot(), error: null,
        status: 'Current version selected. Choose Download to create a separate task; the original ZIP progress is retained.' })
      this.#options.beginProjection(joined)
    })
  }

  startNewReceiveOperation(): void {
    this.#options.recordIntent('start-another-download')
    this.#options.runtime.startNewReceiveOperation()
  }
}
