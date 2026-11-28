import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { BackLink, Notice } from '../ui/layout'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import type { AuthorityClient } from './api'
import { encode } from './identity'
import { networkCopy } from './locales'
import type { Discovery, NodeDetails, NodeSetup, Role, Zone } from './model'

type Props = { api: AuthorityClient; saved: (node: NodeDetails) => void; refresh: () => Promise<void>; setup?: NodeSetup }
type SetupOptions = { zone: Zone; capacityMbps: number }

const BYTES_PER_MBPS = 125_000
const MAX_CAPACITY_MBPS = 4_294_967_295
const MAX_DOMAIN_LENGTH = 253
const SETUP_COMMAND_ROWS = 12
const RESERVED_TLDS = new Set(['localhost', 'local', 'internal', 'invalid', 'onion', 'arpa'])
const DOMAIN_LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/
const POSITIVE_INTEGER = /^\d+$/

function hasControlChar(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code < 0x20 || code === 0x7f) return true
  }
  return false
}

// Mirror the authority's public-origin rules so localhost, IPs, and partial input never emit a sudo script.
function isPublicDomain(hostname: string): boolean {
  if (!hostname || hostname.length > MAX_DOMAIN_LENGTH || !hostname.includes('.') || hostname.endsWith('.')) {
    return false
  }
  const labels = hostname.split('.')
  const tld = labels[labels.length - 1]
  if (RESERVED_TLDS.has(tld) || POSITIVE_INTEGER.test(tld)) return false
  return labels.every(label => DOMAIN_LABEL.test(label) && (label.slice(2, 4) !== '--' || label.startsWith('xn--')))
}

function parsePublicOrigin(address: string): URL | undefined {
  const trimmed = address.trim()
  if (!trimmed.startsWith('https://') || /[?#\\@]/.test(trimmed) || hasControlChar(trimmed)) return undefined
  const authority = trimmed.slice('https://'.length)
  const slash = authority.indexOf('/')
  if (slash !== -1 && slash !== authority.length - 1) return undefined
  let target: URL
  try { target = new URL(trimmed) } catch { return undefined }
  if (target.protocol !== 'https:' || target.username || target.password || target.pathname !== '/' || target.port === '0') {
    return undefined
  }
  return isPublicDomain(target.hostname) ? target : undefined
}

function parseCapacityMbps(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (!POSITIVE_INTEGER.test(trimmed)) return undefined
  const value = Number(trimmed)
  return Number.isSafeInteger(value) && value >= 1 && value <= MAX_CAPACITY_MBPS ? value : undefined
}

function setupCommand(target: URL, trust: NodeSetup, { zone, capacityMbps }: SetupOptions): string {
  const config = {
    listen: '127.0.0.1:8484',
    database: '/var/lib/windshare-network/relay.sqlite',
    identity_file: '/var/lib/windshare-network/identity.json',
    endpoint: 'wss://' + target.host + '/v2/ws',
    origins: { kind: 'public' },
    proxy: { peers: ['127.0.0.1'], header: 'x-windshare-source-ip' },
    stun: { udp_addresses: ['0.0.0.0:3478'], admin_address: '127.0.0.1:8081' },
    node: {
      zone,
      // Explicit measured egress capacity keeps node-wide fair scheduling active on first boot.
      ceiling: { bytes_per_second: capacityMbps * BYTES_PER_MBPS },
      services: { relay: true, stun: true },
      stun_urls: ['stun:' + target.hostname + ':3478'],
      authority: { url: trust.url, issuer: trust.issuer },
      access: {
        anonymous: false,
        trusted_issuers: [{ root: trust.issuer, certificate: trust.certificate }],
      },
    },
  }
  // An exclusive link installs only the fully validated configuration on a fresh host.
  const payload = btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(config))))
  return `sudo sh -eu <<'WINDSHARE_SETUP'
install -d -m 0755 /etc/windshare-network
config=$(mktemp /etc/windshare-network/.setup.XXXXXXXX)
trap 'rm -f -- "$config"' EXIT
printf %s '${payload}' | base64 -d > "$config"
chown root:windshare-network "$config"
chmod 0640 "$config"
runuser -u windshare-network -- /opt/windshare-network/current/wsrelay --check-config "$config"
ln "$config" /etc/windshare-network/node.json
systemctl enable --now windshare-network.service
WINDSHARE_SETUP`
}

function SetupCommandOutput({ command }: { command: string }) {
  const t = useCopy(networkCopy)
  const copy = useAction()
  return <>
    <p>{t.setupHint}</p>
    <textarea className="setup-command" rows={SETUP_COMMAND_ROWS} aria-label={t.setupCommand} readOnly
      value={command} onFocus={event => event.target.select()} />
    <div><button type="button" className="secondary" disabled={copy.busy} onClick={() => void copy.run(async () => {
      try { await navigator.clipboard.writeText(command) }
      catch { throw new MessageError(message(networkCopy, 'copyCommandFailed')) }
      return message(networkCopy, 'setupCommandCopied')
    })}>{t.copySetup}</button></div>
    <Notice feedback={copy.feedback} />
  </>
}

