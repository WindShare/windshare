import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { connectedChannelCount } from './path-presentation'

const ROUTE_LABEL = {
  direct: 'direct',
  'application-relay': 'applicationRelay',
} as const

export function DownloadChannels({ path }: { readonly path: ReceiverPathActivitySnapshot }) {
  const t = useCopy(receiverCopy).connection
  return <section className="download-channels" aria-labelledby="download-channels-heading">
    <div className="download-channels-head">
      <h3 id="download-channels-heading">{t.channels}</h3>
      <span className="download-channels-count" role="status">{connectedChannelCount(path, t)}</span>
    </div>
    {path.lanes.length === 0
      ? <p className="download-channels-empty">{t.emptyChannels}</p>
      : <ul className="download-channels-list" aria-label={t.connectedChannels}>
        {path.lanes.map(lane => <li key={`${lane.laneId}:${lane.laneEpoch}`}>
          <div className="download-channel-identity">
            <strong>{t.channel(lane.laneId)}</strong>
            <span>{t[ROUTE_LABEL[lane.route]]}</span>
          </div>
          <div className="download-channel-state">
            <span className="download-channel-connected">{t.connected}</span>
            <span className={lane.recentContent ? 'download-channel-recent' : undefined}>
              {lane.recentContent ? t.receivedRecently : t.noRecent}
            </span>
          </div>
        </li>)}
      </ul>}
  </section>
}
