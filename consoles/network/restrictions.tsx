import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { Badge, DateStamp, Notice } from '../ui/layout'
import { message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { networkCopy } from './locales'
import type { AuthorityClient } from './api'
import { encode, requireIdentity } from './identity'
import type { Report, Restrictions, Target } from './model'

interface AccessProps {
  api: AuthorityClient
  restrictions: Restrictions
  saved: (value: Restrictions) => void
}

function DirectRestriction({ target, api, restrictions, saved }: AccessProps & { target: Target }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const id = encode(target.id)
  const direct = target.kind === 'account' ? restrictions.accounts : restrictions.shares
  const blocked = direct.some(value => encode(value) === id)
  const actionLabel = blocked ? t.removeRestriction : t.restrictRelay
  return <div className="restriction"><div className="setting-row">
    <div><h3>{target.kind === 'account' ? t.directAccount : t.directShare}</h3>
      <Badge tone={blocked ? 'warning' : 'neutral'}>{blocked ? t.restricted : t.noDirectRestriction}</Badge></div>
    <button className={'secondary' + (blocked ? '' : ' danger-text')} disabled={action.busy} onClick={() => void action.run(async () => {
      saved(await api.restrict(target, !blocked))
      if (blocked) return message(networkCopy, target.kind === 'share' ? 'shareAllowed' : 'accountAllowed')
      return message(networkCopy, target.kind === 'share' ? 'shareRestricted' : 'accountRestricted')
    })}>{action.busy ? t.saving : actionLabel}</button>
  </div><Notice feedback={action.feedback} /></div>
}

export function Reports({ reports, selectedId, ...access }: AccessProps & { reports: Report[]; selectedId?: string }) {
  const t = useCopy(networkCopy)
  const report = reports.find(item => encode(item.id) === selectedId)
  if (report) return <div className="view editor"><a href="#reports">{t.backToReports}</a><h1>{t.shareReport}</h1>
    <article className="panel padded"><div className="section-title"><h2>{t.reason}</h2><DateStamp millis={report.created_at * 1000} /></div>
      <p className="report-reason">{report.reason}</p><dl className="facts"><dt>{t.shareId}</dt><dd><code>{encode(report.share)}</code></dd></dl>
      <DirectRestriction key={encode(report.share)} {...access} target={{ kind: 'share', id: report.share }} />
    </article></div>
  return <div className="view"><div className="page-title"><h1>{t.reports}</h1><div className="actions">
    <a href="#share-access" className="button secondary">{t.findShare}</a><a href="#account-access" className="button secondary">{t.accountRestrictions}</a></div></div>
    {reports.length === 0 ? <div className="panel empty"><h2>{t.noReports}</h2><p>{t.reportHint}</p></div>
      : <ul className="panel report-list">{[...reports].sort((a, b) => b.created_at - a.created_at).map(item => <li key={encode(item.id)}>
        <a href={'#report/' + encode(item.id)}><span>{item.reason}</span><small><code>{encode(item.share)}</code> · <DateStamp millis={item.created_at * 1000} /></small></a>
      </li>)}</ul>}
  </div>
}

export function LookupAccess({ kind, ...access }: AccessProps & { kind: Target['kind'] }) {
  const [target, setTarget] = useState<Target | null>(null)
  const t = useCopy(networkCopy)
  const lookup = useAction()
  const label = kind === 'account' ? t.accountId : t.shareId
  return <div className="view editor"><a href="#reports">{t.backToReports}</a><h1>{kind === 'account' ? t.accountRestrictions : t.shareRestrictions}</h1>
    <ValidatedForm className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const text = String(new FormData(event.currentTarget).get('id'))
      void lookup.run(async () => setTarget({ kind, id: requireIdentity(text, kind) }))
    }}><div className="field"><label htmlFor="target-id">{label}</label>
        <input id="target-id" name="id" required autoCapitalize="none" spellCheck={false} onChange={() => setTarget(null)} /></div>
      <Notice feedback={lookup.feedback} /><div><button>{kind === 'account' ? t.lookupAccount : t.lookupShare}</button></div>
    </ValidatedForm>
    {target && <section className="panel padded"><code>{encode(target.id)}</code>
      <DirectRestriction key={encode(target.id)} {...access} target={target} /></section>}
  </div>
}
