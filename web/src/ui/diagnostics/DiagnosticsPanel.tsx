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
  const { capture, activation, previousCaptures, hasCurrentEvidence } = snapshot
  const label = diagnosticsCaptureLabel(capture)
  return <>
        <p role="status">{label}</p>
        <p>Record a problem and send the diagnostic file to the person helping you. Recording is local to this tab.</p>
        <div className="diagnostics-actions">
          {activation.kind === 'active' && <button type="button" onClick={() => session.disable()}>Stop recording</button>}
          {!capture.enabled && <button type="button" onClick={() => session.enable()}>Start recording</button>}
        </div>
        {activation.kind === 'active' && <p>Recording ends at{' '}
          {new Date(activation.expiresAtMilliseconds).toLocaleTimeString()}.</p>}
        {snapshot.archiveUnavailable && <p role="status">Local backup is unavailable. Export diagnostics before leaving this page.</p>}
        {hasCurrentEvidence ? <section aria-label="Current diagnostics">
          <h3>Current diagnostics</h3>
          {snapshot.savedAt !== null && <p>Last saved locally: {new Date(snapshot.savedAt).toLocaleString()}</p>}
          <DiagnosticFileActions key={capture.capture_generation} readFile={() => session.exportFile()} delivery={delivery} />
        </section> : <p>No diagnostics have been recorded on this page yet.</p>}
        <PreviousDiagnostics captures={previousCaptures} readFile={session.readSavedFile} delivery={delivery} />
        <p>Recent diagnostic files are kept locally for up to 24 hours, within a fixed storage limit.</p>
  </>
}
