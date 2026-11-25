import { useLayoutEffect, useRef, type ReactNode } from 'react'
import { useCopy, useLocale } from './use-locale'
import { sharedCopy } from './locales'
import type { Message } from './locale'
import type { Feedback } from './activity'
import { Icon, type IconName } from './Icon'
import { FocusPage } from './FocusPage'
import { Masthead } from './chrome/Masthead'
import './surface.css'

export function Frame({ area, tools, children }: { area: string; tools?: ReactNode; children: ReactNode }) {
  const { locale } = useLocale()
  const main = useRef<HTMLElement>(null)
  useLayoutEffect(() => {
    document.documentElement.lang = locale
    document.title = area + ' · WindShare'
  }, [area, locale])
  return <>
    <a className="skip" href="#main-content" onClick={event => {
      event.preventDefault()
      main.current?.focus()
    }}>{sharedCopy[locale].skip}</a>
    <Masthead area={area} tools={tools} />
    <main ref={main} id="main-content" tabIndex={-1}>{children}</main>
  </>
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB']
const NOTICE_ICONS = { success: 'check', warning: 'clock', error: 'alert' } as const

export function Notice({ feedback }: { feedback: Feedback | null }) {
  const { locale } = useLocale()
  if (!feedback) return null
  const failed = feedback.kind === 'error'
  return <div className={'notice ' + feedback.kind} role={failed ? 'alert' : 'status'}>
    <Icon name={NOTICE_ICONS[feedback.kind]} /><span>{feedback.message.format(locale)}</span>
  </div>
}

export function ReadError({ error, retry }: { error: Message | null; retry: () => void }) {
  const { locale } = useLocale()
  if (!error) return null
  return <div className="read-error"><Icon name="alert" /><span role="alert">{error.format(locale)}</span>
    <button type="button" className="quiet" onClick={retry}>{sharedCopy[locale].retry}</button></div>
}

export function SessionWait({ error, retry }: { error: Message | null; retry: () => void }) {
  const t = useCopy(sharedCopy)
  return <FocusPage icon={error ? 'alert' : 'loader'} title={error ? t.openFailed : t.opening}>
    <ReadError error={error} retry={retry} />
  </FocusPage>
}

export function Badge({ children, tone = 'neutral' }: {
  children: ReactNode; tone?: 'neutral' | 'good' | 'danger' | 'warning' | 'info'
}) {
  return <span className={'badge ' + tone}>{children}</span>
}

export function DateStamp({ millis }: { millis: number }) {
  const { locale } = useLocale()
  const date = new Date(millis)
  return <time dateTime={date.toISOString()}>{date.toLocaleString(locale, { dateStyle: 'medium', timeStyle: 'short' })}</time>
}

export function Bytes({ value }: { value: number }) {
  const { locale } = useLocale()
  const decimals = 2
  const precision = 10 ** decimals
  let scaled = value
  let unit = 0
  while (unit + 1 < BYTE_UNITS.length && Math.round(scaled * precision) >= 1024 * precision) {
    scaled /= 1024
    unit++
  }
  const amount = scaled.toLocaleString(locale, { maximumFractionDigits: decimals })
  return <span className="byte-value" title={sharedCopy[locale].exactBytes(value)}>{amount} {BYTE_UNITS[unit]}</span>
}

export function EmptyState({ icon, title, children }: { icon: IconName; title: string; children?: ReactNode }) {
  return <div className="empty"><span className="empty-icon"><Icon name={icon} size={24} /></span>
    <h2>{title}</h2>{children}</div>
}

export function BackLink({ href, children }: { href: string; children: ReactNode }) {
  return <a className="back-link" href={href}><Icon name="back" size={16} />{children}</a>
}
