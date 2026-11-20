import { diagnosticsCopy } from '../locales/diagnostics'
import { usePreferences } from '../preferences/use-preferences'
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
  const { language } = usePreferences()
  const t = diagnosticsCopy[language]
  const [expanded, setExpanded] = useState(false)
  const [latest, ...older] = captures
  if (latest === undefined) return null

  const renderFile = (capture: DiagnosticCaptureSummary) => <div className="diagnostics-saved-file" key={capture.id}>
    <h4>{capture.captureState === 'idle' ? t.failure : t.recorded}</h4>
    <p>{t.savedLocally} <time dateTime={new Date(capture.savedAt).toISOString()}>
      {new Date(capture.savedAt).toLocaleString(language)}
    </time></p>
    <SavedDiagnosticFileActions captureId={capture.id} readFile={readFile} delivery={delivery} />
  </div>

  return <section aria-label={t.previous}>
    <h3>{t.previous}</h3>
    {renderFile(latest)}
    {older.length > 0 && <details className="diagnostics-history" open={expanded} onToggle={event => setExpanded(event.currentTarget.open)}>
      <summary>{t.older(older.length)}</summary>
      {/* Collapsed history must not read or retain archived log bodies. */}
      {expanded && older.map(renderFile)}
    </details>}
  </section>
}
