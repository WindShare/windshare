import { useState, type ReactNode } from 'react'
import type { TaskPresentation } from '../tasks'
import { TaskCard, TaskDetails, type TaskViewActions } from '../tasks/TaskView'
import { DetailSheet } from '../controls/DetailSheet'

export function Downloads({ tasks, actions, loading, error, busy, details, home = 'share', onIntent, open, onOpenChange }: {
  readonly tasks: readonly TaskPresentation[]
  readonly actions: TaskViewActions
  readonly loading: boolean
  readonly error: string | null
  readonly busy: boolean
  readonly details?: (operationId: string) => ReactNode
  readonly home?: 'share' | 'home'
  readonly onIntent: (action: string) => void
  readonly open: boolean
  readonly onOpenChange: (open: boolean) => void
}) {
  const [expanded, setExpanded] = useState<string | null>(null)
  const attention = tasks.filter(task => task.attention).length
  const selected = tasks.find(task => task.operationId === expanded)
  const close = () => { onIntent('close-downloads'); onOpenChange(false); setExpanded(null) }
  return <>
    <button className="downloads-entry" type="button" onClick={() => { onIntent('open-downloads'); onOpenChange(true) }}>
      Downloads{attention > 0 && <span className="attention-count" aria-label={`${attention} need attention`}>{attention}</span>}
    </button>
    {open && <DetailSheet title={selected?.objectLabel ?? 'Downloads'} onClose={close}
      returnLabel={`Back to ${home}`} className="downloads-sheet">
      {selected !== undefined ? <>
        <button className="quiet-action" type="button" onClick={() => setExpanded(null)}>All downloads</button>
        <TaskDetails task={selected} actions={actions} busy={busy}>{details?.(selected.operationId)}</TaskDetails>
      </> : <>
        {loading && <p role="status">Loading downloads…</p>}
        {error !== null && <p role="alert">{error}</p>}
        {!loading && tasks.length === 0 && <p>No downloads yet. Saved and unfinished tasks will appear here.</p>}
        <div className="downloads-list">{tasks.map(task =>
          <TaskCard key={task.operationId} task={task} actions={actions} busy={busy}
            onDetails={() => { onIntent('open-download-task-details'); setExpanded(task.operationId) }} />)}</div>
        <details className="storage-details"><summary>About retained downloads</summary>
          <p>Retained data stays in this browser. Clearing this site’s data removes it.
            Save important results promptly; the browser may remove unprotected data when space is low.</p>
          <p>Files already exported are separate from this history. Partial ZIP export keeps the original task available to continue.</p>
        </details>
      </>}
    </DetailSheet>}
  </>
}
