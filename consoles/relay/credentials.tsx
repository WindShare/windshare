import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import { Confirm } from '../ui/forms'
import { Badge, DateStamp, FocusPage, Notice, ReadError } from '../ui/layout'
import type { Capabilities, Credential, RelayClient, ServiceConfiguration } from './api'

function markCredentialRevoked(items: Credential[], credentialId: string): Credential[] {
  return items.map(item => item.credential_id === credentialId ? { ...item, revoked: true } : item)
}

function emptyCredentialsTitle(t: typeof relayCopy.en, list: ReturnType<typeof useResource<Credential[]>>): string {
  if (list.value) return t.noCredentials
  return list.busy ? t.loadingCredentials : t.credentialsUnavailable
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
  return <section aria-labelledby="credentials-title">
    <div className="section-title"><h2 id="credentials-title">{t.credentials}</h2>
      <div className="actions"><button className="quiet" disabled={list.busy} onClick={list.reload}>{shared.refresh}</button>
        <a className="button secondary" href="#service-key">{t.createServiceKey}</a></div></div>
    <Notice feedback={action.feedback} /><ReadError error={list.error} retry={list.reload} />
    <div className="panel inventory">
      {!list.value?.length && <div className="empty"><h3>{emptyCredentialsTitle(t, list)}</h3>
        {list.value && <p>{t.grantAccess}</p>}</div>}
      {list.value?.map(item => <div className="credential-row" key={item.credential_id}>
        <div><strong>{item.name}</strong><small>{t.lastUsed} {item.last_used_at == null ? '—' : <DateStamp millis={item.last_used_at} />}</small></div>
        <Badge tone={item.revoked ? 'neutral' : 'good'}>{item.revoked ? t.revoked : t.active}</Badge>
        {(!item.revoked || !forced) && <button type="button" className="quiet danger-text" disabled={action.busy}
          aria-label={item.revoked ? t.stopNamed(item.name) : t.revokeNamed(item.name)}
          onClick={() => { setStop(false); setSelected(item) }}>{item.revoked ? t.stopShares : t.revoke}</button>}
      </div>)}
    </div>
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
  const [configuration, setConfiguration] = useState<ServiceConfiguration | null>(null)
  const action = useAction()
  return <FocusPage label={t.access} title={configuration ? t.keyCreated : t.createServiceKey}>
    {configuration
      ? <><p className="lead">{t.copyNow}</p>
        <dl className="facts"><dt>{t.relayAddress}</dt><dd><code>{configuration.url}</code></dd>
          <dt>{t.accessKey}</dt><dd><code>{configuration.access_key}</code></dd></dl>
        <button disabled={action.busy} onClick={() => void action.run(async () => {
          try {
            await navigator.clipboard.writeText(JSON.stringify(configuration, null, 2))
          } catch {
            throw new MessageError(message(relayCopy, 'copyFailed'))
          }
          return message(relayCopy, 'configCopied')
        })}>{t.copyConfig}</button></>
      : <ValidatedForm className="stack" onSubmit={event => {
        event.preventDefault()
        const name = String(new FormData(event.currentTarget).get('name')).trim()
        void action.run(async () => setConfiguration(await api.createKey(name)))
      }}>
        <div className="field"><label htmlFor="key-name">{t.credentialName}</label>
          <input id="key-name" name="name" maxLength={128} required /></div>
        <button disabled={action.busy}>{action.busy ? t.creating : t.createKey}</button>
      </ValidatedForm>}
    <Notice feedback={action.feedback} /><div className="foot-link"><a href="/">{t.backToAccess}</a></div>
  </FocusPage>
}
