import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useRef, useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from '../controller'
import { taskActions } from '../experience/task-actions'
import { downloadScopeLabel } from '../experience/share-presentation'
import { resolveWorkspaceTaskLayout } from '../experience/retained-match'
import { presentNewReceiveOperation } from '../lifecycle-presentation'
import { presentSavingActions } from '../saving'
import { SavingControls } from '../saving/SavingControls'
import { InAppBrowserHint } from '../saving/InAppBrowserHint'
import { DetailSheet } from '../controls/DetailSheet'
import { TaskCard, TaskDetails, type TaskViewActions } from '../tasks/TaskView'
import { TaskDownloads, TaskSourceDetails } from '../experience/TaskDownloads'
import { isReceiveOutputDelivered } from '../../application/receive/completion'

const noSubscription = () => () => undefined

type TaskSnapshot = ReturnType<ReceiverController['presentation']['task']['getSnapshot']>

function retainedBannerLabel(
  banner: NonNullable<ReturnType<typeof resolveWorkspaceTaskLayout>['banner']>,
  t: typeof receiverCopy.en.saving,
): string {
  return banner.kind === 'ready-to-save'
    ? t.readyToSaveObject(banner.task.objectLabel)
    : t.continueObject(banner.task.objectLabel)
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
  const { collapseRemoteSaving, repeatRemoteLabel } = resolveWorkspaceTaskLayout(snapshot)
  if (!snapshot.hasContent || collapseRemoteSaving) return null
  const actionLabel = repeatRemoteLabel ? t.downloadAgain : downloadScopeLabel(snapshot.share, snapshot.draft, t)
  return renderSavingControls(controller, snapshot, actionLabel)
}

export function ReceiverActivity({ controller, openDownloads }: {
  readonly controller: ReceiverController
  readonly openDownloads: () => void
}) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { current, share, hasContent } = snapshot
  const [detailsOperationId, setDetailsOperationId] = useState<string | null>(null)
  const [handoffHelpOpenId, setHandoffHelpOpenId] = useState<string | null>(null)
  const invoker = useRef<HTMLButtonElement>(null)
  const copy = useCopy(receiverCopy)
  const t = copy.saving
  const { primaryTask, collapseRemoteSaving, repeatRemoteLabel, banner } = resolveWorkspaceTaskLayout(snapshot)
  const details = primaryTask !== null && detailsOperationId === primaryTask.operationId
  const handoffHelpOpen = primaryTask !== null && handoffHelpOpenId === primaryTask.operationId
  const baseActions = taskActions(controller, snapshot)
  const actions: TaskViewActions = {
    ...baseActions,
    perform: action => {
      // A repeated handoff click strongly indicates the first browser save was blocked or missed.
      if (action.disabledReason === null &&
          primaryTask?.stage === 'handed-to-browser' &&
          action.target.action === 'redownload') {
        setHandoffHelpOpenId(primaryTask.operationId)
      }
      baseActions.perform(action)
    },
  }
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetailsOperationId(null) }
  const actionLabel = repeatRemoteLabel ? t.downloadAgain : downloadScopeLabel(share, snapshot.draft, t)
  const delivered = isReceiveOutputDelivered(snapshot.output.lifecycle)
  const newOperation = delivered ? null : presentNewReceiveOperation({ plan: snapshot.output.plan, lifecycle: snapshot.output.lifecycle }, snapshot.output.language)
  const bannerLabel = banner === null ? null : retainedBannerLabel(banner, t)
  return <>
    {hasContent && <InAppBrowserHint />}
    {banner !== null && <div className="continuation-suggestion">
      <span>{bannerLabel}</span>
      <button type="button" onClick={() => actions.perform(banner.action)}>{banner.action.label}</button>
    </div>}
    {primaryTask !== null && <TaskCard task={primaryTask} actions={actions} busy={snapshot.retained.pending !== null} onDetails={element => {
      invoker.current = element; controller.recordExperienceIntent('open-task-details'); setDetailsOperationId(primaryTask.operationId)
    }} />}
    {primaryTask?.stage === 'handed-to-browser' && <details className="handoff-help"
      open={handoffHelpOpen}
      onToggle={event => setHandoffHelpOpenId(event.currentTarget.open ? primaryTask.operationId : null)}>
      <summary>{copy.downloads.handoffHelp}</summary>
      <p>{copy.downloads.handoffHelpHint}</p>
    </details>}
    {hasContent && collapseRemoteSaving && <details className="new-operation">
      <summary>{actionLabel}</summary>
      {renderSavingControls(controller, snapshot, actionLabel)}
    </details>}
    {snapshot.startAdmission.canReleaseCurrent && <p className="new-operation"><button type="button" onClick={() => controller.startNewReceiveOperation()}>{t.startAnother}</button></p>}
    {newOperation !== null && <details className="new-operation"><summary>{newOperation.title}</summary><p>{newOperation.description}</p>
      <button type="button" onClick={() => controller.startNewReceiveOperation()}>{newOperation.actionLabel}</button></details>}
    {details && primaryTask !== null && <DetailSheet title={primaryTask.objectLabel} onClose={closeDetails} returnFocus={invoker}>
      <TaskDetails task={primaryTask} actions={actions} busy={snapshot.retained.pending !== null}>
        <TaskSourceDetails operationId={primaryTask.operationId} snapshot={snapshot} controller={controller} />
        {current !== null && primaryTask.operationId === current.operationId && controller.canRetainCurrentOperation && <div className="retained-handoff">
          <p>{t.keepHint}</p>
          <button type="button" onClick={async () => {
            if (await controller.retainCurrentOperation()) { closeDetails(); openDownloads() }
          }}>{t.keepProgress}</button>
        </div>}
      </TaskDetails>
    </DetailSheet>}
  </>
}
