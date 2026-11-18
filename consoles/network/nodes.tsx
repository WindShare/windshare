import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { message } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { networkCopy } from './locales'
import { Confirm } from '../ui/forms'
import { Badge, DateStamp, Notice } from '../ui/layout'
import type { AuthorityClient } from './api'
import { encode, requireIdentity } from './identity'
import { GIB, type LoadReport, type PolicyField, type Publication, type RelayNode, type Zone } from './model'

type Directory = Publication | null | undefined

function Listing({ directory, id }: { directory: Directory; id: string }) {
  const t = useCopy(networkCopy)
  if (directory === undefined) return <span>{t.checkingMirror}</span>
  if (directory === null) return <span>{t.unverified}</span>
  const listed = directory.keys.includes(id)
  return <><Badge tone={listed ? 'good' : 'neutral'}>{listed ? t.listed : t.notListed}</Badge>
    <small>{t.directoryVersion(directory.generation, directory.serial)}</small></>
}

function ReportTime({ report }: { report: LoadReport }) {
  const t = useCopy(networkCopy)
  return report.reported_at == null ? t.noReceiptTime : <DateStamp millis={report.reported_at * 1000} />
}

function Latest({ report }: { report?: LoadReport }) {
  const t = useCopy(networkCopy)
  if (!report) return <span>{t.awaitingReport}</span>
  return <><ReportTime report={report} /><small>{t.recentLoad(report.load.active_sessions, report.load.forwarded_bytes / GIB)}</small></>
}

export function Nodes({ nodes, reports, directory }: { nodes: RelayNode[]; reports: LoadReport[]; directory: Directory }) {
  const t = useCopy(networkCopy)
  const byKey = new Map(reports.map(item => [item.node, item]))
  return <div className="view"><div className="page-title"><h1 id="node-heading">{t.nodes}</h1>
    {nodes.length > 0 && <a className="button" href="#enroll">{t.enroll}</a>}</div>
    {nodes.length === 0 ? <div className="panel empty"><h2>{t.noNodes}</h2>
      <p>{t.registerHint}</p><a className="button" href="#enroll">{t.enroll}</a></div>
      : <div className="table-wrap panel" role="region" aria-labelledby="node-heading" tabIndex={0}>
        <table><caption className="sr-only">{t.nodeTable}</caption>
          <thead><tr>{[t.region, t.savedRelayPolicy, t.lastReport, t.directory].map(title => <th scope="col" key={title}>{title}</th>)}</tr></thead>
          <tbody>{nodes.map(node => {
            const id = encode(node.key)
            return <tr key={id}>
              <th scope="row"><a href={'#node/' + id}>{node.region || t.zones[node.zone]}</a><small>{t.zones[node.zone]} · <code>{id.slice(0, 8)}</code></small></th>
              <td data-label={t.savedRelayPolicy}><Badge tone={node.enabled ? 'good' : 'warning'}>{node.enabled ? t.enabled : t.paused}</Badge>
                <small>{node.anonymous ? t.anonymousAllowed : t.accountRequired} · v{node.version}</small></td>
              <td data-label={t.lastReport}><Latest report={byKey.get(id)} /></td>
              <td data-label={t.directory}><Listing directory={directory} id={id} /></td>
            </tr>
          })}</tbody>
        </table>
      </div>}
  </div>
}

export function EnrollNode({ api, saved }: { api: AuthorityClient; saved: (node: RelayNode) => void }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  return <div className="view editor"><a href="#nodes">{t.backToNodes}</a><h1>{t.enroll}</h1>
    <ValidatedForm className="panel padded stack" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(async () => {
        const node = await api.enroll({ key: requireIdentity(String(data.get('key')), 'node'),
          zone: data.get('zone') as Zone, region: String(data.get('region')).trim() })
        saved(node)
        location.hash = 'node/' + encode(node.key)
      })
    }}>
      <div className="field"><label htmlFor="node-key">{t.publicNodeKey}</label>
        <input id="node-key" name="key" autoCapitalize="none" spellCheck={false} required /></div>
      <div className="field"><label htmlFor="zone">{t.zone}</label><select id="zone" name="zone">
        <option value="global">{t.zones.global}</option><option value="cn">{t.zones.cn}</option></select></div>
      <div className="field"><label htmlFor="region">{t.region}</label><input id="region" name="region" maxLength={128} required /></div>
      <Notice feedback={action.feedback} /><div><button disabled={action.busy}>{action.busy ? t.enrolling : t.enroll}</button></div>
    </ValidatedForm></div>
}

export function NodeDetail({ node, report, directory, api, saved }: {
  node: RelayNode; report?: LoadReport; directory: Directory; api: AuthorityClient; saved: (node: RelayNode) => void
}) {
  const [confirmPause, setConfirmPause] = useState(false)
  const t = useCopy(networkCopy)
  const action = useAction()
  const id = encode(node.key)
  function change(field: PolicyField, value: boolean) {
    void action.run(async () => {
      saved(await api.policy(node.key, field, value))
      return message(networkCopy, 'nodePolicySaved')
    })
  }
  return <div className="view editor"><a href="#nodes">{t.backToNodes}</a>
    <h1>{node.region || t.zones[node.zone]}</h1>
    <section className="panel padded"><h2>{t.nodeDetails}</h2>
      <dl className="facts"><dt>{t.zone}</dt><dd>{t.zones[node.zone]}</dd><dt>{t.publicKey}</dt><dd><code>{id}</code></dd>
        <dt>{t.directory}</dt><dd><Listing directory={directory} id={id} /></dd>
        <dt>{t.lastReport}</dt><dd>{report ? <ReportTime report={report} /> : t.awaitingReport}</dd>
        {report && <><dt>{t.loadSnapshot} · <DateStamp millis={report.created_at * 1000} /></dt><dd>
          {t.detailedLoad(report.load.active_sessions, report.load.forwarded_bytes, report.load.admission_refusals)}</dd></>}
      </dl></section>
    <section className="panel padded"><div className="section-title"><h2>{t.savedNodePolicy}</h2><Badge>v{node.version}</Badge></div>
      <div className="setting-row"><div><h3>{t.publicRelay}</h3><p>{node.enabled ? t.enabled : t.paused}</p></div>
        <button className="secondary" disabled={action.busy} onClick={() => node.enabled ? setConfirmPause(true) : change('public_relay', true)}>
          {node.enabled ? t.pauseRelay : t.enableRelay}</button></div>
      <div className="setting-row"><div><h3>{t.anonymousSharing}</h3><p>{node.anonymous ? t.allowed : t.accountRequired}</p></div>
        <button className="secondary" disabled={action.busy} onClick={() => change('anonymous_sharing', !node.anonymous)}>
          {node.anonymous ? t.requireAccount : t.allowAnonymous}</button></div>
      <Notice feedback={action.feedback} />
    </section>
    {confirmPause && <Confirm title={t.confirmPause} confirm={t.pauseRelay} onCancel={() => setConfirmPause(false)}
      onConfirm={() => { setConfirmPause(false); change('public_relay', false) }}>
      <p>{t.pauseConsequences}</p>
    </Confirm>}
  </div>
}
