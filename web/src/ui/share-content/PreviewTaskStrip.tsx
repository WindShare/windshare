import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from '../controller'
import { taskActions } from '../experience/task-actions'
import { receiverCopy } from '../locales/receiver'
import type { ReceiverFailure } from '../model'
import { useCopy } from '../preferences/use-preferences'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'
import { useReceiveRate } from '../tasks/use-receive-rate'
import type { TaskPresentation } from '../tasks'

const DELIVERED_NOTICE_MS = 3_000

const ACTIVE_STAGES = new Set<TaskPresentation['stage']>(['preparing', 'downloading', 'waiting', 'finishing'])
const ACTIONABLE_STAGES = new Set<TaskPresentation['stage']>(['paused', 'needs-action', 'ready-to-save', 'failed'])
const DELIVERED_STAGES = new Set<TaskPresentation['stage']>(['handed-to-browser', 'saved'])

function taskSignature(task: TaskPresentation | null): string | null {
  return task === null ? null : `${task.operationId}:${task.generation}:${task.stage}`
}

function joinStatusParts(...parts: readonly (string | null | undefined)[]): string {
  return parts.filter((part): part is string => part !== null && part !== undefined && part.length > 0).join(' · ')
}

function activeTaskStatusText(task: TaskPresentation, rate: string | null): string {
  const progress = task.progress
  if (progress === null) return task.headline
  if (task.stage === 'preparing') {
    return progress.receivedObjectBytes > 0n || progress.writtenBytes > 0n
      ? joinStatusParts(task.headline, progress.label, progress.status)
      : task.headline
  }
  if (task.stage === 'waiting' || task.stage === 'finishing') {
    return joinStatusParts(task.headline, progress.label, progress.status)
  }
  return joinStatusParts(progress.label, rate, progress.status)
}

function ActivePreviewTaskStrip({ task }: { readonly task: TaskPresentation }) {
  const copy = useCopy(receiverCopy)
  const rate = useReceiveRate(task)
  return <div className={`media-preview-task-strip task-tone-${task.tone}`} data-task-stage={task.stage}>
    <div className="media-preview-task-head">
      <strong className="media-preview-task-name" title={task.objectLabel}>{task.objectLabel}</strong>
      <span className="media-preview-task-status" role="status">{activeTaskStatusText(task, rate)}</span>
    </div>
    {task.progress !== null && <div className="media-preview-task-progress">
      <progress aria-label={copy.downloads.progress} {...(task.progress.percentage === null
        ? {} : { max: 100, value: task.progress.percentage })} />
    </div>}
  </div>
}

/**
 * Independently subscribes to `controller.presentation.task` so background transfer
 * progress ticks never re-render the media preview surface or file explorer.
 */
export function PreviewTaskStrip({
  controller,
  onActionFailure,
  onClearActionFailure,
}: {
  readonly controller: ReceiverController
  readonly onActionFailure?: (failure: ReceiverFailure) => void
  readonly onClearActionFailure?: () => void
}) {
  const source = controller.presentation.task
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const task = snapshot.current
  const activation = snapshot.output.activationPresentation
  const [initialSignature] = useState(() => taskSignature(task))
  const [expiredSignature, setExpiredSignature] = useState<string | null>(null)
  const mountedRef = useRef(true)
  // Remember the snapshot signature at the moment a failure was reported so batched
  // store updates from the failing action do not immediately wipe the new error.
  const reportedErrorSignatureRef = useRef<string | null | undefined>(undefined)
  const currentSignature = taskSignature(task)
  const showDelivered =
    task !== null &&
    DELIVERED_STAGES.has(task.stage) &&
    currentSignature !== null &&
    currentSignature !== initialSignature &&
    expiredSignature !== currentSignature

  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    if (reportedErrorSignatureRef.current === undefined) return
    if (reportedErrorSignatureRef.current === currentSignature) return
    reportedErrorSignatureRef.current = undefined
    onClearActionFailure?.()
  }, [currentSignature, onClearActionFailure])

  useEffect(() => {
    if (!showDelivered || currentSignature === null) return
    const timer = setTimeout(() => setExpiredSignature(currentSignature), DELIVERED_NOTICE_MS)
    return () => clearTimeout(timer)
  }, [currentSignature, showDelivered])

  if (activation !== null) {
    return <div className="media-preview-task-strip" role="status">
      <div className="media-preview-task-head">
        <span className="media-preview-task-name"><ReceiverIcon name="clock" />{activation.title}</span>
        <span className="media-preview-task-status">{activation.description}</span>
      </div>
    </div>
  }

  if (task === null) return null

  if (ACTIVE_STAGES.has(task.stage)) {
    return <ActivePreviewTaskStrip task={task} />
  }

  if (ACTIONABLE_STAGES.has(task.stage)) {
    const primary = task.primaryAction
    const busy = snapshot.retained.pending !== null
    return <div className={`media-preview-task-strip task-tone-${task.tone}`} data-task-stage={task.stage}>
      <div className="media-preview-task-row">
        <div className="media-preview-task-copy">
          <strong className="media-preview-task-name" title={task.objectLabel}>{task.objectLabel}</strong>
          <span className="media-preview-task-reason" role="status">
            {task.headline}{task.description ? ` · ${task.description}` : ''}
          </span>
        </div>
        {primary !== null && <button type="button" className="media-preview-task-action"
          disabled={busy || primary.disabledReason !== null}
          title={primary.disabledReason ?? undefined}
          onClick={() => {
            reportedErrorSignatureRef.current = undefined
            onClearActionFailure?.()
            taskActions(controller, onActionFailure === undefined ? undefined : {
              onAsyncFailure: failure => {
                if (!mountedRef.current) return false
                reportedErrorSignatureRef.current = taskSignature(source.getSnapshot().current)
                onActionFailure(failure)
                return true
              },
            }).perform(primary)
          }}>
          {primary.label}
        </button>}
      </div>
    </div>
  }

  if (showDelivered) {
    return <div className={`media-preview-task-strip task-tone-${task.tone}`} role="status" data-task-stage={task.stage}>
      <div className="media-preview-task-head">
        <strong className="media-preview-task-name" title={task.objectLabel}>{task.objectLabel}</strong>
        <span className="media-preview-task-status">
          <ReceiverIcon name={task.stage === 'saved' ? 'check' : 'download'} />{task.headline}
        </span>
      </div>
    </div>
  }

  return null
}
