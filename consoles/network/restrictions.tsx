import { useState } from 'react'
import { useAction } from '../ui/activity'
import { Badge, DateStamp, Notice } from '../ui/layout'
import type { AuthorityClient } from './api'
import { ACCOUNT_BYTES, decode, encode, SHARE_BYTES } from './identity'
import type { Report, Restrictions, Target } from './model'

interface AccessProps {
  api: AuthorityClient
  restrictions: Restrictions
  saved: (value: Restrictions) => void
}

function DirectRestriction({ target, api, restrictions, saved }: AccessProps & { target: Target }) {
  const action = useAction()
  const id = encode(target.id)
  const direct = target.kind === 'account' ? restrictions.accounts : restrictions.shares
  const blocked = direct.some(value => encode(value) === id)
  const actionLabel = blocked ? 'Remove restriction' : 'Restrict official relay access'
  return <div className="restriction"><div className="setting-row">
    <div><h3>Direct {target.kind} restriction</h3>
      <Badge tone={blocked ? 'warning' : 'neutral'}>{blocked ? 'Restricted' : 'No direct restriction'}</Badge></div>
    <button className={'secondary' + (blocked ? '' : ' danger-text')} disabled={action.busy} onClick={() => void action.run(async () => {
      saved(await api.restrict(target, !blocked))
      if (blocked) return target.kind === 'share'
        ? 'Restriction removed for this share. Account restrictions may still apply.'
        : 'Restriction removed for this account.'
      return 'Official relay access restricted for this ' + target.kind + '.'
    })}>{action.busy ? 'Saving…' : actionLabel}</button>
  </div><Notice feedback={action.feedback} /></div>
}

export function Reports({ reports, selectedId, ...access }: AccessProps & { reports: Report[]; selectedId?: string }) {
  const report = reports.find(item => encode(item.id) === selectedId)
  if (report) return <div className="view editor"><a href="#reports">Back to reports</a><h1>Share report</h1>
    <article className="panel padded"><div className="section-title"><h2>Reason</h2><DateStamp millis={report.created_at * 1000} /></div>
      <p className="report-reason">{report.reason}</p><dl className="facts"><dt>Share ID</dt><dd><code>{encode(report.share)}</code></dd></dl>
      <DirectRestriction key={encode(report.share)} {...access} target={{ kind: 'share', id: report.share }} />
    </article></div>
  return <div className="view"><div className="page-title"><h1>Reports</h1><div className="actions">
    <a href="#share-access" className="button secondary">Find share</a><a href="#account-access" className="button secondary">Account restrictions</a></div></div>
    {reports.length === 0 ? <div className="panel empty"><h2>No reports</h2><p>Submitted share reports will appear here for review.</p></div>
      : <ul className="panel report-list">{[...reports].sort((a, b) => b.created_at - a.created_at).map(item => <li key={encode(item.id)}>
        <a href={'#report/' + encode(item.id)}><span>{item.reason}</span><small><code>{encode(item.share)}</code> · <DateStamp millis={item.created_at * 1000} /></small></a>
      </li>)}</ul>}
  </div>
}

export function LookupAccess({ kind, ...access }: AccessProps & { kind: Target['kind'] }) {
  const [target, setTarget] = useState<Target | null>(null)
  const lookup = useAction()
  const label = kind === 'account' ? 'Account ID' : 'Share ID'
  return <div className="view editor"><a href="#reports">Back to reports</a><h1>{kind === 'account' ? 'Account restrictions' : 'Share restrictions'}</h1>
    <form className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const text = String(new FormData(event.currentTarget).get('id'))
      void lookup.run(async () => setTarget({ kind, id: decode(text, kind === 'account' ? ACCOUNT_BYTES : SHARE_BYTES, label) }))
    }}><div className="field"><label htmlFor="target-id">{label}</label>
        <input id="target-id" name="id" required autoCapitalize="none" spellCheck={false} onChange={() => setTarget(null)} /></div>
      <Notice feedback={lookup.feedback} /><div><button>Look up {kind}</button></div>
    </form>
    {target && <section className="panel padded"><code>{encode(target.id)}</code>
      <DirectRestriction key={encode(target.id)} {...access} target={target} /></section>}
  </div>
}
