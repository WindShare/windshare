import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { BackLink, Notice } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import type { AuthorityClient } from './api'
import { encode } from './identity'
import { networkCopy } from './locales'
import type { Discovery, NodeDetails, Publication, Role, Zone } from './model'

type Props = { api: AuthorityClient; saved: (node: NodeDetails) => void; setup: Publication['setup'] | undefined }

function setupCommand(address: string, trust: Publication['setup']): string | undefined {
  let target: URL
  try { target = new URL(address) } catch { return undefined }
  if (target.protocol !== 'https:' || target.username || target.password || target.pathname !== '/' || target.search || target.hash) return undefined
  const config = { listen: '127.0.0.1:8484', database: 'relay.sqlite', identity_file: 'identity.json',
    endpoint: 'wss://' + target.host + '/v2/ws', origins: { kind: 'public' },
    proxy: { peers: ['127.0.0.1'], header: 'x-windshare-source-ip' },
    stun: { udp_addresses: ['0.0.0.0:3478'], admin_address: '127.0.0.1:8081' },
    node: { services: { relay: true, stun: true }, stun_urls: ['stun:' + target.hostname + ':3478'],
      authority: { url: trust.url, issuer: trust.issuer }, access: { anonymous: false, trusted_issuers: [{ root: trust.issuer, certificate: trust.certificate }] } } }
  // Only base64 reaches the shell. Noclobber preserves an existing node configuration.
  const payload = btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(config))))
  return `(umask 077; set -C; printf %s '${payload}' | base64 -d > node.json) && wsrelay --config node.json`
}

export function EnrollNode({ api, saved, setup }: Props) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const [address, setAddress] = useState('')
  const [node, setNode] = useState<Discovery>()
  const command = setup && setupCommand(address, setup)
  return <div className="view editor"><BackLink href="#nodes">{t.backToNodes}</BackLink><h1>{t.enroll}</h1>
    <ValidatedForm className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      void action.run(async () => setNode(await api.discover(address.trim())))
    }}><div className="field"><label htmlFor="node-address">{t.endpoint}</label>
      <input id="node-address" type="url" required placeholder="https://node.example.org" value={address} disabled={action.busy}
        onChange={event => { setAddress(event.target.value); setNode(undefined) }} /></div>
      <div><button disabled={action.busy}>{t.inspectNode}</button></div>
      {command && <details><summary>{t.freshInstall}</summary>
        <p>{t.setupHint}</p><textarea aria-label={t.setupCommand} readOnly value={command} onFocus={event => event.target.select()} />
        <button type="button" className="secondary" onClick={() => void action.run(() => navigator.clipboard.writeText(command))}>{t.copySetup}</button></details>}
    </ValidatedForm>
    {node && <ValidatedForm key={node.node_key} className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(async () => {
        const added = await api.enroll({ url: node.relay, node_key: node.node_key, alias: String(data.get('alias')).trim(),
          zone: data.get('zone') as Zone, region: String(data.get('region')).trim(), provider: String(data.get('provider')).trim(),
          roles: data.getAll('role') as Role[], capacity_mbps: Number(data.get('capacity')) })
        saved(added)
        location.hash = 'node/' + encode(added.key)
      })
    }}>
      <div className="facts"><span>{t.publicNodeKey}</span><code>{node.node_key}</code></div>
      <label className="field">{t.alias}<input name="alias" pattern="[a-z]([a-z0-9\-]{0,30}[a-z0-9])?" maxLength={32} required /></label>
      <label className="field">{t.zone}<select name="zone" defaultValue={node.zone}>
        <option value="global">{t.zones.global}</option><option value="cn">{t.zones.cn}</option></select></label>
      <label className="field">{t.region}<input name="region" defaultValue={node.region} maxLength={128} required /></label>
      <label className="field">{t.provider}<input name="provider" maxLength={128} required /></label>
      <label className="field">{t.capacity}<input name="capacity" type="number" min={1} max={4294967295} defaultValue={node.capacity_mbps ?? ''} required /></label>
      <fieldset><legend>{t.services}</legend>{node.roles.map(role => <label key={role}>
        <input type="checkbox" name="role" value={role} defaultChecked />{role === 'stun' ? 'STUN' : t.publicRelay}</label>)}</fieldset>
      <div><button disabled={action.busy}>{t.enroll}</button></div>
    </ValidatedForm>}
    <Notice feedback={action.feedback} />
  </div>
}
