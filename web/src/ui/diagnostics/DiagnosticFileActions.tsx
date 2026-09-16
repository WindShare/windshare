import { useState } from 'react'
import type { DiagnosticFile } from '../../diagnostics/browser/file'
import type { DiagnosticsDelivery } from '../../diagnostics/browser/delivery'

export function DiagnosticFileActions({ readFile, delivery }: {
  readonly readFile: () => DiagnosticFile
  readonly delivery: DiagnosticsDelivery
}) {
  const [message, setMessage] = useState<string | null>(null)
  const [manualCopy, setManualCopy] = useState<string | null>(null)
  const [sharing, setSharing] = useState(false)
  const canShare = delivery.supportsFileSharing()
  const read = (): DiagnosticFile | undefined => {
    try { return readFile() } catch {
      setMessage('This diagnostic export is unavailable. Try a previously saved file.')
      return undefined
    }
  }
  const share = async () => {
    const file = read()
    if (file === undefined) return
    setSharing(true)
    try {
      const outcome = await delivery.share(file)
      setMessage(outcome === 'shared' ? 'File handed to your sharing app.' : null)
    } catch {
      setMessage('Sharing is unavailable. Save the file or copy the log instead.')
    } finally {
      setSharing(false)
    }
  }
  const save = () => {
    const file = read()
    if (file === undefined) return
    try {
      delivery.save(file)
      setMessage('Diagnostic download started.')
    } catch {
      setMessage('The download could not start. Copy the log instead.')
    }
  }
  const copy = async () => {
    const file = read()
    if (file === undefined) return
    try {
      await delivery.copy(file)
      setMessage('Diagnostic log copied.')
      setManualCopy(null)
    } catch {
      setManualCopy(file.text)
      setMessage('Select the log below and copy it.')
    }
  }
  return <>
    <div className="diagnostics-actions">
      {canShare && <button type="button" disabled={sharing} onClick={() => { void share() }}>Share file</button>}
      <button type="button" onClick={save}>Save file</button>
      <button type="button" onClick={() => { void copy() }}>Copy log</button>
    </div>
    {message !== null && <p role="status">{message}</p>}
    {manualCopy !== null && <label className="diagnostics-copy">Diagnostic log
      <textarea readOnly value={manualCopy} onFocus={event => event.currentTarget.select()} />
    </label>}
  </>
}
