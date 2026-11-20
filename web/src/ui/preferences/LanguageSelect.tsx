import { shellCopy } from '../locales/shell'
import { usePreferences } from './use-preferences'
import type { Language } from './model'

const LANGUAGE_NAMES = { en: 'English', 'zh-CN': '简体中文' } satisfies Record<Language, string>

export function LanguageSelect({ id, className }: { readonly id?: string; readonly className?: string }) {
  const { language, update } = usePreferences()
  return <select id={id} className={className} value={language}
    aria-label={shellCopy[language].language}
    onChange={event => update({ language: event.target.value as Language })}>
    {Object.entries(LANGUAGE_NAMES).map(([value, name]) =>
      <option key={value} value={value} lang={value}>{name}</option>)}
  </select>
}
