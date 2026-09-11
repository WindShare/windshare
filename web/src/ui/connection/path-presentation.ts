import { RECENT_CONTENT_WINDOW_MILLISECONDS, type ReceiverPathActivitySnapshot } from '../../receiver/path-activity'

export function presentReceiverPathActivity({ lanes }: ReceiverPathActivitySnapshot): string | null {
  const directConnected = lanes.some(lane => lane.route === 'direct')
  const direct = lanes.some(lane => lane.route === 'direct' && lane.recentContent)
  const relay = lanes.some(lane => lane.route !== 'direct' && lane.recentContent)
  const window = `in the last ${RECENT_CONTENT_WINDOW_MILLISECONDS / 1_000} seconds`
  if (direct && relay) return `Received directly and through relay ${window}`
  if (direct) return `Received directly ${window}`
  if (relay) return `Received through relay ${window}`
  return directConnected ? 'Direct connected' : null
}

export function connectedChannelCount({ lanes }: ReceiverPathActivitySnapshot): string {
  return `${lanes.length} ${lanes.length === 1 ? 'channel' : 'channels'} connected`
}
