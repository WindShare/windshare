import { useCallback, useEffect, useRef, useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { BackLink, Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { Icon } from '../ui/Icon'
import { message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { networkCopy } from './locales'
import type { AuthorityClient } from './api'
import { encode, requireIdentity } from './identity'
import type { Restrictions, Target } from './model'

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

function ReportDetail({ id, ...access }: AccessProps & { id: string }) {
  const t = useCopy(networkCopy)
  const { api } = access
  const read = useCallback((signal: AbortSignal) => api.report(id, signal), [api, id])
  const resource = useResource(read)
  const report = resource.value
  return <div className="view editor"><BackLink href="#reports">{t.backToReports}</BackLink><h1>{t.shareReport}</h1>
    <ReadError error={resource.error} retry={resource.retry} />
    {!report && resource.busy && <p className="report-loading" role="status"><Icon name="loader" />{t.loadingReport}</p>}
    {report === null && <div className="panel"><EmptyState icon="report" title={t.reportNotFound} /></div>}
    {report && <article className="panel padded">
      <div className="section-title"><h2>{t.reason}</h2><DateStamp millis={report.created_at * 1000} /></div>
      <p className="report-reason">{report.reason}</p><dl className="facts"><dt>{t.shareId}</dt><dd><code>{encode(report.share)}</code></dd></dl>
      <DirectRestriction key={encode(report.share)} {...access} target={{ kind: 'share', id: report.share }} />
    </article>}
  </div>
}

function ReportList({ api, leaving }: { api: AuthorityClient; leaving: () => void }) {
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const list = useResource(api.reports)
  const page = list.value
  const cursor = page?.next_cursor
  return <div className="view"><div className="page-title"><h1>{t.reports}</h1><div className="actions">
    <a href="#share-access" className="button secondary"><Icon name="search" size={16} />{t.findShare}</a>
    <a href="#account-access" className="button secondary"><Icon name="user" size={16} />{t.accountRestrictions}</a></div></div>
    <ReadError error={list.error} retry={list.retry} />
    {!page && list.busy && <p className="report-loading" role="status"><Icon name="loader" />{t.loadingReports}</p>}
    {page?.reports.length === 0 && <div className="panel"><EmptyState icon="report" title={t.noReports}><p>{t.reportHint}</p></EmptyState></div>}
    {page && page.reports.length > 0 && <ul className="panel report-list">{page.reports.map(item => <li key={encode(item.id)}>
      <a href={'#report/' + encode(item.id)} onClick={leaving}><span className="row-icon"><Icon name="report" size={16} /></span>
        <span className="report-entry"><strong>{item.reason}</strong><small><code>{encode(item.share)}</code> · <DateStamp millis={item.created_at * 1000} /></small></span>
        <Icon name="right" size={16} /></a>
    </li>)}</ul>}
    {cursor && <div className="actions"><button type="button" className="secondary" disabled={list.busy}
      onClick={() => list.extend(signal => api.reports(signal, cursor), (previous, next) => ({
        ...next, reports: [...previous.reports, ...next.reports],
      }))}>{list.busy ? shared.loadingMore : shared.loadMore}</button></div>}
  </div>
}

export function Reports({ selectedId, revision, ...access }: AccessProps & {
  selectedId?: string; revision: { list: number; detail: number }
}) {
  const showList = selectedId === undefined
  const [visitedList, setVisitedList] = useState(showList)
  const scroll = useRef<number | null>(null)
  // Keep the fetched pages only after the list has been opened, including while reading a report.
  if (showList && !visitedList) setVisitedList(true)
  useEffect(() => {
    if (!showList || scroll.current === null) return
    const top = scroll.current
    const frame = requestAnimationFrame(() => {
      scroll.current = null
      window.scrollTo(0, top)
    })
    return () => cancelAnimationFrame(frame)
  }, [showList])
  return <>
    {selectedId !== undefined && <ReportDetail key={selectedId + ':' + revision.detail} {...access} id={selectedId} />}
    {visitedList && <div key="report-list" hidden={!showList}><ReportList key={revision.list} api={access.api}
      leaving={() => { scroll.current = window.scrollY }} /></div>}
  </>
}

export function LookupAccess({ kind, ...access }: AccessProps & { kind: Target['kind'] }) {
  const [target, setTarget] = useState<Target | null>(null)
  const t = useCopy(networkCopy)
  const lookup = useAction()
  const label = kind === 'account' ? t.accountId : t.shareId
  return <div className="view editor"><BackLink href="#reports">{t.backToReports}</BackLink><h1>{kind === 'account' ? t.accountRestrictions : t.shareRestrictions}</h1>
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
