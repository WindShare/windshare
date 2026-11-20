import { diagnosticsCopy } from '../locales/diagnostics'
import { useCopy } from '../preferences/use-preferences'
import { useContext } from 'react'
import { DiagnosticsPanelContext } from './context'

export function DiagnosticsEntry({ label }: { readonly label?: string }) {
  const t = useCopy(diagnosticsCopy)
  const open = useContext(DiagnosticsPanelContext)
  return open === null ? null : <button type="button" className="diagnostics-entry"
    onClick={event => open(event.currentTarget)}>{label ?? t.title}</button>
}
