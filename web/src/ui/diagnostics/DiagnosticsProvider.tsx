import { useRef, useState, useSyncExternalStore, type ReactNode } from 'react'
import type { BrowserDiagnosticsSession, BrowserDiagnosticsSnapshot } from '../../diagnostics/browser/session'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'
import { DetailSheet } from '../controls/DetailSheet'
import { DiagnosticsPanelContext } from './context'
import { DiagnosticFileActions } from './DiagnosticFileActions'
import { SavedDiagnosticFileActions } from './SavedDiagnosticFileActions'
import { diagnosticsCaptureLabel } from './presentation'
import './diagnostics.css'

export function DiagnosticsProvider({ session, delivery, children }: {
  readonly session: BrowserDiagnosticsSession
  readonly delivery: DiagnosticsDelivery
  readonly children: ReactNode
}) {
  const snapshot = useSyncExternalStore(session.subscribe, session.getSnapshot, session.getSnapshot)
  const [open, setOpen] = useState(false)
  const invoker = useRef<HTMLElement | null>(null)
  const openPanel = (element: HTMLElement) => {
    invoker.current = element
    // Failure incidents can arrive while detailed trace capture is off.
    session.refresh()
    setOpen(true)
  }
  const { capture, activation, previous, hasCurrentEvidence } = snapshot
  const label = diagnosticsCaptureLabel(capture)

  return <DiagnosticsPanelContext value={openPanel}>
    <DiagnosticsNotice snapshot={snapshot} onOpen={openPanel} onStop={() => session.disable()} />
    {children}
    {open && <div className="diagnostics-surface">
      <DetailSheet title="Diagnostics" onClose={() => setOpen(false)} returnFocus={invoker}
        returnLabel="Close diagnostics" className="diagnostics-sheet">
        <p role="status">{label}</p>
        <p>Record a problem and send the diagnostic file to the person helping you. Recording is local to this tab.</p>
        <div className="diagnostics-actions">
          {activation.kind === 'active' && <button type="button" onClick={() => session.disable()}>Stop recording</button>}
          {!capture.enabled && <button type="button" onClick={() => session.enable()}>Start recording</button>}
        </div>
        {activation.kind === 'active' && <p>{capture.enabled ? 'Recording ends at ' : 'Reload can resume recording until '}
          {new Date(activation.expiresAtMilliseconds).toLocaleTimeString()}.</p>}
        {snapshot.archiveUnavailable && <p role="status">Local backup is unavailable. Export diagnostics before leaving this page.</p>}
        {hasCurrentEvidence ? <section aria-label="Current diagnostics">
          <h3>Current diagnostics</h3>
          {snapshot.savedAt !== null && <p>Last saved locally: {new Date(snapshot.savedAt).toLocaleString()}</p>}
          <DiagnosticFileActions key={capture.capture_generation} readFile={() => session.exportFile()} delivery={delivery} />
        </section> : <p>No diagnostics have been recorded on this page yet.</p>}
        {previous !== null && <section aria-label="Previous diagnostics">
          <h3>Previous diagnostics</h3>
          <p>Saved locally: {new Date(previous.savedAt).toLocaleString()}</p>
          <SavedDiagnosticFileActions key={previous.id} captureId={previous.id} readFile={session.readSavedFile} delivery={delivery} />
        </section>}
        <p>Recent diagnostic files are kept locally for up to 24 hours, within a fixed storage limit.</p>
      </DetailSheet>
    </div>}
  </DiagnosticsPanelContext>
}

function DiagnosticsNotice({ snapshot, onOpen, onStop }: {
  readonly snapshot: BrowserDiagnosticsSnapshot
  readonly onOpen: (element: HTMLElement) => void
  readonly onStop: () => void
}) {
  const [dismissed, setDismissed] = useState<string | null>(null)
  const { capture, activation, previous, hasCurrentEvidence } = snapshot
  const notice = hasCurrentEvidence ? `capture:${capture.capture_generation}` : previous?.id ?? ''
  const active = activation.kind === 'active'
  // Hiding retained evidence never hides an active recording or revokes its data.
  if (!active && (!hasCurrentEvidence && previous === null || dismissed === notice)) return null
  return <aside className="diagnostics-surface diagnostics-bar" aria-label="Diagnostics">
    <span role="status">{!active && !hasCurrentEvidence && previous !== null
      ? 'Saved diagnostics available' : diagnosticsCaptureLabel(capture)}</span>
    <button type="button" onClick={event => onOpen(event.currentTarget)}>Export diagnostics</button>
    {active
      ? <button type="button" onClick={onStop}>Stop recording</button>
      : <button type="button" onClick={() => setDismissed(notice)}>Hide notification</button>}
  </aside>
}
