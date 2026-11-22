export const SUPPORTED_LOCALES = ['en', 'zh-CN'] as const
export type Locale = (typeof SUPPORTED_LOCALES)[number]

export const LANGUAGE_NAMES: Readonly<Record<Locale, string>> = {
  en: 'English',
  'zh-CN': '简体中文',
}

export const APPEARANCES = ['system', 'light', 'dark'] as const
export type Appearance = (typeof APPEARANCES)[number]

// Share localStorage keys with WindShare's other management consoles.
const LANGUAGE_KEY = 'windshare.console.language'
const APPEARANCE_KEY = 'windshare.console.appearance'

export function isLocale(value: string | null): value is Locale {
  return value === 'en' || value === 'zh-CN'
}

export function parseAppearance(value: string | null): Appearance {
  if (value === 'light' || value === 'dark' || value === 'system') return value
  return 'system'
}

function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

function writeStorage(key: string, value: string): void {
  try {
    localStorage.setItem(key, value)
  } catch {
    // Restricted browser storage still keeps the in-memory choice for this session.
  }
}

function detectBrowserLocale(): Locale {
  const candidates = navigator.languages.length > 0 ? navigator.languages : [navigator.language]
  for (const tag of candidates) {
    const lower = tag.toLowerCase()
    if (lower === 'zh-cn' || lower.startsWith('zh')) return 'zh-CN'
    if (lower === 'en' || lower.startsWith('en-')) return 'en'
  }
  return 'en'
}

export function loadInitialLocale(): Locale {
  const saved = readStorage(LANGUAGE_KEY)
  return isLocale(saved) ? saved : detectBrowserLocale()
}

export function saveLocale(locale: Locale): void {
  writeStorage(LANGUAGE_KEY, locale)
}

export function loadInitialAppearance(): Appearance {
  return parseAppearance(readStorage(APPEARANCE_KEY))
}

export function applyAppearance(appearance: Appearance): void {
  if (appearance === 'system') {
    delete document.documentElement.dataset.appearance
  } else {
    document.documentElement.dataset.appearance = appearance
  }
}

export function saveAppearance(appearance: Appearance): void {
  writeStorage(APPEARANCE_KEY, appearance)
  applyAppearance(appearance)
}
