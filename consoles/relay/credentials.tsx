import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Confirm } from '../ui/forms'
import { Badge, DateStamp, FocusPage, Notice, ReadError } from '../ui/layout'
import type { Capabilities, Credential, RelayClient, ServiceConfiguration } from './api'

function markCredentialRevoked(items: Credential[], credentialId: string): Credential[] {
  return items.map(item => item.credential_id === credentialId ? { ...item, revoked: true } : item)
}

function emptyCredentialsTitle(list: ReturnType<typeof useResource<Credential[]>>): string {
  if (list.value) return 'No credentials yet'
  return list.busy ? 'Loading credentials…' : 'Credentials unavailable'
}

export function Credentials({ api, capabilities }: { api: RelayClient; capabilities: Capabilities }) {
  const list = useResource(api.credentials)
  const action = useAction()
  const [selected, setSelected] = useState<Credential | null>(null)
  const [stop, setStop] = useState(false)
  const forced = capabilities.revocation_stops_shares === true
  const confirm = selected?.revoked ? 'Stop remaining shares' : 'Revoke access'
  function revokeSelected(target: Credential) {
    setSelected(null)
    void action.run(async () => {
      await api.revoke(target.credential_id, forced || target.revoked || stop)
      list.update(items => markCredentialRevoked(items, target.credential_id))
      return target.revoked ? 'Stopping remaining shares on this node.' : 'Credential revoked.'
    })
  }
  return <section aria-labelledby="credentials-title">
    <div className="section-title"><h2 id="credentials-title">Credentials</h2>
      <div className="actions"><button className="quiet" disabled={list.busy} onClick={list.reload}>Refresh</button>
        <a className="button secondary" href="#service-key">Create service key</a></div></div>
    <Notice feedback={action.feedback} /><ReadError error={list.error} retry={list.reload} />
    <div className="panel inventory">
      {!list.value?.length && <div className="empty"><h3>{emptyCredentialsTitle(list)}</h3>
        {list.value && <p>Pair a device or create a service key to grant access.</p>}</div>}
      {list.value?.map(item => <div className="credential-row" key={item.credential_id}>
        <div><strong>{item.name}</strong><small>Last used {item.last_used_at == null ? '—' : <DateStamp millis={item.last_used_at} />}</small></div>
        <Badge tone={item.revoked ? 'neutral' : 'good'}>{item.revoked ? 'Revoked' : 'Active'}</Badge>
        {(!item.revoked || !forced) && <button type="button" className="quiet danger-text" disabled={action.busy}
          aria-label={(item.revoked ? 'Stop remaining shares for ' : 'Revoke ') + item.name}
          onClick={() => { setStop(false); setSelected(item) }}>{item.revoked ? 'Stop remaining shares' : 'Revoke'}</button>}
      </div>)}
    </div>
    {selected && <Confirm title={selected.revoked ? 'Stop remaining shares?' : 'Revoke ' + selected.name + '?'} confirm={confirm}
      onCancel={() => setSelected(null)} onConfirm={() => revokeSelected(selected)}>
      {forced || selected.revoked
        ? <p>This device’s existing shares will stop relaying through this node. Direct transfers can continue.</p>
        : <><p>This credential can no longer start shares. Its managed TURN leases will be withdrawn. Transfers using TURN may need to switch paths.</p>
          <label className="check" htmlFor="stop-shares"><input id="stop-shares" type="checkbox" checked={stop}
            onChange={event => setStop(event.target.checked)} />Also stop existing shares on this node</label></>}
    </Confirm>}
  </section>
}

export function ServiceKey({ api }: { api: RelayClient }) {
  const [configuration, setConfiguration] = useState<ServiceConfiguration | null>(null)
  const action = useAction()
  return <FocusPage label="Node access" title={configuration ? 'Service key created' : 'Create service key'}>
    {configuration
      ? <><p className="lead">Copy the configuration now. The key is shown once.</p>
        <dl className="facts"><dt>Relay address</dt><dd><code>{configuration.url}</code></dd>
          <dt>Access key</dt><dd><code>{configuration.access_key}</code></dd></dl>
        <button disabled={action.busy} onClick={() => void action.run(async () => {
          await navigator.clipboard.writeText(JSON.stringify(configuration, null, 2))
          return 'Configuration copied.'
        })}>Copy configuration</button></>
      : <form className="stack" onSubmit={event => {
        event.preventDefault()
        const name = String(new FormData(event.currentTarget).get('name')).trim()
        void action.run(async () => setConfiguration(await api.createKey(name)))
      }}>
        <div className="field"><label htmlFor="key-name">Credential name</label>
          <input id="key-name" name="name" maxLength={128} required /></div>
        <button disabled={action.busy}>{action.busy ? 'Creating…' : 'Create key'}</button>
      </form>}
    <Notice feedback={action.feedback} /><div className="foot-link"><a href="/">Back to node access</a></div>
  </FocusPage>
}