function FreshNodeSetup({ origin, setup, zone, onZoneChange, capacity, onCapacityChange }: {
  origin: URL
  setup: NodeSetup
  zone: '' | Zone
  onZoneChange: (zone: '' | Zone) => void
  capacity: string
  onCapacityChange: (capacity: string) => void
}) {
  const t = useCopy(networkCopy)
  const capacityMbps = parseCapacityMbps(capacity)
  const command = zone && capacityMbps !== undefined
    ? setupCommand(origin, setup, { zone, capacityMbps })
    : undefined
  return <details className="setup-panel"><summary>{t.freshInstall}</summary>
    <div className="setup-body stack">
      <div className="setup-fields">
        <div className="field"><label htmlFor="setup-zone">{t.zone}</label>
          <select id="setup-zone" value={zone} onChange={event => onZoneChange(event.target.value as '' | Zone)}>
            <option value="">{t.selectZone}</option>
            <option value="global">{t.zones.global}</option>
            <option value="cn">{t.zones.cn}</option>
          </select></div>
        <div className="field"><label htmlFor="setup-capacity">{t.capacity}</label>
          <input id="setup-capacity" type="number" inputMode="numeric" min={1} max={MAX_CAPACITY_MBPS} step={1}
            placeholder="100" value={capacity} onChange={event => onCapacityChange(event.target.value)} /></div>
      </div>
      {command ? <SetupCommandOutput key={command} command={command} /> : <p>{t.setupParamsHint}</p>}
    </div>
  </details>
}

export function EnrollNode({ api, saved, refresh, setup }: Props) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const [address, setAddress] = useState('')
  const [setupZone, setSetupZone] = useState<'' | Zone>('')
  const [setupCapacity, setSetupCapacity] = useState('')
  const [node, setNode] = useState<Discovery>()
  const origin = setup ? parsePublicOrigin(address) : undefined
  return <div className="view editor"><BackLink href="#nodes">{t.backToNodes}</BackLink><h1>{t.enroll}</h1>
    <section className="panel padded">
      <ValidatedForm className="stack" onSubmit={event => {
        event.preventDefault()
        void action.run(async () => setNode(await api.discover(address.trim())))
      }}><div className="field"><label htmlFor="node-address">{t.endpoint}</label>
        <input id="node-address" type="url" required placeholder="https://node.example.org" value={address} disabled={action.busy}
          onChange={event => { setAddress(event.target.value); setNode(undefined) }} /></div>
        <div><button disabled={action.busy}>{t.inspectNode}</button></div>
      </ValidatedForm>
      {origin && setup && <FreshNodeSetup origin={origin} setup={setup}
        zone={setupZone} onZoneChange={setSetupZone} capacity={setupCapacity} onCapacityChange={setSetupCapacity} />}
    </section>
    {node && <ValidatedForm key={node.node_key} className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      const roles = data.getAll('role') as Role[]
      void action.run(async () => {
        if (roles.length === 0) throw new MessageError(message(networkCopy, 'selectService'))
        const added = await api.enroll({ url: node.relay, node_key: node.node_key, alias: String(data.get('alias')).trim(),
          zone: data.get('zone') as Zone, region: String(data.get('region')).trim(), provider: String(data.get('provider')).trim(),
          roles, capacity_mbps: Number(data.get('capacity')) })
        saved(added)
        await refresh()
        location.hash = 'node/' + encode(added.key)
      })
    }}>
      <dl className="facts"><dt>{t.publicNodeKey}</dt><dd><code>{node.node_key}</code></dd></dl>
      <label className="field">{t.alias}<input name="alias" pattern="[a-z]([a-z0-9\-]{0,30}[a-z0-9])?" title={t.aliasFormat} maxLength={32} required /></label>
      <label className="field">{t.zone}<select name="zone" defaultValue={node.zone}>
        <option value="global">{t.zones.global}</option><option value="cn">{t.zones.cn}</option></select></label>
      <label className="field">{t.region}<input name="region" defaultValue={node.region} maxLength={128} required /></label>
      <label className="field">{t.provider}<input name="provider" maxLength={128} required /></label>
      <label className="field">{t.capacity}<input name="capacity" type="number" min={1} max={MAX_CAPACITY_MBPS} defaultValue={node.capacity_mbps ?? ''} required /></label>
      <fieldset className="service-checks"><legend>{t.services}</legend>{node.roles.map(role => <label key={role} className="check">
        <input type="checkbox" name="role" value={role} defaultChecked />{role === 'stun' ? 'STUN' : t.publicRelay}</label>)}</fieldset>
      <div><button disabled={action.busy}>{t.enroll}</button></div>
    </ValidatedForm>}
    <Notice feedback={action.feedback} />
  </div>
}
