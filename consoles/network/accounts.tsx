import { useCallback, useEffect, useRef } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Icon } from '../ui/Icon'
import { BackLink, Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { sharedCopy } from '../ui/locales'
import { useCopy, useLocale } from '../ui/use-locale'
import { ValidatedForm } from '../ui/ValidatedForm'
import type { AuthorityClient } from './api'
import { decode, encode } from './identity'
import { networkCopy } from './locales'
import { identityErrors } from './locales/identity'
import type { AccountDetails, AccountPage, Restrictions, Snapshot } from './model'
import { DirectRestriction, type AccessProps } from './restrictions'

const DAY_MS = 86_400_000
const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB']

function utcDate(day: number) {
  return new Date(day * DAY_MS).toISOString().slice(0, 10)
}

function Bytes({ value }: { value: number }) {
  const { locale } = useLocale()
  const unit = Math.min(BYTE_UNITS.length - 1, Math.floor(Math.log2(Math.max(1, value)) / 10))
  const amount = (value / 1024 ** unit).toLocaleString(locale, { maximumFractionDigits: 2 })
  return <span className="byte-value" title={networkCopy[locale].exactBytes(value)}>{amount} {BYTE_UNITS[unit]}</span>
}

function AccountOverview({ snapshot }: { snapshot: Snapshot }) {
  const { locale } = useLocale()
  const t = networkCopy[locale]
  const metrics = [
    { icon: 'user', label: t.registeredAccounts, value: snapshot.account_count.toLocaleString(locale) },
    { icon: 'clock', label: t.dailyAllowance, value: <Bytes value={snapshot.settings.daily_bytes} /> },
    { icon: 'shield', label: t.restrictedAccounts, value: snapshot.restrictions.accounts.length.toLocaleString(locale) },
  ] as const
  return <dl className="panel node-overview">{metrics.map(metric => <div key={metric.icon}>
    <dt><span className="metric-icon"><Icon name={metric.icon} /></span>{metric.label}</dt><dd>{metric.value}</dd>
  </div>)}</dl>
}

function AccountLookup({ api }: { api: AuthorityClient }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const active = useRef<AbortController | null>(null)
  useEffect(() => () => active.current?.abort(), [])
  return <ValidatedForm className="panel padded stack" onSubmit={event => {
    event.preventDefault()
    const query = String(new FormData(event.currentTarget).get('query'))
    void action.run(async () => {
      const controller = new AbortController()
      active.current = controller
      const account = await api.lookupAccount(query, controller.signal)
      if (!controller.signal.aborted) location.hash = 'account/' + encode(account.id)
    })
  }}>
    <div className="field"><label htmlFor="account-query">{t.queryAccount}</label>
      <div className="account-search"><input name="query" id="account-query" required disabled={action.busy}
        autoCapitalize="none" spellCheck={false} /><button disabled={action.busy}>
        <Icon name={action.busy ? 'loader' : 'search'} size={16} />{t.lookupAccount}</button></div></div>
    <Notice feedback={action.feedback} />
  </ValidatedForm>
}

function AccountsTable({ page, limit, restrictions }: { page: AccountPage; limit: number; restrictions: Restrictions }) {
  const t = useCopy(networkCopy)
  const restricted = new Set(restrictions.accounts.map(encode))
  const usage = t.relayed + ' · ' + utcDate(page.today_day) + ' UTC'
  return <div className="table-wrap panel" role="region" aria-label={t.registeredAccounts} tabIndex={0}>
    <table><caption className="sr-only">{t.registeredAccounts}</caption>
      <thead><tr>{[t.account, t.registeredAt, usage, t.status].map(label => <th key={label} scope="col">{label}</th>)}</tr></thead>
      <tbody>{page.entries.map(entry => {
        const id = encode(entry.id)
        const name = entry.username || entry.nickname
        const blocked = restricted.has(id)
        return <tr key={id}><th scope="row"><a href={'#account/' + id} className="node-link" title={id}>
          {name ? <span>{name}<small><code>{id.slice(0, 10)}…</code></small></span> : <code>{id}</code>}</a></th>
          <td data-label={t.registeredAt}><DateStamp millis={entry.created_at * 1000} /></td>
          <td data-label={usage}><div><Bytes value={entry.today_bytes} /> / <Bytes value={limit} />
            {entry.today_bytes >= limit && <small><Badge tone="warning">{t.exhausted}</Badge></small>}</div></td>
          <td data-label={t.status}><div><Badge tone={blocked ? 'warning' : 'good'}>{blocked ? t.restricted : t.normal}</Badge></div></td>
        </tr>
      })}</tbody>
    </table></div>
}

export function Accounts({ api, snapshot }: { api: AuthorityClient; snapshot: Snapshot }) {
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const list = useResource(api.accounts)
  const page = list.value
  function loadMore() {
    const cursor = page?.next_cursor
    if (!cursor) return
    list.extend(async signal => {
      const next = await api.accounts(signal, cursor)
      // A single "today" column cannot mix measurements from different UTC days.
      return next.today_day === page.today_day ? next : api.accounts(signal)
    }, (previous, next) => previous.today_day === next.today_day
      ? { ...next, entries: [...previous.entries, ...next.entries] } : next)
  }
  return <div className="view"><h1>{t.accounts}</h1><AccountOverview snapshot={snapshot} /><AccountLookup api={api} />
    <ReadError error={list.error} retry={list.retry} />
    {!page && list.busy && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingAccounts}</p>}
    {page && (page.entries.length ? <AccountsTable page={page} limit={snapshot.settings.daily_bytes} restrictions={snapshot.restrictions} />
      : <div className="panel"><EmptyState icon="user" title={t.noAccounts} /></div>)}
    {page?.next_cursor && <div className="actions"><button className="secondary" disabled={list.busy}
      onClick={loadMore}>{list.busy ? shared.loadingMore : shared.loadMore}</button></div>}
  </div>
}

