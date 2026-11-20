import { useId } from 'react'
import type { ReceiverReconnectActivity } from '../../receiver/connection-state'
import { useCountdownSeconds } from './countdown'

export function ConnectionRecovery({ activity, retry }: {
  readonly activity: ReceiverReconnectActivity
  readonly retry: () => void
}) {
  const descriptionId = useId()
  const canRetry = activity.kind === 'waiting' && activity.reason === 'backoff'
  let buttonLabel = 'Please wait'
  if (activity.kind === 'connecting') buttonLabel = 'Connecting…'
  else if (canRetry) buttonLabel = 'Retry now'
  return <section className="connection-recovery" aria-label="Connection recovery">
    <div className="connection-recovery-copy">
      <p className="connection-recovery-status" role="status" id={descriptionId}>{recoveryMessage(activity)}</p>
      {activity.kind === 'waiting' && <RetryCountdown key={activity.retryAt} retryAt={activity.retryAt} />}
      <p>Keep this page open to preserve your download progress.</p>
    </div>
    <button type="button" disabled={!canRetry} aria-describedby={descriptionId} onClick={retry}>
      {buttonLabel}
    </button>
  </section>
}

function recoveryMessage(activity: ReceiverReconnectActivity): string {
  if (activity.kind === 'connecting') return 'Reconnecting to the sender…'
  switch (activity.reason) {
    case 'backoff': return 'Waiting to reconnect. You can retry now.'
    case 'authorization': return 'Official relay access is restricted for this share.'
    case 'bandwidth': return 'Relay bandwidth is busy. Your download will resume automatically.'
    case 'capacity': return 'Taking a short pause after repeated connection attempts.'
    case 'server': return 'The connection service asked us to wait before retrying.'
  }
}

function RetryCountdown({ retryAt }: { readonly retryAt: number }) {
  const seconds = useCountdownSeconds(retryAt)
  // Countdown ticks must not repeatedly interrupt screen reader announcements.
  return <p className="connection-recovery-countdown" role="timer" aria-live="off">
    {seconds > 0 ? `Retrying automatically in ${seconds} s.` : 'Retrying shortly…'}
  </p>
}
