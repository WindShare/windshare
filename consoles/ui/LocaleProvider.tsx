import { useEffect, useState, type ReactNode } from 'react'
import { isLocale, matchBrowserLocale, type Locale } from './locale'
import { LocaleContext } from './use-locale'

const STORAGE_KEY = 'windshare.console.language'

function preferredLocale(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (isLocale(saved)) return saved
  } catch {
    // The console also runs in browsers that block persistent storage.
  }
  return matchBrowserLocale(navigator.languages.length ? navigator.languages : [navigator.language])
}

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocale] = useState(preferredLocale)
  useEffect(() => {
    function syncPreference(event: StorageEvent) {
      if (event.key === STORAGE_KEY || event.key === null) setLocale(preferredLocale())
    }
    window.addEventListener('storage', syncPreference)
    return () => window.removeEventListener('storage', syncPreference)
  }, [])

  function selectLocale(next: Locale) {
    setLocale(next)
    try { localStorage.setItem(STORAGE_KEY, next) } catch {
      // Keep the selection for this page when storage is unavailable.
    }
  }

  return <LocaleContext.Provider value={{ locale, selectLocale }}>{children}</LocaleContext.Provider>
}
