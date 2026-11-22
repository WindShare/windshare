import { diagnosticsCopy } from '../locales/diagnostics'
import { useCopy } from '../preferences/use-preferences'
import { useState } from 'react'
import type { DiagnosticsDelivery } from '../../diagnostics/delivery'
import type { DiagnosticFile } from '../../diagnostics/file'

type DiagnosticFileMessage = 'exportUnavailable' | 'shared' | 'shareFailed' | 'downloadStarted' |
  'downloadFailed' | 'copied' | 'manualCopy'

export function DiagnosticFileActions({ readFile, delivery }: {
  readonly readFile: () => DiagnosticFile
  readonly delivery: DiagnosticsDelivery
}) {
  const t = useCopy(diagnosticsCopy)
  const [message, setMessage] = useState<DiagnosticFileMessage | null>(null)
  const [manualCopy, setManualCopy] = useState<string | null>(null)
  const [sharing, setSharing] = useState(false)
  const canShare = delivery.supportsFileSharing()
  const read = (): DiagnosticFile | undefined => {
    try { return readFile() } catch {
      setMessage('exportUnavailable')
      return undefined
    }
  }
  const share = async () => {
    const file = read()
    if (file === undefined) return
    setSharing(true)
    try {
      const outcome = await delivery.share(file)
      setMessage(outcome === 'shared' ? 'shared' : null)
    } catch {
      setMessage('shareFailed')
    } finally {
      setSharing(false)
    }
  }
  const save = () => {
    const file = read()
    if (file === undefined) return
    try {
      delivery.save(file)
      setMessage('downloadStarted')
    } catch {
      setMessage('downloadFailed')
    }
  }
  const copy = async () => {
    const file = read()
    if (file === undefined) return
    try {
      await delivery.copy(file)
      setMessage('copied')
      setManualCopy(null)
    } catch {
      setManualCopy(file.text)
      setMessage('manualCopy')
    }
  }
  return <>
    <div className="diagnostics-actions">
      {canShare && <button type="button" disabled={sharing} onClick={() => { void share() }}>{t.shareFile}</button>}
      <button type="button" onClick={save}>{t.saveFile}</button>
      <button type="button" onClick={() => { void copy() }}>{t.copyLog}</button>
    </div>
    {message !== null && <p role="status">{t[message]}</p>}
    {manualCopy !== null && <label className="diagnostics-copy">{t.log}
      <textarea readOnly value={manualCopy} onFocus={event => event.currentTarget.select()} />
    </label>}
  </>
}
