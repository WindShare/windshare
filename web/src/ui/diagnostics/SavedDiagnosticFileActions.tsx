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
  const [state, setState] = useState<SavedFileState>({ kind: 'loading' })
  useEffect(() => {
    let active = true
    readFile(captureId).then(file => {
      if (active) setState(file === null ? { kind: 'unavailable' } : { kind: 'ready', file })
    }).catch(() => { if (active) setState({ kind: 'unavailable' }) })
    return () => { active = false }
  }, [captureId, readFile])

  // Prepare only when the panel opens; the eventual share click can hand off
  // synchronously without making a user's gesture wait for browser storage.
  if (state.kind === 'loading') return <p role="status">Loading saved diagnostics…</p>
  if (state.kind === 'unavailable') return <p role="status">This saved diagnostic file is no longer available.</p>
  return <DiagnosticFileActions readFile={() => state.file} delivery={delivery} />
}
