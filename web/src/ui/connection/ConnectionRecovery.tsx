import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useId } from 'react'
import type { ReceiverReconnectActivity } from '../../receiver/connection-state'
import { useCountdownSeconds } from './countdown'

export function ConnectionRecovery({ activity, retry }: {
  readonly activity: ReceiverReconnectActivity
  readonly retry: () => void
}) {
  const t = useCopy(receiverCopy).connection
  const descriptionId = useId()
  const canRetry = activity.kind === 'waiting' && activity.reason === 'backoff'
  let buttonLabel = t.pleaseWait
  if (activity.kind === 'connecting') buttonLabel = t.connecting
  else if (canRetry) buttonLabel = t.retryNow
  return <section className="connection-recovery" aria-label={t.connectionRecovery}>
    <div className="connection-recovery-copy">
      <p className="connection-recovery-status" role="status" id={descriptionId}>{recoveryMessage(activity, t)}</p>
      {activity.kind === 'waiting' && <RetryCountdown key={activity.retryAt} retryAt={activity.retryAt} />}
      <p>{t.keepOpen}</p>
    </div>
    <button type="button" disabled={!canRetry} aria-describedby={descriptionId} onClick={retry}>
      {buttonLabel}
    </button>
  </section>
}

function recoveryMessage(activity: ReceiverReconnectActivity, t: typeof receiverCopy.en.connection): string {
  if (activity.kind === 'connecting') return t.reconnecting
  switch (activity.reason) {
    case 'backoff': return t.backoff
    case 'bandwidth': return t.bandwidth
    case 'capacity': return t.capacity
    case 'server': return t.server
  }
}

function RetryCountdown({ retryAt }: { readonly retryAt: number }) {
  const t = useCopy(receiverCopy).connection
  const seconds = useCountdownSeconds(retryAt)
  // Countdown ticks must not repeatedly interrupt screen reader announcements.
  return <p className="connection-recovery-countdown" role="timer" aria-live="off">
    {seconds > 0 ? t.retryIn(seconds) : t.retryShortly}
  </p>
}
