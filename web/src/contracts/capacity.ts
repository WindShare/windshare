import type { FrameChannel } from './channel'

export const MINIMUM_BANDWIDTH_RETRY_MILLISECONDS = 30_000
export const MAXIMUM_BANDWIDTH_RETRY_MILLISECONDS = 60_000

/** Advisory congestion never renews protocol authority or suppresses heartbeats. */
export interface CapacityPressure {
  readonly remainingMilliseconds: number
  /** An explicit release is valid until newer pressure arrives. Expiry alone is silent. */
  readonly cleared: boolean
  subscribeClearance(listener: () => void): () => void
}

export class BandwidthWaitError extends Error {
  readonly retryAfterMilliseconds: number
  readonly pressure: CapacityPressure | undefined

  constructor(milliseconds: number, cause: unknown, pressure?: CapacityPressure) {
    super('Relay bandwidth is busy', { cause })
    this.name = 'BandwidthWaitError'
    this.pressure = pressure
    this.retryAfterMilliseconds = Math.min(MAXIMUM_BANDWIDTH_RETRY_MILLISECONDS,
      Math.max(MINIMUM_BANDWIDTH_RETRY_MILLISECONDS, milliseconds))
  }
}

/** Only deadline/progress owners may call this; cancellation and EOF stay distinct. */
export function capacityWaitFailure(channel: FrameChannel, cause: unknown): unknown {
  const pressure = channel.capacityPressure
  const milliseconds = pressure?.remainingMilliseconds ?? 0
  return milliseconds > 0 ? new BandwidthWaitError(milliseconds, cause, pressure) : cause
}
