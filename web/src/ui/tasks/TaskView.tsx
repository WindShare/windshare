import { receiverCopy } from '../locales/receiver'
import { useCopy, usePreferences } from '../preferences/use-preferences'
import { useRef, useState, type ReactNode, type Ref } from 'react'
import type { TaskAction, TaskPresentation } from './index'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon, type ReceiverIconName } from '../receiver-presentation/ReceiverIcon'
import { useReceiveRate } from './use-receive-rate'
import { formatElapsedTime } from './elapsed'

const STAGE_ICONS: Readonly<Record<TaskPresentation['stage'], ReceiverIconName>> = {
  preparing: 'clock', downloading: 'download', waiting: 'clock', paused: 'pause',
  finishing: 'clock', 'ready-to-save': 'download', 'handed-to-browser': 'download',
  saved: 'check', 'needs-action': 'alert', cancelled: 'close', failed: 'alert',
}

// Routine controls speak for themselves; avoid dumping explanatory prose beneath them.
const SELF_EXPLANATORY_ACTIONS = new Set(['pause', 'continue'])
// Local re-save explains itself inside TaskDetails while keeping compact TaskCards uncluttered.
const DETAILS_ONLY_CONSEQUENCE_ACTIONS = new Set(['save'])

export interface TaskViewActions {
  readonly perform: (action: TaskAction) => void
}

export function TaskActionButton({ action, perform, primary = false, busy = false, detailed = false }: {
  readonly action: TaskAction
  readonly perform: (action: TaskAction) => void
  readonly primary?: boolean
  readonly detailed?: boolean
  readonly busy?: boolean
}) {
  const t = useCopy(receiverCopy).downloads
  const [confirm, setConfirm] = useState(false)
  const actionButton = useRef<HTMLButtonElement>(null)
  const dispatch = () => {
    if (action.disabledReason !== null || busy) return
    if (action.destructive) setConfirm(true)
    else perform(action)
  }
  const showConsequence = !action.destructive && action.consequence !== null &&
    !SELF_EXPLANATORY_ACTIONS.has(action.target.action) &&
    (detailed || !DETAILS_ONLY_CONSEQUENCE_ACTIONS.has(action.target.action))
  const buttonClass = action.destructive ? 'danger-action' : ''
  return <div className="task-action">
    <button ref={actionButton} type="button" className={primary ? 'primary-action' : buttonClass}
      disabled={busy || action.disabledReason !== null}
      title={action.disabledReason ?? (showConsequence ? undefined : action.consequence ?? undefined)}
      onClick={dispatch}>{action.label}</button>
    {action.disabledReason !== null && <small className="action-reason">{action.disabledReason}</small>}
    {showConsequence && <small className="action-reason">{action.consequence}</small>}
    {confirm && <DetailSheet title={action.label} onClose={() => setConfirm(false)} returnFocus={actionButton} returnLabel={t.keepTask}>
      <p>{action.consequence ?? t.removeConsequence}</p>
      <button type="button" className="danger-action" onClick={() => { setConfirm(false); perform(action) }}>
        {action.label}
      </button>
    </DetailSheet>}
  </div>
}

export function TaskCard({ task, actions, onDetails, detailsRef, busy = false, showTimestamp = false }: {
  readonly task: TaskPresentation
  readonly actions: TaskViewActions
  readonly onDetails: (invoker: HTMLButtonElement) => void
  readonly detailsRef?: Ref<HTMLButtonElement>
  readonly busy?: boolean
  readonly showTimestamp?: boolean
}) {
  const { language } = usePreferences()
  const t = receiverCopy[language].downloads
  const rate = useReceiveRate(task)
  const hasMeta = task.destinationLabel !== null || task.elapsedMilliseconds !== null || (showTimestamp && task.createdAtMilliseconds !== null)
  return <section className={`task-card task-tone-${task.tone}`} aria-label={t.downloadObject(task.objectLabel)}
    data-task-stage={task.stage} data-operation-id={task.operationId}>
    <div className="task-summary">
      <div className="task-identity">
        <div className="task-identity-header">
          <strong title={task.objectLabel}>{task.objectLabel}</strong>
          <span className="task-stage" role="status" aria-live="polite">
            <ReceiverIcon name={STAGE_ICONS[task.stage]} />{task.headline}
          </span>
        </div>
        {hasMeta && <div className="task-meta-line">
          {task.destinationLabel !== null && <small>{task.destinationLabel}</small>}
          {task.elapsedMilliseconds !== null && <small title={t.elapsedDescription}>{t.elapsedLabel(formatElapsedTime(task.elapsedMilliseconds, t))}</small>}
          {showTimestamp && task.createdAtMilliseconds !== null && <small><time dateTime={new Date(task.createdAtMilliseconds).toISOString()}>{new Date(task.createdAtMilliseconds).toLocaleString(language)}</time></small>}
        </div>}
      </div>
      <div className="task-actions">
        {task.primaryAction !== null && <TaskActionButton action={task.primaryAction}
          perform={actions.perform} primary busy={busy} />}
        <button ref={detailsRef} className="quiet-action" type="button" onClick={event => onDetails(event.currentTarget)}>{t.details}<ReceiverIcon name="chevron-right" /></button>
      </div>
    </div>
    {task.progress !== null && <div className="task-progress" data-progress-mode={task.progress.mode}>
      <progress aria-label={t.progress} {...(task.progress.percentage === null
        ? {} : { max: 100, value: task.progress.percentage })} />
      <div className="task-progress-copy">
        <span>{task.progress.label}{rate === null ? '' : ` · ${rate}`}</span>
        {task.progress.status !== null && <span>{task.progress.status}</span>}
      </div>
    </div>}
    {task.completeness === 'partial' && <p className="task-notice">{t.partial}</p>}
    {task.safeRenamed !== null && task.safeRenamed.count > 0 &&
      <p className="task-notice">{t.adjustedCount(task.safeRenamed.count)}</p>}
  </section>
}

