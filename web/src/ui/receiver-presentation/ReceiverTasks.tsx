import { useRef, useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from '../controller'
import { taskActions } from '../experience/task-actions'
import { downloadScopeLabel } from '../experience/share-presentation'
import { presentNewReceiveOperation } from '../lifecycle-presentation'
import { presentSavingActions } from '../saving'
import { SavingControls } from '../saving/SavingControls'
import { InAppBrowserHint } from '../saving/InAppBrowserHint'
import { DetailSheet } from '../controls/DetailSheet'
import { TaskCard, TaskDetails } from '../tasks/TaskView'
import { TaskDownloads, TaskSourceDetails } from '../experience/TaskDownloads'
import { isReceiveOutputDelivered } from '../../application/receive/completion'

const noSubscription = () => () => undefined

export function ReceiverDownloads({ controller, open, onOpenChange, ...entry }: {
  readonly controller: ReceiverController; readonly open: boolean; readonly onOpenChange: (open: boolean) => void
  readonly entryLabel?: string
  readonly entryDescription?: string
}) {
  const history = controller.presentation.downloads
  useSyncExternalStore(history.subscribe, history.getSnapshot, history.getSnapshot)
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(open ? source.subscribe : noSubscription, source.getSnapshot, source.getSnapshot)
  return <TaskDownloads tasks={snapshot.tasks} snapshot={snapshot} controller={controller} open={open} onOpenChange={onOpenChange} {...entry} />
}

export function ReceiverTasks({ controller, openDownloads }: { readonly controller: ReceiverController; readonly openDownloads: () => void }) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { current, tasks, share, hasContent } = snapshot
  const [details, setDetails] = useState(false)
  const invoker = useRef<HTMLButtonElement>(null)
  const actions = taskActions(controller, snapshot)
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetails(false) }
  const singleResult = share !== null && share.kind !== 'browser' &&
    (current?.stage === 'saved' || current?.stage === 'handed-to-browser') && snapshot.output.activationPresentation === null
  const actionLabel = singleResult ? 'Download from sender again' : downloadScopeLabel(share, snapshot.draft)
  const saving = presentSavingActions({
    offers: snapshot.output.offers, actionLabel,
    disabledReason: snapshot.draft.empty ? 'Select items to download' : snapshot.startAdmission.reason,
  })
  const delivered = isReceiveOutputDelivered(snapshot.output.lifecycle)
  const newOperation = delivered ? null : presentNewReceiveOperation({ plan: snapshot.output.plan, lifecycle: snapshot.output.lifecycle })
  const matching = current === null && share !== null ? tasks.find(task => snapshot.retained.operations.some(operation =>
    operation.operationId === task.operationId && operation.shareInstance === share.shareInstance && task.primaryAction !== null)) : undefined
  const additionalReceive = singleResult && current !== null && current.primaryAction !== null
  const savingControls = <SavingControls model={saving} activation={snapshot.output.activationPresentation}
    currentTaskContext={current !== null && !snapshot.draft.empty && !snapshot.startAdmission.allowed && snapshot.startAdmission.reason !== null
      ? { operationId: current.operationId, reason: snapshot.startAdmission.reason } : null}
    actionLabel={actionLabel} choose={choice => controller.chooseArtifact(choice.offered.choice.choiceId, choice.recoveryPreference)}
    retry={() => controller.retryOutputConfirmation()} cancel={() => controller.cancelPreparing()}
    onIntent={action => controller.recordExperienceIntent(action)} />
  return <>
    {hasContent && <InAppBrowserHint />}
    {hasContent && !additionalReceive && savingControls}
    {matching !== undefined && <div className="continuation-suggestion">
      <span>Continue {matching.objectLabel}</span>
      <button type="button" onClick={() => { if (matching.primaryAction !== null) actions.perform(matching.primaryAction) }}>{matching.primaryAction?.label}</button>
    </div>}
    {current !== null && <TaskCard task={current} actions={actions} busy={snapshot.retained.pending !== null} onDetails={element => {
      invoker.current = element; controller.recordExperienceIntent('open-task-details'); setDetails(true)
    }} />}
    {hasContent && additionalReceive && <details className="new-operation"><summary>{actionLabel}</summary>{savingControls}</details>}
    {snapshot.startAdmission.canReleaseCurrent && <p className="new-operation"><button type="button" onClick={() => controller.startNewReceiveOperation()}>Start another download</button></p>}
    {newOperation !== null && <details className="new-operation"><summary>{newOperation.title}</summary><p>{newOperation.description}</p>
      <button type="button" onClick={() => controller.startNewReceiveOperation()}>{newOperation.actionLabel}</button></details>}
    {details && current !== null && <DetailSheet title={current.objectLabel} onClose={closeDetails} returnFocus={invoker}>
      <TaskDetails task={current} actions={actions} busy={snapshot.retained.pending !== null}>
        <TaskSourceDetails operationId={current.operationId} snapshot={snapshot} controller={controller} />
        {controller.canRetainCurrentOperation && <div className="retained-handoff">
          <p>Keep this paused task in Downloads to continue later or save its complete received files.</p>
          <button type="button" onClick={async () => {
            if (await controller.retainCurrentOperation()) { closeDetails(); openDownloads() }
          }}>Keep progress in Downloads</button>
        </div>}
      </TaskDetails>
    </DetailSheet>}
  </>
}
