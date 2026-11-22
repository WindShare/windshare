import { diagnosticsCopy } from '../locales/diagnostics'
import { usePreferences } from '../preferences/use-preferences'
import type { DiagnosticsDelivery } from '../../diagnostics/delivery'
import type { DiagnosticsRuntime, DiagnosticsSnapshot } from '../../diagnostics/runtime'
import { DiagnosticFileActions } from './DiagnosticFileActions'
import { diagnosticsCaptureLabel } from './presentation'

export function DiagnosticsPanel({ snapshot, runtime, delivery }: {
  readonly snapshot: DiagnosticsSnapshot
  readonly runtime: DiagnosticsRuntime
  readonly delivery: DiagnosticsDelivery
}) {
  const { language } = usePreferences()
  const t = diagnosticsCopy[language]
  const { status, activation, hasEvidence } = snapshot
  const label = diagnosticsCaptureLabel(status, t)
  return <>
    <p role="status">{label}</p>
    <p>{t.instructions}</p>
    <div className="diagnostics-actions">
      {activation.kind === 'active' && <button type="button" onClick={() => runtime.disable()}>{t.stop}</button>}
      {!status.enabled && <button type="button" onClick={() => runtime.enable()}>{t.start}</button>}
    </div>
    {activation.kind === 'active' && <p>{t.ends(new Date(activation.expiresAtMilliseconds).toLocaleTimeString(language))}</p>}
    {hasEvidence ? <section aria-label={t.current}>
      <h3>{t.current}</h3>
      <DiagnosticFileActions key={status.capture_generation} readFile={() => runtime.exportFile()} delivery={delivery} />
    </section> : <p>{t.none}</p>}
  </>
}
