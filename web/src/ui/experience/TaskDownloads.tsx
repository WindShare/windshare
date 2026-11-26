import { useEffect, useState } from 'react'
import type { ReceiverController } from '../controller'
import type { ReceiverSnapshot } from '../model'
import { Downloads } from '../downloads/Downloads'
import type { TaskPresentation } from '../tasks'
import { safeNameRestoreActions, taskActions } from './task-actions'
import { SourceRevisionFailuresPanel } from '../source-replacement/SourceRevisionFailuresPanel'

export function TaskSourceDetails({ operationId, snapshot, controller, onPrepared }: {
  readonly operationId: string
  readonly snapshot: Pick<ReceiverSnapshot, 'retained' | 'output'>
  readonly controller: ReceiverController
  readonly onPrepared?: () => void
}) {
  const task = snapshot.retained.tasks.find(candidate => candidate.taskId === operationId)
  if (task === undefined) return null
  return <SourceRevisionFailuresPanel
    task={task}
    admission={controller.replacementAdmission(task)}
    prepareReplacement={(failure, callbacks) => controller.prepareReplacementDownload(task, failure, callbacks)}
    {...(onPrepared === undefined ? {} : { onPrepared })}
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
  const isOpen = open ?? localOpen
  useEffect(() => {
    if (!isOpen || typeof document === 'undefined') return
    // Revalidate when returning to a tab whose Downloads sheet stayed open so
    // tasks paused or closed in another tab unlock without closing the sheet.
    const onVisibilityChange = () => {
      if (document.visibilityState === 'visible') {
        void controller.refreshDownloads()
      }
    }
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => { document.removeEventListener('visibilitychange', onVisibilityChange) }
  }, [isOpen, controller])
  const handleOpenChange = (next: boolean) => {
    if (onOpenChange !== undefined) {
      onOpenChange(next)
      return
    }
    if (next && !localOpen) void controller.refreshDownloads()
    setLocalOpen(next)
  }
  return <Downloads
    open={isOpen}
    onOpenChange={handleOpenChange}
    tasks={tasks}
    actions={taskActions(controller)}
    safeNameRestore={safeNameRestoreActions(controller)}
    inventory={snapshot.retained.kind}
    onRetry={() => { void controller.refreshDownloads() }}
    busy={snapshot.retained.pending !== null}
    {...(entryLabel === undefined ? {} : { entryLabel })}
    {...(entryDescription === undefined ? {} : { entryDescription })}
    onIntent={action => controller.recordExperienceIntent(action)}
    details={(operationId, closeSheet) => (
      <TaskSourceDetails operationId={operationId} snapshot={snapshot} controller={controller} onPrepared={closeSheet} />
    )}
  />
}
