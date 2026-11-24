import { useState } from 'react'
import type { ReceiverController } from '../controller'
import type { ReceiverSnapshot } from '../model'
import { Downloads } from '../downloads/Downloads'
import type { TaskPresentation } from '../tasks'
import { safeNameRestoreActions, taskActions } from './task-actions'
import { SourceRevisionFailuresPanel } from '../source-replacement/SourceRevisionFailuresPanel'

export function TaskSourceDetails({ operationId, snapshot, controller }: {
  readonly operationId: string
  readonly snapshot: Pick<ReceiverSnapshot, 'retained' | 'output'>
  readonly controller: ReceiverController
}) {
  const task = snapshot.retained.tasks.find(candidate => candidate.taskId === operationId)
  return task === undefined
    ? null
    : <SourceRevisionFailuresPanel
        task={task}
        busy={snapshot.retained.pending !== null}
        prepareReplacement={failure => controller.prepareReplacementDownload(task, failure)}
      />
}

export function TaskDownloads({ tasks, snapshot, controller, entryLabel, entryDescription, open, onOpenChange }: {
  readonly tasks: readonly TaskPresentation[]
  readonly snapshot: Pick<ReceiverSnapshot, 'retained' | 'output'>
  readonly controller: ReceiverController
  readonly entryLabel?: string
  readonly entryDescription?: string
  readonly open?: boolean
  readonly onOpenChange?: (open: boolean) => void
}) {
  const [localOpen, setLocalOpen] = useState(false)
  return <Downloads
    open={open ?? localOpen}
    onOpenChange={next => {
      (onOpenChange ?? setLocalOpen)(next)
      if (next) void controller.ensureDownloads()
    }}
    tasks={tasks}
    actions={taskActions(controller)}
    safeNameRestore={safeNameRestoreActions(controller)}
    inventory={snapshot.retained.kind}
    onRetry={() => { void controller.ensureDownloads() }}
    busy={snapshot.retained.pending !== null}
    {...(entryLabel === undefined ? {} : { entryLabel })}
    {...(entryDescription === undefined ? {} : { entryDescription })}
    onIntent={action => controller.recordExperienceIntent(action)}
    details={operationId => <TaskSourceDetails operationId={operationId} snapshot={snapshot} controller={controller} />}
  />
}
