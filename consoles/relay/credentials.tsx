import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import { Confirm } from '../ui/forms'
import { BackLink, Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { FocusPage } from '../ui/FocusPage'
import { Icon } from '../ui/Icon'
import type { Capabilities, Credential, RelayClient, ServiceCredential } from './api'

function markCredentialRevoked(items: Credential[], credentialId: string): Credential[] {
  return items.map(item => item.credential_id === credentialId ? { ...item, revoked: true } : item)
}

export function Credentials({ api, capabilities }: { api: RelayClient; capabilities: Capabilities }) {
  const t = useCopy(relayCopy)
  const shared = useCopy(sharedCopy)
  const list = useResource(api.credentials)
  const action = useAction()
  const [selected, setSelected] = useState<Credential | null>(null)
  const [stop, setStop] = useState(false)
  const forced = capabilities.revocation_stops_shares === true
  const confirm = selected?.revoked ? t.stopShares : t.revokeAccess
  function revokeSelected(target: Credential) {
    setSelected(null)
    void action.run(async () => {
      await api.revoke(target.credential_id, forced || target.revoked || stop)
      list.update(items => markCredentialRevoked(items, target.credential_id))
      return message(relayCopy, target.revoked ? 'stoppingShares' : 'credentialRevoked')
    })
  }
  return <section className="panel inventory" aria-labelledby="credentials-title">
    <div className="section-title"><h2 id="credentials-title">{t.credentials}</h2>
      <div className="actions"><button className="quiet icon-button" aria-label={shared.refresh} title={shared.refresh}
        disabled={list.busy} onClick={list.reload}><Icon name={list.busy ? 'loader' : 'refresh'} size={17} /></button>
        <a className="button secondary" href="#service-key"><Icon name="plus" size={16} />{t.createServiceKey}</a></div></div>
    <Notice feedback={action.feedback} /><ReadError error={list.error} retry={list.reload} />
    {list.value === undefined && list.busy && <p className="pending" role="status"><Icon name="loader" />{t.loadingCredentials}</p>}
    {list.value?.length === 0 && <EmptyState icon="key" title={t.noCredentials}><p>{t.grantAccess}</p></EmptyState>}
    {!!list.value?.length && <table className="credential-table" aria-labelledby="credentials-title">
      <thead><tr><th scope="col">{t.credentialName}</th><th scope="col">{t.lastUsed}</th>
        <th scope="col">{t.status}</th><th scope="col"><span className="sr-only">{t.actions}</span></th></tr></thead>
      <tbody>{list.value.map(item => <tr key={item.credential_id}>
        <th scope="row"><div className="credential-name"><span className="row-icon"><Icon name="key" size={19} /></span>{item.name}</div></th>
        <td className="credential-used">{item.last_used_at == null ? t.neverUsed : <>
          <span className="mobile-label">{t.lastUsed} </span><DateStamp millis={item.last_used_at} /></>}</td>
        <td className="credential-status"><Badge tone={item.revoked ? 'neutral' : 'good'}>{item.revoked ? t.revoked : t.active}</Badge></td>
        <td className="credential-action">{(!item.revoked || !forced) && <button type="button" className="quiet danger-text" disabled={action.busy}
          aria-label={item.revoked ? t.stopNamed(item.name) : t.revokeNamed(item.name)}
          onClick={() => { setStop(false); setSelected(item) }}>{item.revoked ? t.stopShares : t.revoke}</button>}</td>
      </tr>)}</tbody>
    </table>}
    {selected && <Confirm title={selected.revoked ? t.confirmStop : t.confirmRevoke(selected.name)} confirm={confirm}
      onCancel={() => setSelected(null)} onConfirm={() => revokeSelected(selected)}>
      {forced || selected.revoked
        ? <p>{t.directContinues}</p>
        : <><p>{t.revokeConsequences}</p>
          <label className="check" htmlFor="stop-shares"><input id="stop-shares" type="checkbox" checked={stop}
            onChange={event => setStop(event.target.checked)} />{t.stopExisting}</label></>}
    </Confirm>}
  </section>
}

export function ServiceKey({ api }: { api: RelayClient }) {
  const t = useCopy(relayCopy)
  const [credential, setCredential] = useState<ServiceCredential | null>(null)
  const action = useAction()
  return <FocusPage icon={credential ? 'check' : 'key'} label={t.access} title={credential ? t.keyCreated : t.createServiceKey}>
    {credential
      ? <><p className="lead">{t.keyShownOnce}</p>
        <dl className="facts"><dt>{t.relayAddress}</dt><dd><code>{credential.url}</code></dd>
          <dt>{t.accessKey}</dt><dd><code>{credential.access_key}</code></dd></dl>
        <button disabled={action.busy} onClick={() => void action.run(async () => {
          try {
            await navigator.clipboard.writeText(JSON.stringify(credential, null, 2))
          } catch {
            throw new MessageError(message(relayCopy, 'copyFailed'))
          }
          return message(relayCopy, 'relayCredentialCopied')
        })}><Icon name="copy" size={16} />{t.copyRelayCredential}</button>
        <p>{t.installWithCLI}<br /><code>{'wind relay add <url> --access-key <key>'}</code></p></>
      : <ValidatedForm className="stack" onSubmit={event => {
        event.preventDefault()
        const name = String(new FormData(event.currentTarget).get('name')).trim()
        void action.run(async () => setCredential(await api.createKey(name)))
      }}>
        <div className="field"><label htmlFor="key-name">{t.credentialName}</label>
          <input id="key-name" name="name" maxLength={128} required /></div>
        <button disabled={action.busy}>{action.busy ? t.creating : t.createKey}</button>
      </ValidatedForm>}
    <Notice feedback={action.feedback} /><BackLink href="/">{t.backToAccess}</BackLink>
  </FocusPage>
}
