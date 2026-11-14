import { useState } from 'react'
import type { ReceiverController } from '../controller'
import type { ReceiverSnapshot } from '../model'
import { Downloads } from '../downloads/Downloads'
import type { TaskPresentation } from '../tasks'
import { taskActions } from './task-actions'
import { SourceRevisionFailuresPanel } from '../source-replacement/SourceRevisionFailuresPanel'

export function TaskSourceDetails({ operationId, snapshot, controller }: {
  readonly operationId: string
  readonly snapshot: Pick<ReceiverSnapshot, 'retained' | 'output'>
  readonly controller: ReceiverController
}) {
  const operation = snapshot.retained.operations.find(candidate => candidate.operationId === operationId)
  return operation === undefined ? null : <SourceRevisionFailuresPanel operation={operation}
    busy={snapshot.retained.pending !== null}
    prepareReplacement={failure => controller.prepareReplacementDownload(operation, failure)} />
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
  return <Downloads open={open ?? localOpen} onOpenChange={next => {
    (onOpenChange ?? setLocalOpen)(next)
    if (next) void controller.ensureDownloads()
  }} tasks={tasks} actions={taskActions(controller, snapshot)}
    loading={snapshot.retained.kind === 'unloaded' || snapshot.retained.kind === 'loading'} error={snapshot.retained.error}
    onRetry={() => { void controller.ensureDownloads() }}
    busy={snapshot.retained.pending !== null} {...(entryLabel === undefined ? {} : { entryLabel })} {...(entryDescription === undefined ? {} : { entryDescription })} onIntent={action => controller.recordExperienceIntent(action)}
    details={operationId => <TaskSourceDetails operationId={operationId} snapshot={snapshot} controller={controller} />} />
}