function TaskDetailHero({ task, rate }: { readonly task: TaskPresentation; readonly rate: string | null }) {
  const t = useCopy(receiverCopy).downloads
  const headlineMetric = task.progress?.percentage !== null && task.progress?.percentage !== undefined
    ? `${task.progress.percentage}%` : rate
  return <div className="task-detail-hero">
    <div className="task-detail-hero-head">
      <span className="task-stage" role="status">
        <ReceiverIcon name={STAGE_ICONS[task.stage]} />{task.headline}
      </span>
      {headlineMetric !== null && <span className="task-detail-metric">{headlineMetric}</span>}
    </div>
    {task.progress !== null && <div className="task-progress" data-progress-mode={task.progress.mode}>
      <progress aria-label={t.progress} {...(task.progress.percentage === null
        ? {} : { max: 100, value: task.progress.percentage })} />
      <div className="task-progress-copy">
        <span>{task.progress.label}{rate === null || headlineMetric === rate ? '' : ` · ${rate}`}</span>
        {task.progress.status !== null && <span>{task.progress.status}</span>}
      </div>
    </div>}
    <p className="task-detail-summary">{task.description}</p>
  </div>
}

function TaskDetailFacts({ task }: { readonly task: TaskPresentation }) {
  const { language } = usePreferences()
  const t = receiverCopy[language].downloads
  const hasFacts = task.destinationLabel !== null || task.elapsedMilliseconds !== null || task.createdAtMilliseconds !== null
  if (!hasFacts) return null
  return <dl className="detail-inspector task-facts">
    {task.destinationLabel !== null && <div className="detail-fact-row">
      <dt>{t.destination}</dt><dd>{task.destinationLabel}</dd>
    </div>}
    {task.elapsedMilliseconds !== null && <div className="detail-fact-row" title={t.elapsedDescription}>
      <dt>{t.elapsed}</dt><dd>{formatElapsedTime(task.elapsedMilliseconds, t)}</dd>
    </div>}
    {task.createdAtMilliseconds !== null && <div className="detail-fact-row">
      <dt>{t.created}</dt><dd><time dateTime={new Date(task.createdAtMilliseconds).toISOString()}>{new Date(task.createdAtMilliseconds).toLocaleString(language)}</time></dd>
    </div>}
  </dl>
}

export function TaskDetails({ task, actions, busy = false, children }: {
  readonly task: TaskPresentation
  readonly actions: TaskViewActions
  readonly busy?: boolean
  readonly children?: ReactNode
}) {
  const rate = useReceiveRate(task)
  const t = useCopy(receiverCopy).downloads
  const hasActions = task.primaryAction !== null || task.secondaryActions.length > 0
  const technicalLines = task.progress?.details ?? []
  const safeRenamed = task.safeRenamed !== null && task.safeRenamed.sample.length > 0 ? task.safeRenamed : null
  const remainingAdjusted = safeRenamed === null ? 0 : Math.max(0, safeRenamed.count - safeRenamed.sample.length)
  return <div className={`task-details task-tone-${task.tone}`} data-task-stage={task.stage}>
    <TaskDetailHero task={task} rate={rate} />
    {hasActions && <div className="task-actions task-detail-actions">
      {task.primaryAction !== null && <TaskActionButton action={task.primaryAction} perform={actions.perform} primary busy={busy} detailed />}
      {task.secondaryActions.map(action => <TaskActionButton key={action.id} action={action} perform={actions.perform} busy={busy} detailed />)}
    </div>}
    <TaskDetailFacts task={task} />
    {task.details.length > 0 && <ul className="task-detail-notices">
      {task.details.map((line, index) => <li key={`${index}-${line}`}>{line}</li>)}
    </ul>}
    {children}
    {(task.stage === 'handed-to-browser' || safeRenamed !== null || technicalLines.length > 0 || task.destructiveActions.length > 0) && <div className="detail-disclosures">
      {task.stage === 'handed-to-browser' && <details className="detail-disclosure task-handoff-help">
        <summary>{t.handoffHelp}</summary>
        <p>{t.handoffHelpHint}</p>
      </details>}
      {safeRenamed !== null && <details className="detail-disclosure">
        <summary>{t.adjustedNames}</summary>
        <ul className="task-detail-list">
          {safeRenamed.sample.map((entry, index) => <li key={`${index}-${entry.physicalName}`}>
            {entry.logicalPath.join('/')} → {entry.physicalName}
          </li>)}
          {remainingAdjusted > 0 && <li>{t.moreAdjusted(remainingAdjusted)}</li>}
        </ul>
      </details>}
      {technicalLines.length > 0 && <details className="detail-disclosure">
        <summary>{t.transferDetails}</summary>
        <ul className="task-detail-list">{technicalLines.map((line, index) => <li key={`${index}-${line}`}>{line}</li>)}</ul>
      </details>}
      {task.destructiveActions.length > 0 && <details className="detail-disclosure task-removal">
        <summary>{t.remove}</summary>
        <p>{t.removeHint}</p>
        <div className="task-actions">{task.destructiveActions.map(action =>
          <TaskActionButton key={action.id} action={action} perform={actions.perform} busy={busy} detailed />)}</div>
      </details>}
    </div>}
  </div>
}
