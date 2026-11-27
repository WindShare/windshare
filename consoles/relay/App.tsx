import { Contribution } from './contribution'
import { ManagedTURN } from './managed-turn'
import { useMemo, useState } from 'react'
import { useAction, useResource, type Feedback } from '../ui/activity'
import { ChangePasswordDialog, Login } from '../ui/forms'
import { Icon } from '../ui/Icon'
import './relay.css'
import { Frame, Notice, SessionWait } from '../ui/layout'
import { message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import { usePage } from '../ui/location'
import { RelayClient, type Capabilities } from './api'
import { Credentials } from './credentials'
import { ServiceKey } from './service-key'
import { useServiceKeyCreation } from './use-service-key-creation'
import { CodeEntry, Pairing } from './pairing'

function RelayContent({ api, session, capabilities, code, token, expired, clearToken, resetSession }: {
  api: RelayClient
  session: ReturnType<typeof useResource<Capabilities | 'uninitialized' | null>>
  capabilities: Capabilities | 'uninitialized' | null | undefined
  code: string
  token: string | null
  expired: boolean
  clearToken: () => void
  resetSession: () => void
}) {
  const t = useCopy(relayCopy)
  const page = usePage()
  // The signed-in console owns delivery of the key across route changes.
  const keyCreation = useServiceKeyCreation(api)
  if (capabilities === undefined) return <SessionWait error={session.error} retry={session.reload} />
  if (capabilities === 'uninitialized' && !token) return <section className="panel"><h1>{t.owner}</h1><p>{t.initializeAtNode}</p></section>
  if (capabilities === null || capabilities === 'uninitialized') {
    return <Login area={code ? t.pairing : t.owner} initialize={!!token} expired={expired} cancelSetup={clearToken}
      submit={async password => { await api.signIn(password, token); resetSession() }} />
  }
  if (code) return <Pairing api={api} code={code} />
  if (page === 'service-key') return <ServiceKey creation={keyCreation} />
  return <div className="workspace">
    <div className="view"><div className="page-title"><h1>{t.access}</h1></div>
      <CodeEntry /><Credentials api={api} capabilities={capabilities} keyPhase={keyCreation.state.phase}
        keyFeedback={keyCreation.feedback} />
      {capabilities.contribution && <Contribution api={api} />}
      {capabilities.managed_turn && <ManagedTURN api={api} />}</div>
  </div>
}

function Console({ api, code, token, expired, clearToken, resetSession }: {
  api: RelayClient; code: string; token: string | null; expired: boolean; clearToken: () => void; resetSession: () => void
}) {
  const session = useResource(api.session)
  const logout = useAction()
  const [changingPassword, setChangingPassword] = useState(false)
  const [passwordNotice, setPasswordNotice] = useState<Feedback | null>(null)
  const t = useCopy(relayCopy)
  const shared = useCopy(sharedCopy)
  const capabilities = session.value
  const tools = capabilities && capabilities !== 'uninitialized' && <div className="mast-account">
    {capabilities.password_change && <button type="button" className="quiet" aria-label={shared.changePassword}
      title={shared.changePassword} disabled={logout.busy} onClick={() => { setPasswordNotice(null); setChangingPassword(true) }}>
      <Icon name="key" size={16} /><span>{shared.changePassword}</span></button>}
    <button type="button" className="quiet" aria-label={shared.signOut} title={shared.signOut} disabled={logout.busy}
      onClick={() => { setPasswordNotice(null); void logout.run(async () => { await api.signOut(); resetSession() }) }}>
      <Icon name="logout" size={16} /><span>{shared.signOut}</span></button>
  </div>
  return <Frame area={t.title} tools={tools}>
    <Notice feedback={logout.feedback ?? passwordNotice} />
    <RelayContent api={api} session={session} capabilities={capabilities} code={code}
      token={token} expired={expired} clearToken={clearToken} resetSession={resetSession} />
    {changingPassword && <ChangePasswordDialog onCancel={() => setChangingPassword(false)}
      onSubmit={async (current, next) => {
        await api.changePassword(current, next)
        setChangingPassword(false)
        setPasswordNotice({ kind: 'success', message: message(sharedCopy, 'passwordUpdated') })
      }} />}
  </Frame>
}

export function App({ initialToken, code }: { initialToken: string | null; code: string }) {
  const [epoch, setEpoch] = useState(0)
  const [token, setToken] = useState(initialToken)
  const [sessionExpired, setSessionExpired] = useState(false)
  const api = useMemo(() => new RelayClient(() => {
    setSessionExpired(true)
    setEpoch(current => current === epoch ? current + 1 : current)
  }), [epoch])
  return <Console key={epoch} api={api} code={code} token={token} expired={sessionExpired} clearToken={() => setToken(null)}
    resetSession={() => { setSessionExpired(false); setToken(null); setEpoch(current => current + 1) }} />
}
