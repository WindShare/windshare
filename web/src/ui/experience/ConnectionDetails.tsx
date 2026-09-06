import { useState } from 'react'
import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { presentReceiverPathActivity } from '../connection/path-presentation'

export function ConnectionDetails({ status, path }: {
  readonly status: string
  readonly path: ReceiverPathActivitySnapshot
}) {
  const [message, setMessage] = useState<string | null>(null)
  const exportDiagnostics = () => {
    try {
      const text = window.windshareDiagnostics.export()
      const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = 'windshare-diagnostics.json'
      anchor.click()
      URL.revokeObjectURL(url)
      setMessage('Diagnostic download started.')
    } catch {
      setMessage('Diagnostics are unavailable in this session.')
    }
  }
  return <div className="connection-details">
    <p>Your files and filenames are encrypted between you and the sender. Relays carry encrypted data.</p>
    <dl><dt>Share connection</dt><dd>{status}</dd><dt>Content path</dt>
      <dd>{presentReceiverPathActivity(path) ?? 'No content is moving right now.'}</dd></dl>
    <details><summary>Developer diagnostics</summary>
      <p>Export connection and operation evidence to help investigate a problem.</p>
      <button type="button" onClick={exportDiagnostics}>Export diagnostics</button>
      {message !== null && <p role="status">{message}</p>}
    </details>
  </div>
}
