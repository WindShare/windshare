import { useState } from 'react'
import { useAction } from '../ui/activity'
import { Confirm } from '../ui/forms'
import { Badge, DateStamp, Notice } from '../ui/layout'
import type { AuthorityClient } from './api'
import { decode, encode, KEY_BYTES } from './identity'
import { GIB, zoneName, type LoadReport, type PolicyField, type Publication, type RelayNode, type Zone } from './model'

type Directory = Publication | null | undefined

function Listing({ directory, id }: { directory: Directory; id: string }) {
  if (directory === undefined) return <span>Checking mirror…</span>
  if (directory === null) return <span>Directory unverified</span>
  const listed = directory.keys.includes(id)
  return <><Badge tone={listed ? 'good' : 'neutral'}>{listed ? 'Listed' : 'Not listed'}</Badge>
    <small>Directory {directory.generation}.{directory.serial}</small></>
}

function ReportTime({ report }: { report: LoadReport }) {
  return report.reported_at == null ? 'Receipt time unavailable' : <DateStamp millis={report.reported_at * 1000} />
}

function Latest({ report }: { report?: LoadReport }) {
  if (!report) return <span>Awaiting first report</span>
  return <><ReportTime report={report} /><small>{report.load.active_sessions.toLocaleString()} sessions
    {' · '}{(report.load.forwarded_bytes / GIB).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB relayed</small></>
}

export function Nodes({ nodes, reports, directory }: { nodes: RelayNode[]; reports: LoadReport[]; directory: Directory }) {
  const byKey = new Map(reports.map(item => [item.node, item]))
  return <div className="view"><div className="page-title"><h1 id="node-heading">Nodes</h1>
    {nodes.length > 0 && <a className="button" href="#enroll">Enroll node</a>}</div>
    {nodes.length === 0 ? <div className="panel empty"><h2>No enrolled nodes</h2>
      <p>Register a node with its public key to start receiving reports.</p><a className="button" href="#enroll">Enroll node</a></div>
      : <div className="table-wrap panel" role="region" aria-labelledby="node-heading" tabIndex={0}>
        <table><caption className="sr-only">Enrolled nodes and their saved public relay policies</caption>
          <thead><tr>{['Region', 'Saved public relay policy', 'Last report', 'Directory'].map(title => <th scope="col" key={title}>{title}</th>)}</tr></thead>
          <tbody>{nodes.map(node => {
            const id = encode(node.key)
            return <tr key={id}>
              <th scope="row"><a href={'#node/' + id}>{node.region || zoneName(node.zone)}</a><small>{zoneName(node.zone)} · <code>{id.slice(0, 8)}</code></small></th>
              <td data-label="Saved public relay policy"><Badge tone={node.enabled ? 'good' : 'warning'}>{node.enabled ? 'Enabled' : 'Paused'}</Badge>
                <small>{node.anonymous ? 'Anonymous allowed' : 'Account required'} · v{node.version}</small></td>
              <td data-label="Last report"><Latest report={byKey.get(id)} /></td>
              <td data-label="Directory"><Listing directory={directory} id={id} /></td>
            </tr>
          })}</tbody>
        </table>
      </div>}
  </div>
}

export function EnrollNode({ api, saved }: { api: AuthorityClient; saved: (node: RelayNode) => void }) {
  const action = useAction()
  return <div className="view editor"><a href="#nodes">Back to nodes</a><h1>Enroll node</h1>
    <form className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(async () => {
        const node = await api.enroll({ key: decode(String(data.get('key')), KEY_BYTES, 'node public key'),
          zone: data.get('zone') as Zone, region: String(data.get('region')).trim() })
        saved(node)
        location.hash = 'node/' + encode(node.key)
      })
    }}>
      <div className="field"><label htmlFor="node-key">Node public key</label>
        <input id="node-key" name="key" autoCapitalize="none" spellCheck={false} required /></div>
      <div className="field"><label htmlFor="zone">Zone</label><select id="zone" name="zone">
        <option value="global">Global</option><option value="cn">Mainland China</option></select></div>
      <div className="field"><label htmlFor="region">Region</label><input id="region" name="region" maxLength={128} required /></div>
      <Notice feedback={action.feedback} /><div><button disabled={action.busy}>{action.busy ? 'Enrolling…' : 'Enroll node'}</button></div>
    </form></div>
}

export function NodeDetail({ node, report, directory, api, saved }: {
  node: RelayNode; report?: LoadReport; directory: Directory; api: AuthorityClient; saved: (node: RelayNode) => void
}) {
  const [confirmPause, setConfirmPause] = useState(false)
  const action = useAction()
  const id = encode(node.key)
  function change(field: PolicyField, value: boolean) {
    void action.run(async () => {
      saved(await api.policy(node.key, field, value))
      return 'Policy saved. It will be sent with the node’s next successful report.'
    })
  }
  return <div className="view editor"><a href="#nodes">Back to nodes</a>
    <h1>{node.region || zoneName(node.zone)}</h1>
    <section className="panel padded"><h2>Node details</h2>
      <dl className="facts"><dt>Zone</dt><dd>{zoneName(node.zone)}</dd><dt>Public key</dt><dd><code>{id}</code></dd>
        <dt>Directory</dt><dd><Listing directory={directory} id={id} /></dd>
        <dt>Last report</dt><dd>{report ? <ReportTime report={report} /> : 'Awaiting first report'}</dd>
        {report && <><dt>Load snapshot · <DateStamp millis={report.created_at * 1000} /></dt><dd>
          {report.load.active_sessions.toLocaleString()} sessions · {report.load.forwarded_bytes.toLocaleString()} bytes relayed · {report.load.admission_refusals.toLocaleString()} refusals</dd></>}
      </dl></section>
    <section className="panel padded"><div className="section-title"><h2>Saved node policy</h2><Badge>v{node.version}</Badge></div>
      <div className="setting-row"><div><h3>Public relay</h3><p>{node.enabled ? 'Enabled' : 'Paused'}</p></div>
        <button className="secondary" disabled={action.busy} onClick={() => node.enabled ? setConfirmPause(true) : change('public_relay', true)}>
          {node.enabled ? 'Pause public relay' : 'Enable public relay'}</button></div>
      <div className="setting-row"><div><h3>Anonymous sharing</h3><p>{node.anonymous ? 'Allowed' : 'Account required'}</p></div>
        <button className="secondary" disabled={action.busy} onClick={() => change('anonymous_sharing', !node.anonymous)}>
          {node.anonymous ? 'Require an account' : 'Allow anonymous sharing'}</button></div>
      <Notice feedback={action.feedback} />
    </section>
    {confirmPause && <Confirm title="Pause public relay on this node?" confirm="Pause public relay" onCancel={() => setConfirmPause(false)}
      onConfirm={() => { setConfirmPause(false); change('public_relay', false) }}>
      <p>Once the node applies this policy, it will close existing public relay connections and refuse new ones for anonymous and network-account shares. Shares using local credentials can still relay through this node.</p>
    </Confirm>}
  </div>
}
