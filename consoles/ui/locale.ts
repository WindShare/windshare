export const supportedLocales = ['en', 'zh-CN'] as const
export type Locale = typeof supportedLocales[number]
export const languageNames: Record<Locale, string> = { en: 'English', 'zh-CN': '简体中文' }
export const FALLBACK_LOCALE: Locale = 'en'
export type Translations<T> = Readonly<Record<Locale, T>>

export function isLocale(value: string | null): value is Locale {
  return value !== null && Object.hasOwn(languageNames, value)
}

export function matchBrowserLocale(languages: readonly string[]): Locale {
  for (const tag of languages) {
    const requested = tag.toLowerCase()
    const language = requested.split('-')[0]
    const match = supportedLocales.find(locale => locale.toLowerCase() === requested)
      ?? supportedLocales.find(locale => locale.split('-')[0] === language)
    if (match) return match
  }
  return FALLBACK_LOCALE
}

// Retain the identity of feedback so an in-flight or completed operation follows the display language.
export interface Message {
  format(locale: Locale): string
}

export function message<K extends string>(catalog: Translations<Record<K, string>>, key: K): Message {
  return { format: locale => catalog[locale][key] }
}

export class MessageError extends Error {
  constructor(readonly userMessage: Message) {
    super(userMessage.format(FALLBACK_LOCALE))
  }
}
