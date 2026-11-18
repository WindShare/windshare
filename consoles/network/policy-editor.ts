import { useState } from 'react'
import { useAction } from '../ui/activity'
import { GIB, type Settings } from './model'

export function formValues(value: Settings) {
  return {
    mode: value.registration, bits: String(value.pow_bits), gib: String(value.daily_bytes / GIB),
    sourceBurst: String(value.source_rate.burst), sourceRefill: String(value.source_rate.refill_seconds),
    globalBurst: String(value.global_rate.burst), globalRefill: String(value.global_rate.refill_seconds),
  }
}
export type Draft = ReturnType<typeof formValues>

export function usePolicyEditor() {
  const [draft, setDraft] = useState<Draft | null>(null)
  const action = useAction()
  return { draft, setDraft, action }
}
