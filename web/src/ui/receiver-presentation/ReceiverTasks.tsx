import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useRef, useState, useSyncExternalStore } from 'react'
import type { ChooseArtifactIntent, ReceiverController } from '../controller'
import { safeNameRestoreActions, taskActions } from '../experience/task-actions'
import { downloadScopeLabel } from '../experience/share-presentation'
import { dismissedTaskKey, isDismissibleWorkspaceTask, resolveWorkspaceTaskLayout } from '../experience/retained-match'
import { presentSavingActions } from '../saving'
import { SavingControls } from '../saving/SavingControls'
import { InAppBrowserHint } from '../saving/InAppBrowserHint'
import { DetailSheet } from '../controls/DetailSheet'
import type { TaskDetailSection } from '../tasks'
import { TaskCard, TaskDetails, type TaskViewActions } from '../tasks/TaskView'
import { TaskDownloads, TaskSourceDetails } from '../experience/TaskDownloads'

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

function renderSavingControls(
  controller: ReceiverController,
  snapshot: TaskSnapshot,
  actionLabel: string,
  intent: ChooseArtifactIntent,
) {
  const { current, draft, output, startAdmission } = snapshot
  const language = output.language
  const saving = presentSavingActions({
    offers: output.offers,
    actionLabel,
    blockedBy: draft.empty ? 'emptySelection' : startAdmission.reason,
    localChoice: intent === 'default' && !draft.empty ? startAdmission.localChoice : null,
  }, language)
  return <SavingControls
    model={saving}
    activation={output.activationPresentation}
    currentTaskContext={current !== null && !draft.empty && saving.primary !== null && saving.primary.disabledReason !== null
      ? { operationId: current.operationId, reason: saving.primary.disabledReason }
      : null}
    actionLabel={actionLabel}
    choose={choice => controller.chooseArtifact(choice.choice.choiceId, intent)}
    retry={() => controller.retryOutputConfirmation()}
    cancel={() => controller.cancelPreparing()}
    onIntent={action => controller.recordExperienceIntent(action)}
  />
}

export function ReceiverDownloads({ controller, open, onOpenChange, ...entry }: {
  readonly controller: ReceiverController
  readonly open: boolean
  readonly onOpenChange: (open: boolean) => void
  readonly entryLabel?: string
  readonly entryDescription?: string
}) {
  const history = controller.presentation.downloads
  useSyncExternalStore(history.subscribe, history.getSnapshot, history.getSnapshot)
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(open ? source.subscribe : noSubscription, source.getSnapshot, source.getSnapshot)
  return <TaskDownloads
    tasks={snapshot.tasks}
    snapshot={snapshot}
    controller={controller}
    open={open}
    onOpenChange={onOpenChange}
    {...entry}
  />
}

export function ReceiverPrimarySaving({ controller, dismissedTaskKeys }: {
  readonly controller: ReceiverController
  readonly dismissedTaskKeys?: ReadonlySet<string>
}) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const t = useCopy(receiverCopy).saving
  const { collapseRemoteSaving, repeatRemoteLabel } = resolveWorkspaceTaskLayout(snapshot, dismissedTaskKeys)
  if (!snapshot.hasContent || collapseRemoteSaving) return null
  const actionLabel = repeatRemoteLabel ? t.downloadAgain : downloadScopeLabel(snapshot.share, snapshot.draft, t)
  return renderSavingControls(controller, snapshot, actionLabel, repeatRemoteLabel ? 'redownload' : 'default')
}

export function ReceiverActivity({ controller, openDownloads, dismissedTaskKeys, onDismissTask }: {
  readonly controller: ReceiverController
  readonly openDownloads: () => void
  readonly dismissedTaskKeys?: ReadonlySet<string>
  readonly onDismissTask?: (taskKey: string) => void
}) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const { current, share, hasContent } = snapshot
  const [detailsTarget, setDetailsTarget] = useState<{ readonly operationId: string; readonly section: TaskDetailSection } | null>(null)
  const [handoffHelpOpenId, setHandoffHelpOpenId] = useState<string | null>(null)
  const invoker = useRef<HTMLButtonElement>(null)
  const copy = useCopy(receiverCopy)
  const t = copy.saving
  const { primaryTask, collapseRemoteSaving, repeatRemoteLabel, banner } = resolveWorkspaceTaskLayout(snapshot, dismissedTaskKeys)
  const details = primaryTask !== null && detailsTarget?.operationId === primaryTask.operationId
  const handoffHelpOpen = primaryTask !== null && handoffHelpOpenId === primaryTask.operationId
  const baseActions = taskActions(controller)
  const restoreActions = safeNameRestoreActions(controller)
  const actions: TaskViewActions = {
    ...baseActions,
    perform: (action, callbacks) => {
      // A repeated handoff click strongly indicates the first browser save was blocked or missed.
      if (
        action.disabledReason === null &&
        primaryTask?.stage === 'handed-to-browser' &&
        action.target.action === 'save'
      ) {
        setHandoffHelpOpenId(primaryTask.operationId)
      }
      baseActions.perform(action, callbacks)
    },
  }
  const closeDetails = () => { controller.recordExperienceIntent('close-details'); setDetailsTarget(null) }
  const actionLabel = repeatRemoteLabel ? t.downloadAgain : downloadScopeLabel(share, snapshot.draft, t)
  const bannerLabel = banner === null ? null : retainedBannerLabel(banner, t)
  return <>
    {hasContent && <InAppBrowserHint />}
    {banner !== null && <div className="continuation-suggestion">
      <span>{bannerLabel}</span>
      <button type="button" onClick={() => actions.perform(banner.action)}>{banner.action.label}</button>
    </div>}
    {primaryTask !== null && <TaskCard
      task={primaryTask}
      actions={actions}
      busy={snapshot.retained.pending !== null}
      {...(onDismissTask !== undefined && isDismissibleWorkspaceTask(primaryTask)
        ? { onDismiss: () => onDismissTask(dismissedTaskKey(primaryTask.operationId, primaryTask.generation)) }
        : {})}
      onDetails={(element, section) => {
        invoker.current = element
        controller.recordExperienceIntent('open-task-details')
        setDetailsTarget({ operationId: primaryTask.operationId, section: section ?? 'overview' })
      }}
    />}
    {primaryTask?.stage === 'handed-to-browser' && <details
      className="handoff-help"
      open={handoffHelpOpen}
      onToggle={event => setHandoffHelpOpenId(event.currentTarget.open ? primaryTask.operationId : null)}
    >
      <summary>{copy.downloads.handoffHelp}</summary>
      <p>{copy.downloads.handoffHelpHint}</p>
    </details>}
    {hasContent && collapseRemoteSaving && <details className="new-operation">
      <summary>{actionLabel}</summary>
      {renderSavingControls(controller, snapshot, actionLabel, repeatRemoteLabel ? 'redownload' : 'default')}
    </details>}
    {snapshot.startAdmission.canReleaseCurrent && <p className="new-operation">
      <button type="button" onClick={() => controller.startNewReceiveOperation()}>{t.startAnother}</button>
    </p>}
    {details && primaryTask !== null && <DetailSheet title={primaryTask.objectLabel} onClose={closeDetails} returnFocus={invoker}>
      <TaskDetails
        task={primaryTask}
        actions={actions}
        safeNameRestore={restoreActions}
        initialSection={detailsTarget?.section ?? 'overview'}
        busy={snapshot.retained.pending !== null}
      >
        <TaskSourceDetails
          operationId={primaryTask.operationId}
          snapshot={snapshot}
          controller={controller}
          onPrepared={() => setDetailsTarget(null)}
        />
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
