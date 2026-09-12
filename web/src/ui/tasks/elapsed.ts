const MILLISECONDS_PER_SECOND = 1000
const SECONDS_PER_MINUTE = 60
const MINUTES_PER_HOUR = 60
const HOURS_PER_DAY = 24

export const ELAPSED_DESCRIPTION = 'From starting the download until the result is ready, including pauses and reconnecting. Excludes waiting to save and browser-managed saving.'

export function formatElapsedTime(milliseconds: number): string {
  if (milliseconds < MILLISECONDS_PER_SECOND) return 'Less than 1 sec'
  const seconds = Math.floor(milliseconds / MILLISECONDS_PER_SECOND)
  const minutes = Math.floor(seconds / SECONDS_PER_MINUTE)
  const hours = Math.floor(minutes / MINUTES_PER_HOUR)
  const days = Math.floor(hours / HOURS_PER_DAY)
  const dayUnit = days === 1 ? 'day' : 'days'
  const parts = [
    days > 0 ? `${days} ${dayUnit}` : '',
    hours > 0 ? `${hours % HOURS_PER_DAY} hr` : '',
    minutes > 0 ? `${minutes % MINUTES_PER_HOUR} min` : '',
    `${seconds % SECONDS_PER_MINUTE} sec`,
  ]
  return parts.filter(Boolean).join(' ')
}
