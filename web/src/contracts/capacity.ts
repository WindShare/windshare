import type { FrameChannel } from './channel'

export const MINIMUM_BANDWIDTH_RETRY_MILLISECONDS = 30_000
export const MAXIMUM_BANDWIDTH_RETRY_MILLISECONDS = 60_000

/** Advisory congestion never renews protocol authority or suppresses heartbeats. */
export class BandwidthWaitError extends Error {
  readonly retryAfterMilliseconds: number
  constructor(milliseconds: number, cause: unknown) {
    super('Relay bandwidth is busy', { cause })
    this.name = 'BandwidthWaitError'
    this.retryAfterMilliseconds = Math.min(MAXIMUM_BANDWIDTH_RETRY_MILLISECONDS,
      Math.max(MINIMUM_BANDWIDTH_RETRY_MILLISECONDS, milliseconds))
  }
}

/** Only deadline/progress owners may call this; cancellation and EOF stay distinct. */
export function capacityWaitFailure(channel: FrameChannel, cause: unknown): unknown {
  const milliseconds = channel.capacityWaitMilliseconds ?? 0
  return milliseconds > 0 ? new BandwidthWaitError(milliseconds, cause) : cause
}
