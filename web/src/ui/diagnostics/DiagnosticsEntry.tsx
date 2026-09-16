import { useContext } from 'react'
import { DiagnosticsPanelContext } from './context'

export function DiagnosticsEntry({ label = 'Diagnostics' }: { readonly label?: string }) {
  const open = useContext(DiagnosticsPanelContext)
  return open === null ? null : <button type="button" className="diagnostics-entry"
    onClick={event => open(event.currentTarget)}>{label}</button>
}
