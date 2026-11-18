import { createContext, useContext } from 'react'
import type { Locale, Translations } from './locale'

export const LocaleContext = createContext<{ locale: Locale; selectLocale: (locale: Locale) => void } | null>(null)

export function useLocale() {
  const value = useContext(LocaleContext)
  if (!value) throw new Error('Missing console locale provider')
  return value
}

export function useCopy<T>(catalog: Translations<T>): T {
  return catalog[useLocale().locale]
}
