import { useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import type { TaskPresentation } from '../tasks'
import { TaskCard, TaskDetails, type TaskViewActions } from '../tasks/TaskView'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export function Downloads({ tasks, actions, loading, error, busy, details, entryLabel = 'Downloads', entryDescription, onIntent, open, onOpenChange }: {
  readonly tasks: readonly TaskPresentation[]
  readonly actions: TaskViewActions
  readonly loading: boolean
  readonly error: string | null
  readonly busy: boolean
  readonly details?: (operationId: string) => ReactNode
  readonly entryLabel?: string
  readonly entryDescription?: string
  readonly onIntent: (action: string) => void
  readonly open: boolean
  readonly onOpenChange: (open: boolean) => void
}) {
  const [expanded, setExpanded] = useState<string | null>(null)
  const descriptionId = useId()
  const entryButton = useRef<HTMLButtonElement>(null)
  const detailsButtons = useRef(new Map<string, HTMLButtonElement>())
  const backButton = useRef<HTMLButtonElement>(null)
  const index = useRef<HTMLDivElement>(null)
  const previousSelection = useRef<string | undefined>(undefined)
  const attention = tasks.filter(task => task.attention).length
  const selected = tasks.find(task => task.operationId === expanded)
  const selectedId = selected?.operationId
  useLayoutEffect(() => {
    if (!open) { previousSelection.current = undefined; return }
    // Drill-in replaces DOM inside the same modal, so native dialog restoration cannot handle it.
    if (selectedId !== undefined) backButton.current?.focus()
    else if (previousSelection.current !== undefined) {
      (detailsButtons.current.get(previousSelection.current) ?? index.current)?.focus()
    }
    previousSelection.current = selectedId
  }, [open, selectedId])
  const close = () => { onIntent('close-downloads'); onOpenChange(false); setExpanded(null) }
  return <>
    <button ref={entryButton} className="downloads-entry" type="button" aria-describedby={entryDescription === undefined ? undefined : descriptionId}
      onClick={() => { onIntent('open-downloads'); onOpenChange(true) }}>
      <ReceiverIcon name="download" />{entryLabel}{attention > 0 && <span className="attention-count" aria-label={`${attention} need attention`}>{attention}</span>}
    </button>
    {entryDescription !== undefined && <span id={descriptionId} hidden>{entryDescription}</span>}
    {open && <DetailSheet title={selected?.objectLabel ?? 'Downloads'} onClose={close} returnFocus={entryButton}
      returnLabel="Close downloads" className="downloads-sheet"
      {...(selected === undefined ? { subtitle: 'Tasks and saved records in this browser.' } : {})}
      navigation={selected === undefined ? undefined : <button ref={backButton} className="quiet-action downloads-back" type="button"
        onClick={() => { onIntent('close-download-task-details'); setExpanded(null) }}><ReceiverIcon name="chevron-left" />All downloads</button>}>
      {selected !== undefined ?
        <TaskDetails task={selected} actions={actions} busy={busy}>{details?.(selected.operationId)}</TaskDetails>
        : <div ref={index} tabIndex={-1} className="downloads-index">
          {loading && <p role="status">Loading downloads…</p>}
          {error !== null && <p className="downloads-error" role="alert">{error}</p>}
          {!loading && error === null && tasks.length === 0 && <div className="downloads-empty">
            <p>No downloads yet.</p><p>Downloads you start here will appear in this browser.</p>
          </div>}
          <div className="downloads-list">{tasks.map(task =>
            <TaskCard key={task.operationId} task={task} actions={actions} busy={busy}
              detailsRef={element => {
                if (element === null) detailsButtons.current.delete(task.operationId)
                else detailsButtons.current.set(task.operationId, element)
              }}
              onDetails={() => { onIntent('open-download-task-details'); setExpanded(task.operationId) }} />)}</div>
          {tasks.length > 0 && <details className="storage-details"><summary>About retained downloads</summary>
            <p>Retained data stays in this browser. Clearing this site’s data removes it.
              Save important results promptly; the browser may remove unprotected data when space is low.</p>
            <p>Files already exported are separate from this history. Partial ZIP export keeps the original task available to continue.</p>
          </details>}
        </div>}
    </DetailSheet>}
  </>
}
