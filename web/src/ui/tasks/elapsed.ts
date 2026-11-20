import { receiverCopy } from '../locales/receiver'

const MILLISECONDS_PER_SECOND = 1000
const SECONDS_PER_MINUTE = 60
const MINUTES_PER_HOUR = 60
const HOURS_PER_DAY = 24

export function formatElapsedTime(milliseconds: number, t = receiverCopy.en.downloads): string {
  if (milliseconds < MILLISECONDS_PER_SECOND) return t.lessSecond
  const seconds = Math.floor(milliseconds / MILLISECONDS_PER_SECOND)
  const minutes = Math.floor(seconds / SECONDS_PER_MINUTE)
  const hours = Math.floor(minutes / MINUTES_PER_HOUR)
  const days = Math.floor(hours / HOURS_PER_DAY)
  const parts = [
    days > 0 ? t.days(days) : '',
    hours > 0 ? t.hours(hours % HOURS_PER_DAY) : '',
    minutes > 0 ? t.minutes(minutes % MINUTES_PER_HOUR) : '',
    t.seconds(seconds % SECONDS_PER_MINUTE),
  ]
  return parts.filter(Boolean).join(' ')
}
