import { useEffect, useRef, useState } from 'react'
import { useAction, useResource, type Feedback } from '../ui/activity'
import { message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import { Confirm } from '../ui/forms'
import { Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { Icon } from '../ui/Icon'
import type { Capabilities, Credential, CredentialPage, RelayClient } from './api'
import { ServiceKeyEntry } from './service-key'
import type { ServiceKeyPhase } from './use-service-key-creation'

function withoutCredential(page: CredentialPage, id: string): CredentialPage {
  return { ...page, credentials: page.credentials.filter(item => item.credential_id !== id) }
}

interface InventoryProps {
  api: RelayClient
  capabilities: Capabilities
  keyPhase: ServiceKeyPhase
  keyFeedback: Feedback | null
}

export function Credentials(props: InventoryProps) {
  const [history, setHistory] = useState(false)
  return <CredentialInventory key={String(history)} {...props} history={history}
    switchInventory={() => setHistory(current => !current)} />
}

function CredentialInventory({ api, capabilities, keyPhase, keyFeedback, history, switchInventory }: InventoryProps & {
  history: boolean; switchInventory: () => void
}) {
  const t = useCopy(relayCopy)
  const shared = useCopy(sharedCopy)
  const list = useResource(history ? api.revokedCredentials : api.activeCredentials)
  const { reload } = list
  const keyWasReady = useRef(keyPhase === 'ready')
  useEffect(() => {
    const keyReady = keyPhase === 'ready'
    // An active-list read may finish before the creation started on the other route commits.
    if (!history && keyReady && !keyWasReady.current) reload()
    keyWasReady.current = keyReady
  }, [history, keyPhase, reload])
  const page = list.value
  const cursor = page?.next_cursor
  const action = useAction()
  const [selected, setSelected] = useState<Credential | null>(null)
  const [stop, setStop] = useState(false)
  const forced = capabilities.revocation_stops_shares === true
  const stopping = selected != null && selected.revoked_at != null
  const confirm = stopping ? t.stopShares : t.revokeAccess
  function revokeSelected(target: Credential) {
    setSelected(null)
    void action.run(async () => {
      const alreadyRevoked = target.revoked_at != null
      await api.revoke(target.credential_id, forced || alreadyRevoked || stop)
      if (!alreadyRevoked) list.update(current => withoutCredential(current, target.credential_id))
      return message(relayCopy, alreadyRevoked ? 'stoppingShares' : 'credentialRevoked')
    })
  }
  return <section className="panel inventory" aria-labelledby="credentials-title">
    <div className="section-title"><h2 id="credentials-title">{history ? t.credentialHistory : t.credentials}</h2>
      <div className="actions"><button type="button" className="quiet" disabled={action.busy} onClick={switchInventory}>
        {history ? t.credentials : t.credentialHistory}</button>
        <button type="button" className="quiet icon-button" aria-label={shared.refresh} title={shared.refresh}
          disabled={list.busy || action.busy} onClick={list.reload}><Icon name={list.busy ? 'loader' : 'refresh'} size={17} /></button>
        {(!history || keyPhase !== 'idle') && <ServiceKeyEntry phase={keyPhase} />}</div></div>
    <Notice feedback={keyFeedback} /><Notice feedback={action.feedback} /><ReadError error={list.error} retry={list.retry} />
    {!page && list.busy && <p className="pending" role="status"><Icon name="loader" />{t.loadingCredentials}</p>}
    {page?.credentials.length === 0 && <EmptyState icon="key" title={history ? t.noRevokedCredentials : t.noCredentials}>
      {!history && <p>{t.grantAccess}</p>}</EmptyState>}
    {page && page.credentials.length > 0 && <table className="credential-table" aria-labelledby="credentials-title">
      <thead><tr><th scope="col">{t.credentialName}</th><th scope="col">{t.lastUsed}</th>
        <th scope="col">{history ? t.revokedAt : t.status}</th><th scope="col"><span className="sr-only">{t.actions}</span></th></tr></thead>
      <tbody>{page.credentials.map(item => <tr key={item.credential_id}>
        <th scope="row"><div className="credential-name"><span className="row-icon"><Icon name="key" size={19} /></span>{item.name}</div></th>
        <td className="credential-used">{item.last_used_at == null ? t.neverUsed : <>
          <span className="mobile-label">{t.lastUsed} </span><DateStamp millis={item.last_used_at} /></>}</td>
        <td className="credential-status">{item.revoked_at == null ? <Badge tone="good">{t.active}</Badge> : <>
          <span className="mobile-label">{t.revokedAt} </span><DateStamp millis={item.revoked_at} /></>}</td>
        <td className="credential-action">{(!history || !forced) && <button type="button" className="quiet danger-text" disabled={action.busy}
          aria-label={history ? t.stopNamed(item.name) : t.revokeNamed(item.name)}
          onClick={() => { setStop(false); setSelected(item) }}>{history ? t.stopShares : t.revoke}</button>}</td>
      </tr>)}</tbody>
    </table>}
    {cursor && <div className="credential-pagination"><button type="button" className="secondary" disabled={list.busy}
      onClick={() => list.extend(signal => api.revokedCredentials(signal, cursor), (previous, next) => ({
        ...next, credentials: [...previous.credentials, ...next.credentials],
      }))}>{list.busy ? shared.loadingMore : shared.loadMore}</button></div>}
    {selected && <Confirm title={stopping ? t.confirmStop : t.confirmRevoke(selected.name)} confirm={confirm}
      onCancel={() => setSelected(null)} onConfirm={() => revokeSelected(selected)}>
      {forced || stopping
        ? <p>{t.directContinues}</p>
        : <><p>{t.revokeConsequences}</p>
          <label className="check" htmlFor="stop-shares"><input id="stop-shares" type="checkbox" checked={stop}
            onChange={event => setStop(event.target.checked)} />{t.stopExisting}</label></>}
    </Confirm>}
  </section>
}
