import { useMemo, useState } from 'react'
import { useAction, useResource, type Feedback } from '../ui/activity'
import { ChangePasswordDialog, Login } from '../ui/forms'
import { Icon } from '../ui/Icon'
import { WorkspaceNav } from '../ui/chrome/WorkspaceNav'
import { Frame, Notice, ReadError, SessionWait } from '../ui/layout'
import { message, type Message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { networkCopy } from './locales'
import { usePage } from '../ui/location'
import { AuthorityClient } from './api'
import { EnrollNode } from './onboarding'
import { encode } from './identity'
import type { NodeDetails, RelayNode, Restrictions, Snapshot } from './model'
import { LookupNode, NodeDetail, Nodes } from './nodes'
import { Accounts } from './accounts'
import { LookupShare, Reports } from './restrictions'
import { usePolicyEditor } from './policy-editor'
import { PolicySettings } from './settings'

function mergeNodeEntries(current: NodeDetails[], received: NodeDetails[]) {
  const byKey = new Map(current.map(entry => [encode(entry.key), entry]))
  for (const entry of received) {
    const key = encode(entry.key)
    const cached = byKey.get(key)
    byKey.set(key, cached && cached.version > entry.version ? { ...cached, report: entry.report } : entry)
  }
  return [...byKey.values()]
}

function Dashboard({ api, snapshot, update, refresh, loadMoreNodes, retry, refreshing, error }: {
  api: AuthorityClient; snapshot: Snapshot; update: (merge: (current: Snapshot) => Snapshot) => void
  refresh: () => Promise<void>; loadMoreNodes: () => void; retry: () => void; refreshing: boolean; error: Message | null
}) {
  const t = useCopy(networkCopy)
  const [page, id] = usePage().split('/')
  const directory = snapshot.directory
  const editor = usePolicyEditor(snapshot.settings)
  const policy = page === 'policy'
  const accounts = page === 'accounts' || page === 'account'
  const reports = ['reports', 'report', 'share-access'].includes(page)
  const [accountRevision, setAccountRevision] = useState({ list: 0, detail: 0 })
  const [reportRevision, setReportRevision] = useState({ list: 0, detail: 0 })
  const [nodeRevision, setNodeRevision] = useState(0)

  function saveNode(next: RelayNode) {
    const key = encode(next.key)
    update(current => ({ ...current, node_page: { ...current.node_page,
      entries: current.node_page.entries.map(entry => encode(entry.key) === key && next.version >= entry.version
        ? { ...entry, ...next } : entry),
    } }))
  }

  function enrollNode(next: NodeDetails) {
    update(current => ({ ...current, node_page: { ...current.node_page,
      entries: mergeNodeEntries(current.node_page.entries, [next]),
    } }))
  }

  const access = {
    api, restrictions: snapshot.restrictions,
    saved: (restrictions: Restrictions) => update(current => restrictions.version < current.restrictions.version
      ? current : { ...current, restrictions }),
  }

  function refreshCurrent(): Promise<void> {
    if (page === 'accounts') setAccountRevision(current => ({ ...current, list: current.list + 1 }))
    if (page === 'account') setAccountRevision(current => ({ ...current, detail: current.detail + 1 }))
    if (page === 'node') setNodeRevision(current => current + 1)
    if (page === 'reports') setReportRevision(current => ({ ...current, list: current.list + 1 }))
    if (page === 'report') setReportRevision(current => ({ ...current, detail: current.detail + 1 }))
    return refresh()
  }

  let view
  if (policy) {
    view = <PolicySettings api={api} snapshot={snapshot} editor={editor}
      saved={({ settings, registration_mode }) => update(current => ({ ...current, settings, registration_mode }))}
      reviewsSaved={reviews => update(current => ({ ...current, reviews }))} />
  } else if (page === 'enroll') {
    view = <EnrollNode api={api} saved={enrollNode} setup={directory?.setup} />
  } else if (page === 'node') {
    const cached = snapshot.node_page.entries.find(entry => encode(entry.key) === id)
    view = cached ? <NodeDetail key={id} node={cached} api={api} saved={saveNode} directory={directory} refresh={refreshCurrent} />
      : <LookupNode key={id + ':' + nodeRevision} id={id ?? ''} api={api} saved={saveNode} directory={directory} refresh={refreshCurrent} />
  } else if (accounts) {
    view = <Accounts {...access} snapshot={snapshot} revision={accountRevision} refresh={refresh} selectedId={page === 'account' ? id ?? '' : undefined} />
  } else if (page === 'share-access') {
    view = <LookupShare {...access} />
  } else if (reports) {
    view = <Reports {...access} revision={reportRevision} selectedId={page === 'report' ? id : undefined} />
  } else {
    view = <Nodes page={snapshot.node_page} directory={directory} loading={refreshing} loadMore={loadMoreNodes} />
  }
  const refreshLabel = refreshing ? t.refreshing : t.refreshSnapshot
  return <div className="workspace">
    <WorkspaceNav label={t.management} destinations={[
      { href: '#nodes', label: t.nodes, icon: 'server', current: !reports && !policy && !accounts },
      { href: '#accounts', label: t.accounts, icon: 'user', current: accounts },
      { href: '#reports', label: t.reports, icon: 'report', current: reports },
      { href: '#policy', label: t.policy, icon: 'policy', current: policy },
    ]} action={<button className="quiet" onClick={() => { void refreshCurrent() }} disabled={refreshing} aria-label={refreshLabel} title={refreshLabel}>
      <Icon name={refreshing ? 'loader' : 'refresh'} size={16} /><span className="action-text">{refreshLabel}</span></button>} />
    <ReadError error={error} retry={retry} />
    {view}
  </div>
}

function ConsoleBody({ api, state, token, expired, clearToken, resetSession }: {
  api: AuthorityClient
  state: ReturnType<typeof useResource<Snapshot | null>>
  token: string | null
  expired: boolean
  clearToken: () => void
  resetSession: () => void
}) {
  const t = useCopy(networkCopy)
  if (state.value === undefined) return <SessionWait error={state.error} retry={state.retry} />
  if (state.value === null) {
    return <Login area={t.administrator} username initialize={!!token} expired={expired} cancelSetup={clearToken}
      submit={async (password, name) => { await api.signIn(name, password, token); resetSession() }} />
  }
  const snapshot = state.value
  function loadMoreNodes() {
    const cursor = snapshot.node_page.next_cursor
    if (!cursor) return
    state.extend(async signal => ({ ...snapshot, node_page: await api.nodes(cursor, signal) }), (current, next) => {
      if (!current || !next) return next
      return { ...current, node_page: { ...next.node_page,
        entries: mergeNodeEntries(current.node_page.entries, next.node_page.entries),
      } }
    })
  }
  return <Dashboard api={api} snapshot={snapshot} refreshing={state.busy} refresh={state.reload} error={state.error}
    loadMoreNodes={loadMoreNodes} retry={state.retry} update={merge => state.update(previous => previous && merge(previous))} />
}

function Console({ api, token, expired, clearToken, resetSession }: {
  api: AuthorityClient; token: string | null; expired: boolean; clearToken: () => void; resetSession: () => void
}) {
  const state = useResource(api.snapshot)
  const logout = useAction()
  const [changingPassword, setChangingPassword] = useState(false)
  const [passwordNotice, setPasswordNotice] = useState<Feedback | null>(null)
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const tools = state.value && <div className="mast-account">
    <button type="button" className="quiet" aria-label={shared.changePassword} title={shared.changePassword}
      disabled={logout.busy} onClick={() => { setPasswordNotice(null); setChangingPassword(true) }}>
      <Icon name="key" size={16} /><span>{shared.changePassword}</span></button>
    <button type="button" className="quiet" aria-label={shared.signOut} title={shared.signOut} disabled={logout.busy}
      onClick={() => { setPasswordNotice(null); void logout.run(async () => { await api.signOut(); resetSession() }) }}>
      <Icon name="logout" size={16} /><span>{shared.signOut}</span></button>
  </div>
  return <Frame area={t.title} tools={tools}>
    <Notice feedback={logout.feedback ?? passwordNotice} />
    <ConsoleBody api={api} state={state} token={token} expired={expired} clearToken={clearToken} resetSession={resetSession} />
    {changingPassword && <ChangePasswordDialog onCancel={() => setChangingPassword(false)}
      onSubmit={async (current, next) => {
        await api.changePassword(current, next)
        setChangingPassword(false)
        setPasswordNotice({ kind: 'success', message: message(sharedCopy, 'passwordUpdated') })
      }} />}
  </Frame>
}

export function App({ initialToken }: { initialToken: string | null }) {
  const [epoch, setEpoch] = useState(0)
  const [token, setToken] = useState(initialToken)
  const [sessionExpired, setSessionExpired] = useState(false)
  const api = useMemo(() => new AuthorityClient(() => {
    setSessionExpired(true)
    setEpoch(current => current === epoch ? current + 1 : current)
  }), [epoch])
  return <Console key={epoch} api={api} token={token} expired={sessionExpired} clearToken={() => setToken(null)}
    resetSession={() => { setSessionExpired(false); setToken(null); setEpoch(current => current + 1) }} />
}
