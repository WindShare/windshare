import { RECENT_CONTENT_WINDOW_MILLISECONDS, type ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import { connectedChannelCount } from './path-presentation'

const ROUTE_LABEL = {
  direct: 'Direct (P2P)',
  turn: 'TURN relay',
  'application-relay': 'Application relay',
} as const

export function DownloadChannels({ path }: { readonly path: ReceiverPathActivitySnapshot }) {
  return <section className="download-channels" aria-labelledby="download-channels-heading">
    <h3 id="download-channels-heading">Download channels</h3>
    <p role="status">{connectedChannelCount(path)}</p>
    {path.lanes.length === 0
      ? <p>Channels appear when available for downloads or previews.</p>
      : <>
        <ul aria-label="Connected download channels">
          {path.lanes.map(lane => <li key={`${lane.laneId}:${lane.laneEpoch}`}>
            <div className="download-channel-identity">
              <strong>Channel {lane.laneId}</strong>
              <span>{ROUTE_LABEL[lane.route]}</span>
              {lane.turnService !== undefined && <span>{lane.turnService.provider} · {lane.turnService.profile}</span>}
            </div>
            <div className="download-channel-state">
              <span className="download-channel-connected">Connected</span>
              <span className={lane.recentContent ? 'download-channel-recent' : undefined}>
                {lane.recentContent ? 'Received recently' : 'No recent data'}
              </span>
            </div>
          </li>)}
        </ul>
        <p className="download-channel-help">Activity reflects data received in the last {RECENT_CONTENT_WINDOW_MILLISECONDS / 1_000} seconds.
          Connected channels may be idle. Disconnected channels leave this list.</p>
      </>}
    {path.turnServices !== undefined && <ul aria-label="TURN services">
      {path.turnServices.map(service => <li key={service.profile}>
        <strong>{service.profile}</strong> · {service.provider ?? 'TURN'} · {service.state}
        {service.usefulBytes !== undefined && <span> · {(service.usefulBytes / (1024 * 1024)).toFixed(2)} MiB received</span>}
        {service.reason !== undefined && <span> · {service.reason}</span>}
      </li>)}
    </ul>}
  </section>
}
