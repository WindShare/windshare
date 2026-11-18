import { useId, useLayoutEffect, type ReactNode } from 'react'
import { appearances, parseAppearance, useAppearance } from './appearance'
import { isLocale, languageNames, supportedLocales, type Message } from './locale'
import { useCopy, useLocale } from './use-locale'
import { sharedCopy } from './locales'
import type { Feedback } from './activity'
import './surface.css'

function LanguageSelect() {
  const { locale, selectLocale } = useLocale()
  const id = useId()
  return <>
    <label className="sr-only" htmlFor={id}>{sharedCopy[locale].language}</label>
    <select id={id} value={locale} onChange={event => {
      if (isLocale(event.target.value)) selectLocale(event.target.value)
    }}>
      {supportedLocales.map(value => <option key={value} value={value} lang={value}>{languageNames[value]}</option>)}
    </select>
  </>
}

function AppearanceSelect() {
  const { appearance, selectAppearance } = useAppearance()
  const t = useCopy(sharedCopy)
  return <select aria-label={t.appearance} value={appearance}
    onChange={event => selectAppearance(parseAppearance(event.currentTarget.value))}>
    {appearances.map(value => <option key={value} value={value}>{t.appearanceOptions[value]}</option>)}
  </select>
}

export function Frame({ area, tools, children }: { area: string; tools?: ReactNode; children: ReactNode }) {
  const { locale } = useLocale()
  useLayoutEffect(() => {
    document.documentElement.lang = locale
    document.title = area + ' · WindShare'
  }, [area, locale])
  return <>
    <a className="skip" href="#main-content" onClick={event => {
      event.preventDefault()
      document.getElementById('main-content')?.focus()
    }}>{sharedCopy[locale].skip}</a>
    <header className="masthead"><div className="mast-inner">
      <div className="brand"><span className="wordmark">WindShare</span><span className="brand-rule" />
        <span className="area">{area}</span></div>
      <div className="mast-tools"><LanguageSelect /><AppearanceSelect />{tools}</div>
    </div></header>
    <main id="main-content" tabIndex={-1}>{children}</main>
  </>
}

export function FocusPage({ label, title, children }: { label?: string; title: string; children?: ReactNode }) {
  return <div className="focus-page"><div className="focus-card">
    {label && <p className="eyebrow">{label}</p>}
    <h1>{title}</h1>{children}
  </div></div>
}

export function Notice({ feedback }: { feedback: Feedback | null }) {
  const { locale } = useLocale()
  if (!feedback) return null
  return <p className={'notice ' + feedback.kind} role={feedback.kind === 'error' ? 'alert' : 'status'}>{feedback.message.format(locale)}</p>
}

export function ReadError({ error, retry }: { error: Message | null; retry: () => void }) {
  const { locale } = useLocale()
  if (!error) return null
  return <div className="read-error"><span role="alert">{error.format(locale)}</span>
    <button type="button" className="quiet" onClick={retry}>{sharedCopy[locale].retry}</button></div>
}

export function SessionWait({ error, retry }: { error: Message | null; retry: () => void }) {
  const t = useCopy(sharedCopy)
  return <FocusPage title={error ? t.openFailed : t.opening}>
    <ReadError error={error} retry={retry} />
  </FocusPage>
}

export function Badge({ children, tone = 'neutral' }: {
  children: ReactNode; tone?: 'neutral' | 'good' | 'warning'
}) {
  return <span className={'badge ' + tone}>{children}</span>
}

export function DateStamp({ millis }: { millis: number }) {
  const { locale } = useLocale()
  const date = new Date(millis)
  return <time dateTime={date.toISOString()}>{date.toLocaleString(locale, { dateStyle: 'medium', timeStyle: 'short' })}</time>
}
