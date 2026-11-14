import { useRef, useState, useSyncExternalStore, type ReactNode, type ComponentType } from 'react'
import type { BrowserDiagnosticsSession, BrowserDiagnosticsSnapshot } from '../../diagnostics/browser/session'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'
import { DetailSheet } from '../controls/DetailSheet'
import { DiagnosticsPanelContext } from './context'
import { diagnosticsCaptureLabel } from './presentation'
import './diagnostics.css'

export function DiagnosticsProvider({ session, delivery, children }: {
  readonly session: BrowserDiagnosticsSession
  readonly delivery: DiagnosticsDelivery
  readonly children: ReactNode
}) {
  const snapshot = useSyncExternalStore(session.subscribe, session.getSnapshot, session.getSnapshot)
  const [open, setOpen] = useState(false)
  const [panel, setPanel] = useState<ComponentType<{ snapshot: BrowserDiagnosticsSnapshot; session: BrowserDiagnosticsSession; delivery: DiagnosticsDelivery }> | null>(null)
  const [loadFailed, setLoadFailed] = useState(false)
  const preparePanel = () => {
    setLoadFailed(false)
    void import('./DiagnosticsPanel').then(module => setPanel(() => module.DiagnosticsPanel), () => setLoadFailed(true))
  }
  const invoker = useRef<HTMLElement | null>(null)
  const openPanel = (element: HTMLElement) => {
    invoker.current = element
    // Failure incidents can arrive while detailed trace capture is off.
    session.refresh()
    setOpen(true)
    if (panel === null) preparePanel()
  }
  const Panel = panel
  const pendingPanel = loadFailed
    ? <p role="alert">Unable to open diagnostics. <button type="button" onClick={preparePanel}>Retry</button></p>
    : <p role="status">Opening diagnostics&hellip;</p>

  return <DiagnosticsPanelContext value={openPanel}>
    <DiagnosticsNotice snapshot={snapshot} onOpen={openPanel} onStop={() => session.disable()} />
    {children}
    {open && <div className="diagnostics-surface">
      <DetailSheet title="Diagnostics" onClose={() => setOpen(false)} returnFocus={invoker}
        returnLabel="Close diagnostics" className="diagnostics-sheet">
        {Panel !== null ? <Panel snapshot={snapshot} session={session} delivery={delivery} />
          : pendingPanel}
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
  const { capture, activation, previousCaptures, hasCurrentEvidence } = snapshot
  const currentRecording = capture.state !== 'idle' && hasCurrentEvidence
  // Silent snapshots must not replace the recording that owns this notification or its dismissal.
  const savedRecording = previousCaptures.find(previous => previous.captureState !== 'idle')
  const notice = currentRecording ? `capture:${capture.capture_generation}` : savedRecording?.id ?? ''
  const active = activation.kind === 'active'
  // Automatically recovered failures remain available in the panel without announcing a recording.
  if (!active && ((!currentRecording && savedRecording === undefined) || dismissed === notice)) return null
  return <aside className="diagnostics-surface diagnostics-bar" aria-label="Diagnostics">
    <span role="status">{!active && !currentRecording && savedRecording !== undefined
      ? 'Saved diagnostics available' : diagnosticsCaptureLabel(capture)}</span>
    <button type="button" onClick={event => onOpen(event.currentTarget)}>Export diagnostics</button>
    {active
      ? <button type="button" onClick={onStop}>Stop recording</button>
      : <button type="button" onClick={() => setDismissed(notice)}>Hide notification</button>}
  </aside>
}
