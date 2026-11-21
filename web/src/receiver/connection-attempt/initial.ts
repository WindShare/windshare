import { BandwidthWaitError } from '../../contracts/capacity'
import type { FrameChannel } from '../../contracts/channel'
import { RelayEndpointFailure } from '../relay-selection/race'
import { runAbortableConnection } from './lifetime'

const FAST_ATTEMPT_MILLISECONDS = 15_000
const MAXIMUM_ATTEMPT_MILLISECONDS = 30_000

export interface InitialJoinAttempt {
  readonly signal: AbortSignal
  readonly explicitRetry: boolean
  trackHandshake(relayBase: string, channel: FrameChannel, signal: AbortSignal): () => void
}

interface AttemptOptions<T> {
  readonly parent: AbortSignal
  readonly deadline: number
  readonly now: () => number
  readonly explicitRetry?: boolean
  readonly connect: (attempt: InitialJoinAttempt) => Promise<T>
  readonly close: (value: T) => Promise<void>
  readonly onBandwidthWait: (milliseconds: number) => void
  readonly onRelayFailure: (relayBase: string, failure: BandwidthWaitError) => void
}

export async function runInitialJoinAttempt<T>(options: AttemptOptions<T>): Promise<T> {
  const startedAt = options.now()
  const fastDeadline = Math.min(options.deadline, startedAt + FAST_ATTEMPT_MILLISECONDS)
  const finalDeadline = Math.min(options.deadline, startedAt + MAXIMUM_ATTEMPT_MILLISECONDS)
  const timeout = new AbortController()
  const signal = AbortSignal.any([options.parent, timeout.signal])
  const handshakes = new Map<FrameChannel, { relayBase: string; signal: AbortSignal }>()
  const attempt: InitialJoinAttempt = {
    signal,
    explicitRetry: options.explicitRetry ?? false,
    trackHandshake(relayBase, channel, candidateSignal) {
      handshakes.set(channel, { relayBase, signal: candidateSignal })
      return () => { handshakes.delete(channel) }
    },
  }

  function congestedHandshakes(): { relayBase: string; milliseconds: number }[] {
    const congested = []
    for (const [channel, handshake] of handshakes) {
      const milliseconds = channel.capacityPressure?.remainingMilliseconds ?? 0
      if (!handshake.signal.aborted && channel.state === 'open' && milliseconds > 0) {
        congested.push({ relayBase: handshake.relayBase, milliseconds })
      }
    }
    return congested
  }

  function expire(congested: ReturnType<typeof congestedHandshakes>): void {
    const failure = new Error('Initial connection attempt timed out')
    const rejections = congested.map(({ relayBase, milliseconds }) => {
      const wait = new BandwidthWaitError(milliseconds, failure)
      // The round's abort would otherwise erase evidence from a queued handshake.
      options.onRelayFailure(relayBase, wait)
      return new RelayEndpointFailure(relayBase, wait)
    })
    timeout.abort(rejections.length > 0
      ? new AggregateError(rejections, failure.message, { cause: failure }) : failure)
  }

  let timer = setTimeout(() => {
    const congested = congestedHandshakes()
    const remaining = finalDeadline - options.now()
    if (congested.length === 0 || remaining <= 0) {
      expire(congested)
      return
    }
    // Keeping the race alive gives an already queued handshake time to finish.
    // Renewed pressure notices cannot move the hard deadline or extend authority.
    timer = setTimeout(() => expire(congestedHandshakes()), remaining)
    options.onBandwidthWait(remaining)
  }, Math.max(0, fastDeadline - startedAt))
  try {
    return await runAbortableConnection(signal, () => options.connect(attempt), options.close)
  } finally {
    clearTimeout(timer)
    handshakes.clear()
  }
}
