import { useSyncExternalStore } from 'react'
import { browserPreferences } from './browser'

export function usePreferences() {
  const preferences = useSyncExternalStore(
    browserPreferences.subscribe, browserPreferences.getSnapshot, browserPreferences.getSnapshot,
  )
  return { ...preferences, update: browserPreferences.update }
}
