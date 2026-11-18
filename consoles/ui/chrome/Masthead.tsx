import type { ReactNode } from 'react'
import { appearances, parseAppearance, useAppearance } from '../appearance'
import { isLocale, languageNames, supportedLocales } from '../locale'
import { useLocale } from '../use-locale'
import { sharedCopy } from '../locales'
import { Icon } from '../Icon'
import { WindMark } from './WindMark'

function Preferences() {
  const { locale, selectLocale } = useLocale()
  const { appearance, selectAppearance } = useAppearance()
  const t = sharedCopy[locale]
  return <>
    <label className="preference language" title={t.language}>
      <Icon name="globe" size={16} /><span aria-hidden="true">{languageNames[locale]}</span><Icon name="down" size={13} />
      <select aria-label={t.language} value={locale} onChange={event => {
        if (isLocale(event.target.value)) selectLocale(event.target.value)
      }}>
        {supportedLocales.map(value => <option key={value} value={value} lang={value}>{languageNames[value]}</option>)}
      </select>
    </label>
    <label className="preference appearance" title={t.appearance + ' · ' + t.appearanceOptions[appearance]}>
      <Icon name={appearance} /><span className="sr-only">{t.appearanceOptions[appearance]}</span>
      <select aria-label={t.appearance} value={appearance} onChange={event => selectAppearance(parseAppearance(event.target.value))}>
        {appearances.map(value => <option key={value} value={value}>{t.appearanceOptions[value]}</option>)}
      </select>
    </label>
  </>
}

export function Masthead({ area, tools }: { area: string; tools?: ReactNode }) {
  return <header className="masthead"><div className="mast-inner">
    <div className="brand"><WindMark /><div className="brand-type"><span className="wordmark">WindShare</span>
      <span className="area">{area}</span></div></div>
    <div className="mast-tools"><Preferences />{tools}</div>
  </div></header>
}
