import { diagnosticsCopy } from '../locales/diagnostics'
import { useCopy } from '../preferences/use-preferences'
import { useRef, useState, useSyncExternalStore, type ReactNode, type ComponentType } from 'react'
import type { DiagnosticsDelivery } from '../../diagnostics/delivery'
import type { DiagnosticsRuntime, DiagnosticsSnapshot } from '../../diagnostics/runtime'
import { DetailSheet } from '../controls/DetailSheet'
import { DiagnosticsPanelContext } from './context'
import { diagnosticsCaptureLabel } from './presentation'
import './diagnostics.css'

export function DiagnosticsProvider({ runtime, delivery, children }: {
  readonly runtime: DiagnosticsRuntime
  readonly delivery: DiagnosticsDelivery
  readonly children: ReactNode
}) {
  const t = useCopy(diagnosticsCopy)
  const snapshot = useSyncExternalStore(runtime.subscribe, runtime.getSnapshot, runtime.getSnapshot)
  const [open, setOpen] = useState(false)
  const [panel, setPanel] = useState<ComponentType<{
    snapshot: DiagnosticsSnapshot
    runtime: DiagnosticsRuntime
    delivery: DiagnosticsDelivery
  }> | null>(null)
  const [loadFailed, setLoadFailed] = useState(false)
  const preparePanel = () => {
    setLoadFailed(false)
    void import('./DiagnosticsPanel').then(module => setPanel(() => module.DiagnosticsPanel), () => setLoadFailed(true))
  }
  const invoker = useRef<HTMLElement | null>(null)
  const openPanel = (element: HTMLElement) => {
    invoker.current = element
    runtime.refresh()
    setOpen(true)
    if (panel === null) preparePanel()
  }
  const Panel = panel
  const pendingPanel = loadFailed
    ? <p role="alert">{t.openFailure} <button type="button" onClick={preparePanel}>{t.retry}</button></p>
    : <p role="status">{t.opening}</p>

  return <DiagnosticsPanelContext value={openPanel}>
    <DiagnosticsNotice snapshot={snapshot} onOpen={openPanel} onStop={() => runtime.disable()} />
    {children}
    {open && <div className="diagnostics-surface">
      <DetailSheet title={t.title} onClose={() => setOpen(false)} returnFocus={invoker}
        returnLabel={t.close} className="diagnostics-sheet">
        {Panel !== null ? <Panel snapshot={snapshot} runtime={runtime} delivery={delivery} /> : pendingPanel}
      </DetailSheet>
    </div>}
  </DiagnosticsPanelContext>
}

function DiagnosticsNotice({ snapshot, onOpen, onStop }: {
  readonly snapshot: DiagnosticsSnapshot
  readonly onOpen: (element: HTMLElement) => void
  readonly onStop: () => void
}) {
  const t = useCopy(diagnosticsCopy)
  const [dismissed, setDismissed] = useState<string | null>(null)
  const { status, activation, hasEvidence } = snapshot
  const currentRecording = status.state !== 'idle' && hasEvidence
  const notice = currentRecording ? `capture:${status.capture_generation}` : ''
  const active = activation.kind === 'active'
  if (!active && (!currentRecording || dismissed === notice)) return null
  return <aside className="diagnostics-surface diagnostics-bar" aria-label={t.title}>
    <span role="status">{diagnosticsCaptureLabel(status, t)}</span>
    <button type="button" onClick={event => onOpen(event.currentTarget)}>{t.export}</button>
    {active
      ? <button type="button" onClick={onStop}>{t.stop}</button>
      : <button type="button" onClick={() => setDismissed(notice)}>{t.hide}</button>}
  </aside>
}