function Usage({ account }: { account: AccountDetails }) {
  const t = useCopy(networkCopy)
  return <section className="panel padded"><div className="section-title"><h2>{t.dailyUsage}</h2>
    <small>{utcDate(account.today_day)} UTC</small></div>
    <dl className="facts"><dt>{t.used}</dt><dd><Bytes value={account.today_bytes} /></dd>
      <dt>{t.dailyLimit}</dt><dd><Bytes value={account.daily_limit_bytes} /></dd>
      <dt>{t.remaining}</dt><dd><Bytes value={account.remaining_bytes} /></dd></dl>
    {account.recent_usage.length === 0 ? <p>{t.noUsage}</p> : <dl className="usage-days">{account.recent_usage.map(({ day, bytes }) =>
      <div key={day}><dt><time dateTime={utcDate(day)}>{utcDate(day)} UTC</time></dt><dd><Bytes value={bytes} /></dd></div>)}</dl>}
    {account.has_older_usage && <p>{t.recentUsageLimit}</p>}
  </section>
}

function Shares({ account, restrictions }: { account: AccountDetails; restrictions: Restrictions }) {
  const t = useCopy(networkCopy)
  const direct = new Set(restrictions.shares.map(encode))
  const id = encode(account.id)
  const inherited = restrictions.accounts.some(value => encode(value) === id)
  return <section className="panel padded"><h2>{t.attributedShares}</h2>
    {account.shares.length === 0 ? <p>{t.noShares}</p> : <ul className="identity-list">{account.shares.map(encode).map(share =>
      <li key={share}><code>{share}</code><div className="actions">
        {direct.has(share) && <Badge tone="warning">{t.directlyRestricted}</Badge>}
        {inherited && <Badge tone="warning">{t.viaAccount}</Badge>}</div></li>)}</ul>}
    {account.has_more_shares && <p>{t.shareLimit}</p>}
  </section>
}

export function AccountDetail({ id, ...access }: AccessProps & { id: string }) {
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const invalid = useCopy(identityErrors)
  const { api } = access
  const read = useCallback((signal: AbortSignal) => api.account(id, signal), [api, id])
  const resource = useResource(read)
  const account = resource.value
  const bytes = decode(id, 'account')
  return <div className="view"><BackLink href="#accounts">{t.backToAccounts}</BackLink>
    <h1>{account?.username || account?.nickname || t.accountDetails}</h1>
    <ReadError error={resource.error} retry={resource.retry} />
    {resource.busy && !account && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingAccount}</p>}
    {account === null && <section className="panel padded"><h2>{bytes ? t.accountNotFound : invalid.account}</h2>
      {bytes && <><code>{id}</code><DirectRestriction {...access} target={{ kind: 'account', id: bytes }} /></>}</section>}
    {account && <><div className="node-detail-grid"><section className="panel padded"><h2>{t.profile}</h2>
      <dl className="facts"><dt>{t.accountId}</dt><dd><code>{encode(account.id)}</code></dd>
        <dt>{shared.username}</dt><dd>{account.username ?? '—'}</dd><dt>{t.nickname}</dt><dd>{account.nickname ?? '—'}</dd>
        <dt>{t.verifiedEmail}</dt><dd>{account.email ?? '—'}</dd><dt>{t.passwordSet}</dt><dd>{account.has_password ? t.yes : t.no}</dd>
        <dt>{t.registeredAt}</dt><dd><DateStamp millis={account.created_at * 1000} /></dd>
        <dt>{t.entitlementVersion}</dt><dd>{account.entitlement_version}</dd></dl>
      <DirectRestriction {...access} target={{ kind: 'account', id: account.id }} /></section>
      <Usage account={account} /></div><Shares account={account} restrictions={access.restrictions} /></>}
  </div>
}
