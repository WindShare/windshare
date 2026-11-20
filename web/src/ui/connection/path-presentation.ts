import { receiverCopy } from '../locales/receiver'
import { RECENT_CONTENT_WINDOW_MILLISECONDS, type ReceiverPathActivitySnapshot } from '../../receiver/path-activity'

export function presentReceiverPathActivity({ lanes }: ReceiverPathActivitySnapshot, t = receiverCopy.en.connection): string | null {
  const directConnected = lanes.some(lane => lane.route === 'direct')
  const direct = lanes.some(lane => lane.route === 'direct' && lane.recentContent)
  const relay = lanes.some(lane => lane.route !== 'direct' && lane.recentContent)
  const seconds = RECENT_CONTENT_WINDOW_MILLISECONDS / 1_000
  if (direct && relay) return t.both(seconds)
  if (direct) return t.directActivity(seconds)
  if (relay) return t.relayActivity(seconds)
  return directConnected ? t.directConnected : null
}

export function connectedChannelCount({ lanes }: ReceiverPathActivitySnapshot, t = receiverCopy.en.connection): string {
  return t.channelCount(lanes.length)
}
