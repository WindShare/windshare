export type Language = 'zh-CN' | 'en'
export type Appearance = 'system' | 'light' | 'dark'

export interface UIPreferences {
  readonly language: Language
  readonly appearance: Appearance
}

export const PREFERENCES_KEY = 'windshare.ui.preferences'

export function parsePreferences(raw: string | null, languages: readonly string[]): UIPreferences {
  let saved: Partial<UIPreferences> = {}
  try {
    const value: unknown = raw === null ? null : JSON.parse(raw)
    if (value !== null && typeof value === 'object') saved = value
  } catch {
    // A stale or damaged preference must never block access to a share.
  }
  const fallbackLanguage = languages[0]?.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en'
  const language = saved.language === 'zh-CN' || saved.language === 'en'
    ? saved.language : fallbackLanguage
  return {
    language,
    appearance: saved.appearance === 'light' || saved.appearance === 'dark' ? saved.appearance : 'system',
  }
}

export function createPreferencesStore(initial: UIPreferences, persist: (next: UIPreferences) => void) {
  let snapshot = initial
  const listeners = new Set<() => void>()
  const replace = (next: UIPreferences) => {
    if (next.language === snapshot.language && next.appearance === snapshot.appearance) return
    snapshot = next
    for (const listener of listeners) listener()
  }
  return {
    getSnapshot: () => snapshot,
    subscribe: (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener) } },
    replace,
    update: (change: Partial<UIPreferences>) => {
      replace({ ...snapshot, ...change })
      persist(snapshot)
    },
  }
}
