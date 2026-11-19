import { useState } from 'react'
import { useAction } from '../ui/activity'
import { GIB, type Rate, type Settings, type SettingsChanges } from './model'

function formValues(settings: Settings) {
  return {
    mode: settings.registration, bits: String(settings.pow_bits), gib: String(settings.daily_bytes / GIB),
    sourceBurst: String(settings.source_rate.burst), sourceRefill: String(settings.source_rate.refill_seconds),
    globalBurst: String(settings.global_rate.burst), globalRefill: String(settings.global_rate.refill_seconds),
  }
}

type FormValues = ReturnType<typeof formValues>
type Draft = Partial<FormValues>
export type NumberField = Exclude<keyof FormValues, 'mode'>

function configuresWorkDifficulty(mode: Settings['registration']) {
  return mode === 'by_account_count' || mode === 'proof_of_work'
}

function rateChanges(burst: string | undefined, refillSeconds: string | undefined) {
  if (burst === undefined && refillSeconds === undefined) return
  const changes: Partial<Rate> = {}
  if (burst !== undefined) changes.burst = Number(burst)
  if (refillSeconds !== undefined) changes.refill_seconds = Number(refillSeconds)
  return changes
}

function settingsChanges(draft: Draft): SettingsChanges {
  const changes: SettingsChanges = {}
  if (draft.mode !== undefined) changes.registration = draft.mode
  if (draft.bits !== undefined) changes.pow_bits = Number(draft.bits)
  if (draft.gib !== undefined) changes.daily_bytes = Math.round(Number(draft.gib) * GIB)
  const source = rateChanges(draft.sourceBurst, draft.sourceRefill)
  const global = rateChanges(draft.globalBurst, draft.globalRefill)
  if (source) changes.source_rate = source
  if (global) changes.global_rate = global
  return changes
}

export function usePolicyEditor(settings: Settings) {
  const [draft, setDraft] = useState<Draft>({})
  const action = useAction()
  const saved = formValues(settings)
  const value = { ...saved, ...draft }
  const changes = settingsChanges(draft)
  // Keep pending difficulty edits visible if a refresh changes the registration mode.
  const showDifficulty = configuresWorkDifficulty(value.mode) || draft.bits !== undefined

  function edit<K extends keyof FormValues>(field: K, input: FormValues[K]) {
    setDraft(previous => {
      const next = { ...previous }
      if (input === saved[field]) delete next[field]
      else next[field] = input
      return next
    })
  }

  return { value, changes, showDifficulty, edit, discard: () => setDraft({}), action }
}
