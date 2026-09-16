import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { presentReceiverPathActivity } from '../connection/path-presentation'
import { DownloadChannels } from '../connection/DownloadChannels'
import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'

export function ConnectionDetails({ status, path }: {
  readonly status: string
  readonly path: ReceiverPathActivitySnapshot
}) {
  return <div className="connection-details">
    <p>Your files and filenames are encrypted between you and the sender. Relays carry encrypted data.</p>
    <dl><dt>Share connection</dt><dd>{status}</dd><dt>Content path</dt>
      <dd>{presentReceiverPathActivity(path) ?? 'No data received recently.'}</dd></dl>
    <DownloadChannels path={path} />
    <DiagnosticsEntry />
  </div>
}
