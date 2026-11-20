import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { presentReceiverPathActivity } from '../connection/path-presentation'
import { DownloadChannels } from '../connection/DownloadChannels'
import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export function ConnectionDetails({ status, path }: {
  readonly status: string
  readonly path: ReceiverPathActivitySnapshot
}) {
  return <div className="connection-details">
    <p className="connection-privacy-note">
      <ReceiverIcon name="lock" />
      <span>End-to-end encrypted — files and filenames stay private between you and the sender.</span>
    </p>
    <dl className="detail-inspector">
      <div className="detail-fact-row"><dt>Share connection</dt><dd>{status}</dd></div>
      <div className="detail-fact-row"><dt>Content path</dt><dd>{presentReceiverPathActivity(path) ?? 'No data received recently.'}</dd></div>
    </dl>
    <DownloadChannels path={path} />
    <DiagnosticsEntry />
  </div>
}
