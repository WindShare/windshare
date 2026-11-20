import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
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

type TaskSnapshot = ReturnType<ReceiverController['presentation']['task']['getSnapshot']>

function isSingleFileResult(snapshot: TaskSnapshot): boolean {
  const { share, current, output } = snapshot
  return share !== null && share.kind !== 'browser' &&
    (current?.stage === 'saved' || current?.stage === 'handed-to-browser') &&
    output.activationPresentation === null
}

function renderSavingControls(controller: ReceiverController, snapshot: TaskSnapshot, actionLabel: string) {
  const { current, draft, output, startAdmission } = snapshot
  const language = output.language
  const saving = presentSavingActions({
    offers: output.offers, actionLabel,
    blockedBy: draft.empty ? 'emptySelection' : startAdmission.reason,
  }, language)
  return <SavingControls model={saving} activation={output.activationPresentation}
    currentTaskContext={current !== null && !draft.empty && !startAdmission.allowed && saving.disabledReason !== null
      ? { operationId: current.operationId, reason: saving.disabledReason } : null}
    actionLabel={actionLabel}
    choose={choice => controller.chooseArtifact(choice.offered.choice.choiceId, choice.recoveryPreference)}
    retry={() => controller.retryOutputConfirmation()} cancel={() => controller.cancelPreparing()}
    onIntent={action => controller.recordExperienceIntent(action)} />
}

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

export function ReceiverPrimarySaving({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const t = useCopy(receiverCopy).saving
  const singleResult = isSingleFileResult(snapshot)
  const additionalReceive = singleResult && snapshot.current !== null && snapshot.current.primaryAction !== null
  if (!snapshot.hasContent || additionalReceive) return null
  const actionLabel = singleResult ? t.downloadAgain : downloadScopeLabel(snapshot.share, snapshot.draft, t)
  return renderSavingControls(controller, snapshot, actionLabel)
}

export function ReceiverActivity({ controller, openDownloads }: {
  readonly controller: ReceiverController
  readonly openDownloads: () => void
}) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { current, tasks, share, hasContent } = snapshot
  const [details, setDetails] = useState(false)
  const invoker = useRef<HTMLButtonElement>(null)
  const actions = taskActions(controller, snapshot)
  const t = useCopy(receiverCopy).saving
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetails(false) }
  const singleResult = isSingleFileResult(snapshot)
  const actionLabel = singleResult ? t.downloadAgain : downloadScopeLabel(share, snapshot.draft, t)
  const delivered = isReceiveOutputDelivered(snapshot.output.lifecycle)
  const newOperation = delivered ? null : presentNewReceiveOperation({ plan: snapshot.output.plan, lifecycle: snapshot.output.lifecycle }, snapshot.output.language)
  const matching = current === null && share !== null ? tasks.find(task => snapshot.retained.operations.some(operation =>
    operation.operationId === task.operationId && operation.shareInstance === share.shareInstance && task.primaryAction !== null)) : undefined
  const additionalReceive = singleResult && current !== null && current.primaryAction !== null
  return <>
    {hasContent && <InAppBrowserHint />}
    {matching !== undefined && <div className="continuation-suggestion">
      <span>{t.continueObject(matching.objectLabel)}</span>
      <button type="button" onClick={() => { if (matching.primaryAction !== null) actions.perform(matching.primaryAction) }}>{matching.primaryAction?.label}</button>
    </div>}
    {current !== null && <TaskCard task={current} actions={actions} busy={snapshot.retained.pending !== null} onDetails={element => {
      invoker.current = element; controller.recordExperienceIntent('open-task-details'); setDetails(true)
    }} />}
    {hasContent && additionalReceive && <details className="new-operation">
      <summary>{actionLabel}</summary>
      {renderSavingControls(controller, snapshot, actionLabel)}
    </details>}
    {snapshot.startAdmission.canReleaseCurrent && <p className="new-operation"><button type="button" onClick={() => controller.startNewReceiveOperation()}>{t.startAnother}</button></p>}
    {newOperation !== null && <details className="new-operation"><summary>{newOperation.title}</summary><p>{newOperation.description}</p>
      <button type="button" onClick={() => controller.startNewReceiveOperation()}>{newOperation.actionLabel}</button></details>}
    {details && current !== null && <DetailSheet title={current.objectLabel} onClose={closeDetails} returnFocus={invoker}>
      <TaskDetails task={current} actions={actions} busy={snapshot.retained.pending !== null}>
        <TaskSourceDetails operationId={current.operationId} snapshot={snapshot} controller={controller} />
        {controller.canRetainCurrentOperation && <div className="retained-handoff">
          <p>{t.keepHint}</p>
          <button type="button" onClick={async () => {
            if (await controller.retainCurrentOperation()) { closeDetails(); openDownloads() }
          }}>{t.keepProgress}</button>
        </div>}
      </TaskDetails>
    </DetailSheet>}
  </>
}
