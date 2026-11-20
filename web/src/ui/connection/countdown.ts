import { useEffect, useState } from 'react'
import { systemReconnectClock } from '../../receiver/recovery-clock'

const COUNTDOWN_INTERVAL_MILLISECONDS = 1_000
const MILLISECONDS_PER_SECOND = 1_000

/** Mount callers with `key={retryAt}` so new deadlines re-initialize cleanly. */
export function useCountdownSeconds(retryAt: number | undefined): number {
  const [now, setNow] = useState(() => systemReconnectClock.now())
  useEffect(() => {
    if (retryAt === undefined || systemReconnectClock.now() >= retryAt) return
    const timer = setInterval(() => {
      const current = systemReconnectClock.now()
      setNow(current)
      if (current >= retryAt) clearInterval(timer)
    }, COUNTDOWN_INTERVAL_MILLISECONDS)
    return () => clearInterval(timer)
  }, [retryAt])
  if (retryAt === undefined) return 0
  return Math.max(0, Math.ceil((retryAt - now) / MILLISECONDS_PER_SECOND))
}
