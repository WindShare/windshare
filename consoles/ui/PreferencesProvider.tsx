import { useEffect, useLayoutEffect, useState, type ReactNode } from 'react'
import { AppearanceContext, parseAppearance } from './appearance'
import { isLocale, matchBrowserLocale, type Locale } from './locale'
import { LocaleContext } from './use-locale'

const LANGUAGE_KEY = 'windshare.console.language'
const APPEARANCE_KEY = 'windshare.console.appearance'

function preferredLocale(saved: string | null): Locale {
  if (isLocale(saved)) return saved
  return matchBrowserLocale(navigator.languages.length ? navigator.languages : [navigator.language])
}

function readPreference(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}

function useStoredPreference<T extends string>(key: string, parse: (saved: string | null) => T) {
  const [value, setValue] = useState(() => parse(readPreference(key)))
  useEffect(() => {
    function syncPreference(event: StorageEvent) {
      if (event.key === key || event.key === null) setValue(parse(readPreference(key)))
    }
    window.addEventListener('storage', syncPreference)
    return () => window.removeEventListener('storage', syncPreference)
  }, [key, parse])

  function select(next: T) {
    setValue(next)
    try { localStorage.setItem(key, next) } catch {
      // Browsers with restricted storage can still keep a choice for this page.
    }
  }
  return [value, select] as const
}

function AppearanceProvider({ children }: { children: ReactNode }) {
  const [appearance, selectAppearance] = useStoredPreference(APPEARANCE_KEY, parseAppearance)
  useLayoutEffect(() => {
    document.documentElement.dataset.appearance = appearance
  }, [appearance])
  return <AppearanceContext.Provider value={{ appearance, selectAppearance }}>{children}</AppearanceContext.Provider>
}

export function PreferencesProvider({ children }: { children: ReactNode }) {
  const [locale, selectLocale] = useStoredPreference(LANGUAGE_KEY, preferredLocale)
  return <LocaleContext.Provider value={{ locale, selectLocale }}>
    <AppearanceProvider>{children}</AppearanceProvider>
  </LocaleContext.Provider>
}
