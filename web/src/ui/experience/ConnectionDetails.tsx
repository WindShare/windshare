import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { presentReceiverPathActivity } from '../connection/path-presentation'
import { DownloadChannels } from '../connection/DownloadChannels'
import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export function ConnectionDetails({ status, path }: {
  readonly status: string
  readonly path: ReceiverPathActivitySnapshot
}) {
  const t = useCopy(receiverCopy).connection
  return <div className="connection-details">
    <p className="connection-privacy-note">
      <ReceiverIcon name="lock" />
      <span>{t.privacy}</span>
    </p>
    <dl className="detail-inspector">
      <div className="detail-fact-row"><dt>{t.shareConnection}</dt><dd>{status}</dd></div>
      <div className="detail-fact-row"><dt>{t.contentPath}</dt><dd>{presentReceiverPathActivity(path, t) ?? t.noRecentData}</dd></div>
    </dl>
    <DownloadChannels path={path} />
    <DiagnosticsEntry />
  </div>
}
