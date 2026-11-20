import { useCallback, useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { BackLink, Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { Icon } from '../ui/Icon'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { useListNavigation } from './list-navigation'
import { networkCopy } from './locales'
import type { AuthorityClient } from './api'
import { directlyRestricted, encode, requireIdentity } from './identity'
import type { Restrictions, Target } from './model'

export interface AccessProps {
  api: AuthorityClient
  restrictions: Restrictions
  saved: (value: Restrictions) => void
}

export function DirectRestriction({ target, api, saved, blocked }: Pick<AccessProps, 'api' | 'saved'> & { target: Target; blocked: boolean }) {
  const t = useCopy(networkCopy)
  const action = useAction()
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
    {!report && resource.busy && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingReport}</p>}
    {report === null && <div className="panel"><EmptyState icon="report" title={t.reportNotFound} /></div>}
    {report && <article className="panel padded">
      <div className="section-title"><h2>{t.reason}</h2><DateStamp millis={report.created_at * 1000} /></div>
      <p className="report-reason">{report.reason}</p><dl className="facts"><dt>{t.shareId}</dt><dd><code>{encode(report.share)}</code></dd></dl>
      <DirectRestriction key={encode(report.share)} {...access} target={{ kind: 'share', id: report.share }}
        blocked={directlyRestricted(access.restrictions, { kind: 'share', id: report.share })} />
    </article>}
  </div>
}

function ReportList({ leaving, ...access }: AccessProps & { leaving: () => void }) {
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const list = useResource(access.api.reports)
  const page = list.value
  const cursor = page?.next_cursor
  return <div className="view"><div className="page-title"><h1>{t.reportsAndRestrictions}</h1>
    <a href="#share-access" className="button secondary"><Icon name="search" size={16} />{t.findShare}</a></div>
    <section className="stack"><h2>{t.pendingReports}</h2>
    <ReadError error={list.error} retry={list.retry} />
    {!page && list.busy && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingReports}</p>}
    {page?.reports.length === 0 && <div className="panel"><EmptyState icon="report" title={t.noReports} /></div>}
    {page && page.reports.length > 0 && <ul className="panel report-list">{page.reports.map(item => <li key={encode(item.id)}>
      <a href={'#report/' + encode(item.id)} onClick={leaving}><span className="row-icon"><Icon name="report" size={16} /></span>
        <span className="report-entry"><strong>{item.reason}</strong><small><code>{encode(item.share)}</code> · <DateStamp millis={item.created_at * 1000} /></small></span>
        <Icon name="right" size={16} /></a>
    </li>)}</ul>}
    {cursor && <div className="actions"><button type="button" className="secondary" disabled={list.busy}
      onClick={() => list.extend(signal => access.api.reports(signal, cursor), (previous, next) => ({
        ...next, reports: [...previous.reports, ...next.reports],
      }))}>{list.busy ? shared.loadingMore : shared.loadMore}</button></div>}</section>
    <RestrictionList kind="account" {...access} />
    <RestrictionList kind="share" {...access} />
  </div>
}

export function Reports({ selectedId, revision, ...access }: AccessProps & {
  selectedId?: string; revision: { list: number; detail: number }
}) {
  const { visitedList, leaving } = useListNavigation(selectedId === undefined)
  return <>
    {selectedId !== undefined && <ReportDetail key={selectedId + ':' + revision.detail} {...access} id={selectedId} />}
    {visitedList && <div hidden={selectedId !== undefined}><ReportList key={revision.list} {...access} leaving={leaving} /></div>}
  </>
}

export function LookupShare(access: AccessProps) {
  const [target, setTarget] = useState<Target | null>(null)
  const t = useCopy(networkCopy)
  const lookup = useAction()
  return <div className="view editor"><BackLink href="#reports">{t.backToReports}</BackLink><h1>{t.shareRestrictions}</h1>
    <ValidatedForm className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const text = String(new FormData(event.currentTarget).get('id'))
      void lookup.run(async () => setTarget({ kind: 'share', id: requireIdentity(text, 'share') }))
    }}><div className="field"><label htmlFor="target-id">{t.shareId}</label>
        <input id="target-id" name="id" required autoCapitalize="none" spellCheck={false} onChange={() => setTarget(null)} /></div>
      <Notice feedback={lookup.feedback} /><div><button>{t.lookupShare}</button></div>
    </ValidatedForm>
    {target && <section className="panel padded"><code>{encode(target.id)}</code>
      <DirectRestriction key={encode(target.id)} {...access} target={target} blocked={directlyRestricted(access.restrictions, target)} /></section>}
    <RestrictionList kind="share" {...access} />
  </div>
}

function RestrictionList({ kind, api, restrictions, saved }: AccessProps & { kind: Target['kind'] }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const accounts = kind === 'account'
  const entries = accounts ? restrictions.accounts : restrictions.shares
  function remove(id: number[]) {
    void action.run(async () => {
      saved(await api.restrict({ kind, id }, false))
      return message(networkCopy, accounts ? 'accountAllowed' : 'shareAllowed')
    })
  }
  function copy(id: string) {
    void action.run(async () => {
      try { await navigator.clipboard.writeText(id) }
      catch { throw new MessageError(message(networkCopy, 'copyFailed')) }
      return message(networkCopy, 'shareIdCopied')
    })
  }
  return <section className="panel padded">
    <div className="section-title"><h2>{accounts ? t.restrictedAccounts : t.restrictedShares}</h2><Badge>{entries.length}</Badge></div>
    <Notice feedback={action.feedback} />
    {entries.length === 0 ? <p>{accounts ? t.noRestrictedAccounts : t.noRestrictedShares}</p> :
      <ul className="identity-list">{entries.map(bytes => {
        const id = encode(bytes)
        return <li key={id}>{accounts ? <a href={'#account/' + id}><code>{id}</code></a> : <code>{id}</code>}
          <div className="actions">{!accounts && <button type="button" className="quiet" title={t.copyShareId} aria-label={t.copyShareId + ' ' + id}
            disabled={action.busy} onClick={() => copy(id)}><Icon name="copy" size={16} /></button>}
            <button type="button" className="secondary" disabled={action.busy} aria-label={t.removeRestriction + ' ' + id}
              onClick={() => remove(bytes)}>{t.removeRestriction}</button></div></li>
      })}</ul>}
  </section>
}
