import { useState, type ReactNode, type Ref } from 'react'
import type { TaskAction, TaskPresentation } from './index'
import { CompatibleNameRepairPanel } from '../compatible-name/CompatibleNameRepairPanel'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon, type ReceiverIconName } from '../receiver-presentation/ReceiverIcon'

const STAGE_ICONS: Readonly<Record<TaskPresentation['stage'], ReceiverIconName>> = {
  preparing: 'clock', downloading: 'download', waiting: 'clock', paused: 'pause',
  finishing: 'clock', 'ready-to-save': 'download', 'handed-to-browser': 'download',
  saved: 'check', 'needs-action': 'alert', cancelled: 'close', failed: 'alert',
}

export interface TaskViewActions {
  readonly perform: (action: TaskAction) => void
  readonly catchUp: (operationId: string) => void
}

export function TaskActionButton({ action, perform, primary = false, busy = false, detailed = false }: {
  readonly action: TaskAction
  readonly perform: (action: TaskAction) => void
  readonly primary?: boolean
  readonly detailed?: boolean
  readonly busy?: boolean
}) {
  const [confirm, setConfirm] = useState(false)
  const dispatch = () => {
    if (action.disabledReason !== null || busy) return
    if (action.destructive) setConfirm(true)
    else perform(action)
  }
  const buttonClass = action.destructive ? 'danger-action' : ''
  return <div className="task-action">
    <button type="button" className={primary ? 'primary-action' : buttonClass}
      disabled={busy || action.disabledReason !== null} title={action.disabledReason ?? undefined}
      onClick={dispatch}>{action.label}</button>
    {action.disabledReason !== null && <small className="action-reason">{action.disabledReason}</small>}
    {!action.destructive && action.consequence !== null && (detailed || action.target.action !== 'pause') &&
      <small className="action-reason">{action.consequence}</small>}
    {confirm && <DetailSheet title={action.label} onClose={() => setConfirm(false)} returnLabel="Keep task">
      <p>{action.consequence ?? 'This removes this task’s owned retained data. Files already exported remain separate.'}</p>
      <button type="button" className="danger-action" onClick={() => { setConfirm(false); perform(action) }}>
        {action.label}
      </button>
    </DetailSheet>}
  </div>
}

export function TaskCard({ task, actions, onDetails, detailsRef, busy = false, primaryAction }: {
  readonly task: TaskPresentation
  readonly actions: TaskViewActions
  readonly onDetails: () => void
  readonly detailsRef?: Ref<HTMLButtonElement>
  readonly primaryAction?: ReactNode
  readonly busy?: boolean
}) {
  return <section className={`task-card task-tone-${task.tone}`} aria-label={`Download: ${task.objectLabel}`}
    data-task-stage={task.stage} data-operation-id={task.operationId}>
    <div className="task-summary">
      <div className="task-identity"><strong title={task.objectLabel}>{task.objectLabel}</strong>
        <span className="task-stage" role="status" aria-live="polite">
          <ReceiverIcon name={STAGE_ICONS[task.stage]} />{task.headline}
        </span>
        {task.destinationLabel !== null && <small>{task.destinationLabel}</small>}
        {task.createdAtMilliseconds !== null && <small><time dateTime={new Date(task.createdAtMilliseconds).toISOString()}>{new Date(task.createdAtMilliseconds).toLocaleString()}</time></small>}
      </div>
      <div className="task-actions">
        {primaryAction ?? (task.primaryAction !== null && <TaskActionButton action={task.primaryAction}
          perform={actions.perform} primary busy={busy} />)}
        <button ref={detailsRef} className="quiet-action" type="button" onClick={onDetails}>Details<ReceiverIcon name="chevron-right" /></button>
      </div>
    </div>
    {task.progress !== null && <div className="task-progress" data-progress-mode={task.progress.mode}>
      <progress aria-label="Download progress" {...(task.progress.percentage === null
        ? {} : { max: 100, value: task.progress.percentage })} />
      <span>{task.progress.label}</span>
    </div>}
    {task.completeness === 'partial' && <p className="task-notice">Partial result — some selected content is unavailable.</p>}
    {task.fidelity?.actionMode === 'catch-up-required' && <p className="task-notice">Filename restoration setup needs attention.</p>}
    {task.fidelity !== null && task.fidelity.replacementCount > 0 &&
      <p className="task-notice">{task.fidelity.replacementCount} {task.fidelity.replacementCount === 1 ? 'filename was' : 'filenames were'} adjusted for this device.</p>}
  </section>
}

export function TaskDetails({ task, actions, busy = false, children }: {
  readonly task: TaskPresentation
  readonly actions: TaskViewActions
  readonly busy?: boolean
  readonly children?: ReactNode
}) {
  return <div className={`task-details task-tone-${task.tone}`} data-task-stage={task.stage}>
    <p className="task-detail-stage" role="status"><ReceiverIcon name={STAGE_ICONS[task.stage]} />{task.headline}</p>
    <p>{task.description}</p>
    <dl className="task-facts">
      <dt>Result</dt><dd>{task.objectLabel}</dd>
      {task.destinationLabel !== null && <><dt>Destination</dt><dd>{task.destinationLabel}</dd></>}
      {task.createdAtMilliseconds !== null && <><dt>Created</dt><dd><time
        dateTime={new Date(task.createdAtMilliseconds).toISOString()}>{new Date(task.createdAtMilliseconds).toLocaleString()}</time></dd></>}
    </dl>
    {task.progress !== null && <p>{task.progress.label}</p>}
    <ul className="task-detail-list">{[...task.details, ...(task.progress?.details ?? [])].map((line, index) =>
      <li key={`${index}-${line}`}>{line}</li>)}</ul>
    <div className="task-actions">
      {task.primaryAction !== null && <TaskActionButton action={task.primaryAction} perform={actions.perform} primary busy={busy} detailed />}
      {task.secondaryActions.map(action => <TaskActionButton key={action.id} action={action} perform={actions.perform} busy={busy} detailed />)}
    </div>
    <CompatibleNameRepairPanel repair={task.fidelity} busy={busy} catchUp={() => actions.catchUp(task.operationId)} />
    {children}
    {task.destructiveActions.length > 0 && <details className="task-removal">
      <summary>Remove task or retained data</summary>
      <p>Removing retained data does not remove files already exported to your device.</p>
      <div className="task-actions">{task.destructiveActions.map(action =>
        <TaskActionButton key={action.id} action={action} perform={actions.perform} busy={busy} detailed />)}</div>
    </details>}
  </div>
}
