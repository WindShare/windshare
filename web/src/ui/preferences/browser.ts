import { createPreferencesStore, parsePreferences, PREFERENCES_KEY, type UIPreferences } from './model'

function readSavedPreferences(): string | null {
  try { return window.localStorage.getItem(PREFERENCES_KEY) } catch { return null }
}

const browserLanguages = typeof navigator === 'undefined' ? ['en'] : navigator.languages
export const browserPreferences = createPreferencesStore(
  parsePreferences(null, browserLanguages),
  next => {
    try { window.localStorage.setItem(PREFERENCES_KEY, JSON.stringify(next)) } catch {
      // Restricted storage still permits in-memory language and appearance changes.
    }
  },
)

function applyPreferences(preferences: UIPreferences): void {
  document.documentElement.lang = preferences.language
  document.documentElement.dataset.appearance = preferences.appearance
}

let initialized = false
export function initializeBrowserPreferences(): void {
  if (initialized || typeof document === 'undefined') return
  initialized = true
  // The document captures and erases its capability before touching browser storage.
  browserPreferences.replace(parsePreferences(readSavedPreferences(), browserLanguages))
  applyPreferences(browserPreferences.getSnapshot())
  browserPreferences.subscribe(() => applyPreferences(browserPreferences.getSnapshot()))
  window.addEventListener('storage', event => {
    if (event.key === PREFERENCES_KEY || event.key === null) {
      browserPreferences.replace(parsePreferences(readSavedPreferences(), browserLanguages))
    }
  })
}
