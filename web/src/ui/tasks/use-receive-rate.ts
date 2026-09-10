import { useEffect, useEffectEvent, useState } from 'react'
import { formatBytes } from '../v2-progress-presentation'
import type { TaskPresentation } from './model'
import { ReceiveRateSampler, type ReceiveRate } from './receive-rate'

const SAMPLE_INTERVAL_MILLISECONDS = 1000
const SECONDS_PER_MINUTE = 60
const MINUTES_PER_HOUR = 60

export function useReceiveRate(task: TaskPresentation): string | null {
  const active = task.stage === 'downloading'
  const identity = `${task.operationId}:${task.progress?.sampleIdentity ?? ''}`
  const [sample, setSample] = useState<Readonly<{ identity: string; rate: ReceiveRate | null }> | null>(null)
  const receipt = useEffectEvent(() => ({
    at: performance.now(),
    bytes: task.progress?.receivedBytes ?? 0n,
    remaining: task.progress?.remainingBytes ?? null,
  }))
  useEffect(() => {
    if (!active) return
    const initial = receipt()
    const sampler = new ReceiveRateSampler(initial.at, initial.bytes)
    // Sampling continues during a stalled read so an old throughput value decays
    // to zero instead of suggesting that bytes are still arriving.
    const timer = setInterval(() => {
      const current = receipt()
      setSample({ identity, rate: sampler.sample(current.at, current.bytes, current.remaining) })
    }, SAMPLE_INTERVAL_MILLISECONDS)
    return () => clearInterval(timer)
  }, [active, identity])
  if (!active || sample?.identity !== identity || sample.rate === null) return null
  const rate = sample.rate
  const remaining = rate.remainingSeconds === null ? '' : ` · About ${formatRemainingTime(rate.remainingSeconds)} left`
  return `${formatBytes(rate.bytesPerSecond)}/s${remaining}`
}

function formatRemainingTime(seconds: number): string {
  if (seconds < SECONDS_PER_MINUTE) return 'less than a minute'
  const minutes = Math.ceil(seconds / SECONDS_PER_MINUTE)
  if (minutes < MINUTES_PER_HOUR) return `${minutes} min`
  const hours = Math.floor(minutes / MINUTES_PER_HOUR)
  const rest = minutes % MINUTES_PER_HOUR
  return rest === 0 ? `${hours} hr` : `${hours} hr ${rest} min`
}
