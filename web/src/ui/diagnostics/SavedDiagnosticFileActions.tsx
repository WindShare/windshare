import { diagnosticsCopy } from '../locales/diagnostics'
import { useCopy } from '../preferences/use-preferences'
import { useEffect, useState } from 'react'
import type { DiagnosticFile } from '../../diagnostics/browser/file'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'
import { DiagnosticFileActions } from './DiagnosticFileActions'

type SavedFileState =
  | Readonly<{ kind: 'loading' }>
  | Readonly<{ kind: 'unavailable' }>
  | Readonly<{ kind: 'ready'; file: DiagnosticFile }>

export function SavedDiagnosticFileActions({ captureId, readFile, delivery }: {
  readonly captureId: string
  readonly readFile: (id: string) => Promise<DiagnosticFile | null>
  readonly delivery: DiagnosticsDelivery
}) {
  const t = useCopy(diagnosticsCopy)
  const [state, setState] = useState<SavedFileState>({ kind: 'loading' })
  useEffect(() => {
    let active = true
    readFile(captureId).then(file => {
      if (active) setState(file === null ? { kind: 'unavailable' } : { kind: 'ready', file })
    }).catch(() => { if (active) setState({ kind: 'unavailable' }) })
    return () => { active = false }
  }, [captureId, readFile])

  // Prepare when a saved file is revealed; the eventual share click can hand off
  // synchronously without making a user's gesture wait for browser storage.
  if (state.kind === 'loading') return <p role="status">{t.loading}</p>
  if (state.kind === 'unavailable') return <p role="status">{t.unavailable}</p>
  return <DiagnosticFileActions readFile={() => state.file} delivery={delivery} />
}
