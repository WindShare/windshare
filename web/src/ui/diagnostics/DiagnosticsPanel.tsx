import { diagnosticsCopy } from '../locales/diagnostics'
import { usePreferences } from '../preferences/use-preferences'
import type { BrowserDiagnosticsSession, BrowserDiagnosticsSnapshot } from '../../diagnostics/browser/session'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'
import { DiagnosticFileActions } from './DiagnosticFileActions'
import { PreviousDiagnostics } from './PreviousDiagnostics'
import { diagnosticsCaptureLabel } from './presentation'

export function DiagnosticsPanel({ snapshot, session, delivery }: {
  readonly snapshot: BrowserDiagnosticsSnapshot
  readonly session: BrowserDiagnosticsSession
  readonly delivery: DiagnosticsDelivery
}) {
  const { language } = usePreferences()
  const t = diagnosticsCopy[language]
  const { capture, activation, previousCaptures, hasCurrentEvidence } = snapshot
  const label = diagnosticsCaptureLabel(capture, t)
  return <>
        <p role="status">{label}</p>
        <p>{t.instructions}</p>
        <div className="diagnostics-actions">
          {activation.kind === 'active' && <button type="button" onClick={() => session.disable()}>{t.stop}</button>}
          {!capture.enabled && <button type="button" onClick={() => session.enable()}>{t.start}</button>}
        </div>
        {activation.kind === 'active' && <p>{t.ends(new Date(activation.expiresAtMilliseconds).toLocaleTimeString(language))}</p>}
        {snapshot.archiveUnavailable && <p role="status">{t.backupUnavailable}</p>}
        {hasCurrentEvidence ? <section aria-label={t.current}>
          <h3>{t.current}</h3>
          {snapshot.savedAt !== null && <p>{t.lastSaved(new Date(snapshot.savedAt).toLocaleString(language))}</p>}
          <DiagnosticFileActions key={capture.capture_generation} readFile={() => session.exportFile()} delivery={delivery} />
        </section> : <p>{t.none}</p>}
        <PreviousDiagnostics captures={previousCaptures} readFile={session.readSavedFile} delivery={delivery} />
        <p>{t.retention}</p>
  </>
}
