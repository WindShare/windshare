import { useMemo, useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Login } from '../ui/forms'
import { Frame, Notice, SessionWait } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import { usePage } from '../ui/location'
import { RelayClient, type Capabilities } from './api'
import { Credentials, ServiceKey } from './credentials'
import { CodeEntry, Pairing } from './pairing'
import { ManagedTurn } from './turn'

function RelayContent({ api, session, capabilities, code, token, clearToken, resetSession }: {
  api: RelayClient
  session: ReturnType<typeof useResource<Capabilities | null>>
  capabilities: Capabilities | null | undefined
  code: string
  token: string | null
  clearToken: () => void
  resetSession: () => void
}) {
  const t = useCopy(relayCopy)
  const page = usePage()
  if (capabilities === undefined) return <SessionWait error={session.error} retry={session.reload} />
  if (capabilities === null) {
    return <Login area={code ? t.pairing : t.owner} initialize={!!token} cancelSetup={clearToken}
      submit={async password => { await api.signIn(password, token); resetSession() }} />
  }
  if (code) return <Pairing api={api} code={code} />
  if (page === 'service-key') return <ServiceKey api={api} />
  const turn = page === 'turn' && capabilities.managed_turn
  return <>
    {capabilities.managed_turn && <nav className="tabs" aria-label={t.management}><div>
      <a href="#access" aria-current={!turn ? 'page' : undefined}>{t.access}</a>
      <a href="#turn" aria-current={turn ? 'page' : undefined}>{t.managedTurn}</a>
    </div></nav>}
    {turn
      ? <ManagedTurn api={api} />
      : <div className="workspace narrow"><div className="page-title"><h1>{t.access}</h1></div>
        <CodeEntry /><Credentials api={api} capabilities={capabilities} /></div>}
  </>
}

function Console({ api, code, token, clearToken, resetSession }: {
  api: RelayClient; code: string; token: string | null; clearToken: () => void; resetSession: () => void
}) {
  const session = useResource(api.session)
  const logout = useAction()
  const t = useCopy(relayCopy)
  const shared = useCopy(sharedCopy)
  const capabilities = session.value
  return <Frame area={t.title} tools={capabilities && <button className="quiet" disabled={logout.busy}
    onClick={() => void logout.run(async () => { await api.signOut(); resetSession() })}>{shared.signOut}</button>}>
    <Notice feedback={logout.feedback} />
    <RelayContent api={api} session={session} capabilities={capabilities} code={code}
      token={token} clearToken={clearToken} resetSession={resetSession} />
  </Frame>
}

export function App({ initialToken, code }: { initialToken: string | null; code: string }) {
  const [epoch, setEpoch] = useState(0)
  const [token, setToken] = useState(initialToken)
  const api = useMemo(() => new RelayClient(() => setEpoch(current => current === epoch ? current + 1 : current)), [epoch])
  return <Console key={epoch} api={api} code={code} token={token} clearToken={() => setToken(null)}
    resetSession={() => { setToken(null); setEpoch(epoch + 1) }} />
}
