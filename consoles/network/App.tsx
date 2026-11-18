import { useMemo, useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Login } from '../ui/forms'
import { Frame, Notice, ReadError, SessionWait } from '../ui/layout'
import { usePage } from '../ui/location'
import { AuthorityClient } from './api'
import { useDirectory } from './directory'
import { encode } from './identity'
import type { RelayNode, Restrictions, Snapshot } from './model'
import { EnrollNode, NodeDetail, Nodes } from './nodes'
import { LookupAccess, Reports } from './restrictions'
import { usePolicyEditor } from './policy-editor'
import { PolicySettings } from './settings'

function upsertNode(nodes: RelayNode[], next: RelayNode): RelayNode[] {
  const key = encode(next.key)
  const existing = nodes.find(item => encode(item.key) === key)
  if (existing && existing.version > next.version) return nodes
  if (existing) return nodes.map(item => item === existing ? next : item)
  return [...nodes, next]
}

function DashboardView({ api, snapshot, page, id, reports, directory, saveNode, access }: {
  api: AuthorityClient
  snapshot: Snapshot
  page: string
  id: string | undefined
  reports: boolean
  directory: ReturnType<typeof useDirectory>
  saveNode: (next: RelayNode) => void
  access: { api: AuthorityClient; restrictions: Restrictions; saved: (restrictions: Restrictions) => void }
}) {
  if (page === 'enroll') return <EnrollNode api={api} saved={saveNode} />
  const node = page === 'node' ? snapshot.nodes.find(item => encode(item.key) === id) : undefined
  if (node) {
    return <NodeDetail key={id} node={node} api={api} saved={saveNode}
      report={snapshot.load.find(item => item.node === id)} directory={directory} />
  }
  if (page === 'account-access' || page === 'share-access') {
    return <LookupAccess key={page} kind={page === 'account-access' ? 'account' : 'share'} {...access} />
  }
  if (reports) return <Reports {...access} reports={snapshot.reports} selectedId={page === 'report' ? id : undefined} />
  return <Nodes nodes={snapshot.nodes} reports={snapshot.load} directory={directory} />
}

function Dashboard({ api, snapshot, update, refresh, refreshing, error }: {
  api: AuthorityClient; snapshot: Snapshot; update: (merge: (current: Snapshot) => Snapshot) => void
  refresh: () => void; refreshing: boolean; error: string
}) {
  const [page, id] = usePage().split('/')
  const directory = useDirectory(snapshot.directory)
  const editor = usePolicyEditor()
  const policy = page === 'policy'
  const reports = ['reports', 'report', 'account-access', 'share-access'].includes(page)
  const saveNode = (next: RelayNode) => update(current => ({ ...current, nodes: upsertNode(current.nodes, next) }))
  const access = {
    api, restrictions: snapshot.restrictions,
    saved: (restrictions: Restrictions) => update(current => restrictions.version < current.restrictions.version
      ? current : { ...current, restrictions }),
  }
  return <><nav className="tabs" aria-label="Network management"><div>
    <a href="#nodes" aria-current={!reports && !policy ? 'page' : undefined}>Nodes</a>
    <a href="#reports" aria-current={reports ? 'page' : undefined}>Reports</a>
    <a href="#policy" aria-current={policy ? 'page' : undefined}>Policy</a>
    <button className="quiet refresh-snapshot" onClick={refresh} disabled={refreshing}>{refreshing ? 'Refreshing…' : 'Refresh snapshot'}</button>
  </div></nav>
    <div className="workspace">
      <ReadError error={error} retry={refresh} />
      {policy ? <PolicySettings api={api} snapshot={snapshot} editor={editor} saved={settings => update(current => ({ ...current, settings }))} />
        : <DashboardView api={api} snapshot={snapshot} page={page} id={id} reports={reports}
          directory={directory} saveNode={saveNode} access={access} />}
    </div></>
}

function ConsoleBody({ api, state, token, clearToken, resetSession }: {
  api: AuthorityClient
  state: ReturnType<typeof useResource<Snapshot | null>>
  token: string | null
  clearToken: () => void
  resetSession: () => void
}) {
  if (state.value === undefined) return <SessionWait error={state.error} retry={state.reload} />
  if (state.value === null) {
    return <Login area="Network administrator" username initialize={!!token} cancelSetup={clearToken}
      submit={async (password, name) => { await api.signIn(name, password, token); resetSession() }} />
  }
  return <Dashboard api={api} snapshot={state.value} refreshing={state.busy} refresh={state.reload} error={state.error}
    update={merge => state.update(previous => previous && merge(previous))} />
}

function Console({ api, token, clearToken, resetSession }: {
  api: AuthorityClient; token: string | null; clearToken: () => void; resetSession: () => void
}) {
  const state = useResource(api.snapshot)
  const logout = useAction()
  return <Frame area="Network console" tools={state.value && <button className="quiet" disabled={logout.busy}
    onClick={() => void logout.run(async () => { await api.signOut(); resetSession() })}>Sign out</button>}>
    <Notice feedback={logout.feedback} />
    <ConsoleBody api={api} state={state} token={token} clearToken={clearToken} resetSession={resetSession} />
  </Frame>
}

export function App({ initialToken }: { initialToken: string | null }) {
  const [epoch, setEpoch] = useState(0)
  const [token, setToken] = useState(initialToken)
  const api = useMemo(() => new AuthorityClient(() => setEpoch(current => current === epoch ? current + 1 : current)), [epoch])
  return <Console key={epoch} api={api} token={token} clearToken={() => setToken(null)}
    resetSession={() => { setToken(null); setEpoch(epoch + 1) }} />
}
