import { taskActions } from './experience/task-actions'
import { downloadScopeLabel, shareConnectionLabel } from './experience/share-presentation'
import { useState, useSyncExternalStore, type FormEvent } from 'react'
import type { V2ReceiverController } from './v2-controller'
import { presentNewReceiveOperation } from './v2-lifecycle-presentation'
import { presentSavingActions } from './saving'
import { SavingControls } from './saving/SavingControls'
import { ShareContent } from './share-content/ShareContent'
import { MediaPreview, type PreviewActions } from './share-content/MediaPreview'
import { DetailSheet } from './controls/DetailSheet'
import { TaskCard, TaskDetails } from './tasks/TaskView'
import { composeTasks } from './experience/task-composition'
import { TaskDownloads, TaskSourceDetails } from './experience/TaskDownloads'
import { ConnectionDetails } from './experience/ConnectionDetails'
import { ReceiverIcon } from './receiver-presentation/ReceiverIcon'
import { ReceiverFold } from './receiver-presentation/ReceiverFold'

function KeyForm({ controller }: { readonly controller: V2ReceiverController }) {
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = event.currentTarget.elements.namedItem('capability-key')
    if (!(input instanceof HTMLInputElement)) return
    const key = input.value
    input.value = ''
    controller.submitKey(key)
  }
  return <form className="key-form" onSubmit={submit}>
    <label htmlFor="capability-key">Separate key</label>
    <p id="key-help">Paste the key or the complete WindShare link to open this share.</p>
    <div className="key-entry"><input id="capability-key" name="capability-key" type="password"
      autoComplete="off" spellCheck={false} aria-describedby="key-help" required autoFocus />
      <button className="primary-action" type="submit">Open share</button></div>
  </form>
}

