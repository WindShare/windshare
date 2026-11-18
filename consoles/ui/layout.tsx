import type { ReactNode } from 'react'
import type { Feedback } from './activity'
import './surface.css'

export function Frame({ area, tools, children }: { area: string; tools?: ReactNode; children: ReactNode }) {
  return <>
    <a className="skip" href="#main-content" onClick={event => {
      event.preventDefault()
      document.getElementById('main-content')?.focus()
    }}>Skip to content</a>
    <header className="masthead"><div className="mast-inner">
      <div className="brand"><span className="wordmark">WindShare</span><span className="brand-rule" />
        <span className="area">{area}</span></div>
      {tools}
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
  if (!feedback) return null
  return <p className={'notice ' + feedback.kind} role={feedback.kind === 'error' ? 'alert' : 'status'}>{feedback.message}</p>
}

export function ReadError({ error, retry }: { error: string; retry: () => void }) {
  if (!error) return null
  return <div className="read-error"><span role="alert">{error}</span>
    <button type="button" className="quiet" onClick={retry}>Retry</button></div>
}

export function SessionWait({ error, retry }: { error: string; retry: () => void }) {
  return <FocusPage title={error ? 'Could not open the console' : 'Opening console…'}>
    <ReadError error={error} retry={retry} />
  </FocusPage>
}

export function Badge({ children, tone = 'neutral' }: {
  children: ReactNode; tone?: 'neutral' | 'good' | 'warning'
}) {
  return <span className={'badge ' + tone}>{children}</span>
}

export function DateStamp({ millis }: { millis: number }) {
  const date = new Date(millis)
  return <time dateTime={date.toISOString()}>{date.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}</time>
}
