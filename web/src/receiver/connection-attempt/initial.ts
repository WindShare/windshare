import { BandwidthWaitError } from '../../contracts/capacity'
import type { FrameChannel } from '../../contracts/channel'
import { runAbortableConnection } from './lifetime'

const FAST_ATTEMPT_MILLISECONDS = 15_000
const MAXIMUM_ATTEMPT_MILLISECONDS = 30_000

export interface InitialJoinAttempt {
  readonly signal: AbortSignal
  trackHandshake(channel: FrameChannel, signal: AbortSignal): () => void
}

interface AttemptOptions<T> {
  readonly parent: AbortSignal
  readonly deadline: number
  readonly now: () => number
  readonly connect: (attempt: InitialJoinAttempt) => Promise<T>
  readonly close: (value: T) => Promise<void>
  readonly onBandwidthWait: (milliseconds: number) => void
}

export async function runInitialJoinAttempt<T>(options: AttemptOptions<T>): Promise<T> {
  const startedAt = options.now()
  const fastDeadline = Math.min(options.deadline, startedAt + FAST_ATTEMPT_MILLISECONDS)
  const finalDeadline = Math.min(options.deadline, startedAt + MAXIMUM_ATTEMPT_MILLISECONDS)
  const timeout = new AbortController()
  const signal = AbortSignal.any([options.parent, timeout.signal])
  const handshakes = new Map<FrameChannel, AbortSignal>()
  const attempt: InitialJoinAttempt = {
    signal,
    trackHandshake(channel, candidateSignal) {
      handshakes.set(channel, candidateSignal)
      return () => { handshakes.delete(channel) }
    },
  }

  function capacityWait(): number {
    let remaining = 0
    for (const [channel, candidateSignal] of handshakes) {
      if (!candidateSignal.aborted && channel.state === 'open') {
        remaining = Math.max(remaining, channel.capacityWaitMilliseconds ?? 0)
      }
    }
    return remaining
  }

  function expire(milliseconds: number): void {
    const failure = new Error('Initial connection attempt timed out')
    timeout.abort(milliseconds > 0 ? new BandwidthWaitError(milliseconds, failure) : failure)
  }

  let timer = setTimeout(() => {
    const wait = capacityWait()
    const remaining = finalDeadline - options.now()
    if (wait === 0 || remaining <= 0) {
      expire(wait)
      return
    }
    // Keeping the race alive gives an already queued handshake time to finish.
    // Renewed pressure notices cannot move the hard deadline or extend authority.
    timer = setTimeout(() => expire(capacityWait()), remaining)
    options.onBandwidthWait(remaining)
  }, Math.max(0, fastDeadline - startedAt))
  try {
    return await runAbortableConnection(signal, () => options.connect(attempt), options.close)
  } finally {
    clearTimeout(timer)
    handshakes.clear()
  }
}