export function V2ReceiverApp({ controller }: { readonly controller: V2ReceiverController }) {
  const snapshot = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot)
  const [details, setDetails] = useState<'connection' | 'task' | null>(null)
  const [downloadsOpen, setDownloadsOpen] = useState(false)
  const { current, tasks } = composeTasks(snapshot, (operation, action) => controller.retainedActionAdmission(operation, action),
    action => controller.activeLifecycleActionAdmission(action))
  const actions = taskActions(controller, snapshot)
  const openDetails = (next: 'connection' | 'task' | null) => {
    controller.recordExperienceIntent(next === null ? 'close-details' : 'open-' + next + '-details')
    setDetails(next)
  }
  const share = snapshot.share
  const single = share !== null && share.kind !== 'browser' ? share : null
  const actionLabel = downloadScopeLabel(share, snapshot.draft)
  const saving = presentSavingActions({
    offers: snapshot.output.offers,
    actionLabel,
    disabledReason: snapshot.draft.empty ? 'Select items to download' : snapshot.startAdmission.reason,
  })
  const previewActions: PreviewActions = {
    close: () => controller.cancelPreview(),
    seek: seconds => controller.seekPreview(seconds),
    presented: id => controller.previewMediaPresented(id),
    failed: id => controller.previewMediaFailed(id),
  }
  const newOperation = presentNewReceiveOperation({ plan: snapshot.output.plan, lifecycle: snapshot.output.lifecycle })
  const matching = current === null && share !== null ? tasks.find(task => snapshot.retained.operations.some(operation =>
    operation.operationId === task.operationId && operation.shareInstance === share?.shareInstance && task.primaryAction !== null)) : undefined
  const hasContent = share !== null || snapshot.breadcrumbs.length > 0

  return <main className={`receiver-shell receiver-content-${share?.kind ?? 'pending'}`}>
    <header className="receiver-header">
      <a className="brand" href="/" aria-label="WindShare home">WindShare</a>
      <div className="receiver-header-actions">
        <TaskDownloads tasks={tasks} snapshot={snapshot} controller={controller} open={downloadsOpen} onOpenChange={setDownloadsOpen} />
      </div>
    </header>
    <div className="share-workspace">
      <div className="share-heading">
        <div className="share-heading-copy">
          <h1 title={share?.name}>{share?.name ?? (snapshot.phase === 'awaiting-key' ? 'Open your share' : 'Shared files')}</h1>
          <div className="share-metadata">
            <button className={`connection-status connection-${snapshot.connection.kind}`} type="button" onClick={() => openDetails('connection')}>
              <ReceiverIcon name="connection" />
              {shareConnectionLabel(snapshot.connection, snapshot.phase, snapshot.status)}
            </button>
            <button className="encryption-note" type="button" onClick={() => openDetails('connection')}>
              <ReceiverIcon name="lock" />Encrypted
            </button>
          </div>
        </div>
        <ReceiverFold compact={single?.kind === 'photo' || single?.kind === 'video'} />
      </div>
      {snapshot.error !== null && <div className="share-error" role="alert">{snapshot.error}</div>}
      {snapshot.phase === 'awaiting-key' && <KeyForm controller={controller} />}
      {!hasContent && snapshot.phase === 'joining' && <p className="share-loading" role="status">Connecting to the sender…</p>}
      {hasContent && <ShareContent share={share} preview={snapshot.preview} previewActions={previewActions} explorer={{ rows: snapshot.rows, breadcrumbs: snapshot.breadcrumbs,
        pageIndex: snapshot.pageIndex, pageCount: snapshot.pageCount, omittedCount: snapshot.omittedCount,
        browse: snapshot.browse, draft: snapshot.draft, actions: {
          openDirectory: id => controller.openDirectory(id), openBreadcrumb: index => controller.openBreadcrumb(index),
          showPage: page => controller.showPage(page), preview: id => controller.previewFile(id),
          toggle: id => controller.toggleSelection(id), enterSelection: () => controller.enterSelectionMode(),
          exitSelection: () => controller.exitSelectionMode(), selectPage: () => controller.selectPage(),
          clearSelection: () => controller.clearSelection(), retry: () => controller.retryDirectory(),
        } }} />}
      {hasContent && <SavingControls model={saving} activation={snapshot.output.activationPresentation}
        currentTaskContext={current !== null && !snapshot.draft.empty && !snapshot.startAdmission.allowed && snapshot.startAdmission.reason !== null
          ? { operationId: current.operationId, reason: snapshot.startAdmission.reason } : null}
        actionLabel={actionLabel} choose={choice => controller.chooseArtifact(choice.offered.choice.choiceId)}
        retry={() => controller.retryOutputConfirmation()} cancel={() => controller.cancelPreparing()}
        onIntent={action => controller.recordExperienceIntent(action)} />}
      {matching !== undefined && <div className="continuation-suggestion">
        <span>Continue {matching.objectLabel}</span>
        <button type="button" onClick={() => { if (matching.primaryAction !== null) actions.perform(matching.primaryAction) }}>
          {matching.primaryAction?.label}
        </button>
      </div>}
      {current !== null && <TaskCard task={current} actions={actions} onDetails={() => openDetails('task')}
        busy={snapshot.retained.pending !== null} />}
      {snapshot.startAdmission.canReleaseCurrent && <p className="new-operation"><button type="button" onClick={() => controller.startNewReceiveOperation()}>Start another download</button></p>}
      {newOperation !== null && <details className="new-operation">
        <summary>{newOperation.title}</summary><p>{newOperation.description}</p>
        <button type="button" onClick={() => controller.startNewReceiveOperation()}>{newOperation.actionLabel}</button>
      </details>}
    </div>
    {single === null && snapshot.preview.state !== 'idle' && <DetailSheet title={snapshot.preview.name}
      onClose={previewActions.close} className="preview-sheet">
      <MediaPreview preview={snapshot.preview} actions={previewActions} />
    </DetailSheet>}
    {details === 'connection' && <DetailSheet title="Encryption and connection" onClose={() => openDetails(null)}>
      <ConnectionDetails status={shareConnectionLabel(snapshot.connection, snapshot.phase, snapshot.status)} path={snapshot.pathActivity} />
    </DetailSheet>}
    {details === 'task' && current !== null && <DetailSheet title={current.objectLabel} onClose={() => openDetails(null)}>
      <TaskDetails task={current} actions={actions} busy={snapshot.retained.pending !== null}>
        <TaskSourceDetails operationId={current.operationId} snapshot={snapshot} controller={controller} />
        {controller.canRetainCurrentOperation && <div className="retained-handoff">
          <p>Keep this paused task in Downloads to continue later or save its complete files as a partial ZIP.</p>
          <button type="button" onClick={async () => {
            if (await controller.retainCurrentOperation()) {
              openDetails(null)
              setDownloadsOpen(true)
            }
          }}>Keep progress in Downloads</button>
        </div>}
      </TaskDetails>
    </DetailSheet>}
  </main>
}
