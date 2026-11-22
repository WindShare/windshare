import type { RetainedInventorySnapshot } from '../../application/receive/contracts'
import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import type { TaskPresentation } from '../tasks'
import { TaskCard, TaskDetails, type TaskViewActions } from '../tasks/TaskView'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export function Downloads({ tasks, actions, inventory, busy, details, entryLabel, entryDescription, onIntent, onRetry, open, onOpenChange }: {
  readonly tasks: readonly TaskPresentation[]
  readonly actions: TaskViewActions
  readonly inventory: RetainedInventorySnapshot['kind']
  readonly busy: boolean
  readonly details?: (operationId: string) => ReactNode
  readonly entryLabel?: string
  readonly entryDescription?: string
  readonly onIntent: (action: string) => void
  readonly onRetry?: () => void
  readonly open: boolean
  readonly onOpenChange: (open: boolean) => void
}) {
  const { downloads: t, failures } = useCopy(receiverCopy)
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
      <ReceiverIcon name="download" />{entryLabel ?? t.downloads}{attention > 0 && <span className="attention-count" aria-label={t.attention(attention)}>{attention}</span>}
    </button>
    {entryDescription !== undefined && <span id={descriptionId} hidden>{entryDescription}</span>}
    {open && <DetailSheet title={selected?.objectLabel ?? t.downloads} onClose={close} returnFocus={entryButton}
      returnLabel={t.closeDownloads} className="downloads-sheet"
      navigation={selected === undefined ? undefined : <button ref={backButton} className="quiet-action downloads-back" type="button"
        onClick={() => { onIntent('close-download-task-details'); setExpanded(null) }}><ReceiverIcon name="chevron-left" />{t.allDownloads}</button>}>
      {selected !== undefined ?
        <TaskDetails task={selected} actions={actions} busy={busy}>{details?.(selected.operationId)}</TaskDetails>
        : <div ref={index} tabIndex={-1} className="downloads-index">
          {(inventory === 'unloaded' || inventory === 'loading') && <p role="status">{t.loading}</p>}
          {inventory === 'failed' && <div className="downloads-error" role="alert"><p>{failures.history}</p>
            {onRetry !== undefined && <button type="button" onClick={onRetry}>{t.retry}</button>}</div>}
          {(inventory === 'empty' || inventory === 'available') && tasks.length === 0 && <div className="downloads-empty">
            <p>{t.empty}</p><p>{t.emptyHint}</p>
          </div>}
          <div className="downloads-list">{tasks.map(task =>
            <TaskCard key={task.operationId} task={task} actions={actions} busy={busy} showTimestamp
              detailsRef={element => {
                if (element === null) detailsButtons.current.delete(task.operationId)
                else detailsButtons.current.set(task.operationId, element)
              }}
              onDetails={() => { onIntent('open-download-task-details'); setExpanded(task.operationId) }} />)}</div>
          {tasks.length > 0 && <details className="storage-details"><summary>{t.storage}</summary>
            <p>{t.storageHint}</p>
          </details>}
        </div>}
    </DetailSheet>}
  </>
}
