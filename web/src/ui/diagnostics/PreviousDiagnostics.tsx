import { useState } from 'react'
import type { DiagnosticCaptureSummary } from '../../diagnostics/browser/archive'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'
import type { DiagnosticFile } from '../../diagnostics/browser/file'
import { SavedDiagnosticFileActions } from './SavedDiagnosticFileActions'

export function PreviousDiagnostics({ captures, readFile, delivery }: {
  readonly captures: readonly DiagnosticCaptureSummary[]
  readonly readFile: (id: string) => Promise<DiagnosticFile | null>
  readonly delivery: DiagnosticsDelivery
}) {
  const [expanded, setExpanded] = useState(false)
  const [latest, ...older] = captures
  if (latest === undefined) return null

  const renderFile = (capture: DiagnosticCaptureSummary) => <div className="diagnostics-saved-file" key={capture.id}>
    <h4>{capture.captureState === 'idle' ? 'Failure diagnostics' : 'Recorded diagnostics'}</h4>
    <p>Saved locally: <time dateTime={new Date(capture.savedAt).toISOString()}>
      {new Date(capture.savedAt).toLocaleString()}
    </time></p>
    <SavedDiagnosticFileActions captureId={capture.id} readFile={readFile} delivery={delivery} />
  </div>

  return <section aria-label="Previous diagnostics">
    <h3>Previous diagnostics</h3>
    {renderFile(latest)}
    {older.length > 0 && <details className="diagnostics-history" open={expanded} onToggle={event => setExpanded(event.currentTarget.open)}>
      <summary>Older diagnostic files ({older.length})</summary>
      {/* Collapsed history must not read or retain archived log bodies. */}
      {expanded && older.map(renderFile)}
    </details>}
  </section>
}
