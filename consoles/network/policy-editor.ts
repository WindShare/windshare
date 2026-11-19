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

export function settingsFromDraft(draft: Draft, previous: Settings): Settings {
  return {
    registration: draft.mode,
    // Automatic registration can still require proof of work for bursty sources.
    pow_bits: draft.mode === 'proof_of_work' ? Number(draft.bits) : previous.pow_bits,
    daily_bytes: Math.round(Number(draft.gib) * GIB),
    source_rate: { burst: Number(draft.sourceBurst), refill_seconds: Number(draft.sourceRefill) },
    global_rate: { burst: Number(draft.globalBurst), refill_seconds: Number(draft.globalRefill) },
  }
}

export function usePolicyEditor() {
  const [draft, setDraft] = useState<Draft | null>(null)
  const action = useAction()
  return { draft, setDraft, action }
}
