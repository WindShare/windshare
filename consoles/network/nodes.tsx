import { useCallback, useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { message } from '../ui/locale'
import { useCopy, useLocale } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { networkCopy } from './locales'
import { Confirm } from '../ui/forms'
import { BackLink, Badge, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { Icon, type IconName } from '../ui/Icon'
import type { AuthorityClient } from './api'
import { encode, requireIdentity } from './identity'
import { GIB, type LoadReport, type NodeDetails, type NodePage, type PolicyField, type Publication, type RelayNode, type Zone } from './model'

type Directory = Publication | null | undefined

function Listing({ directory, id }: { directory: Directory; id: string }) {
  const t = useCopy(networkCopy)
  if (directory === undefined) return <span>{t.checkingMirror}</span>
  if (directory === null) return <span>{t.unverified}</span>
  const listed = directory.keys.includes(id)
  return <><Badge tone={listed ? 'info' : 'neutral'}>{listed ? t.listed : t.notListed}</Badge>
    <small>{t.directoryVersion(directory.generation, directory.serial)}</small></>
}

function ReportTime({ report }: { report: LoadReport }) {
  const t = useCopy(networkCopy)
  return report.reported_at == null ? t.noReceiptTime : <DateStamp millis={report.reported_at * 1000} />
}

function Latest({ report }: { report: LoadReport | null }) {
  const t = useCopy(networkCopy)
  if (!report) return <span>{t.awaitingReport}</span>
  return <><ReportTime report={report} /><small>{t.recentLoad(report.load.active_sessions, report.load.forwarded_bytes / GIB)}</small></>
}

function NodeOverview({ nodes, complete, directory }: {
  nodes: NodeDetails[]; complete: boolean; directory: Directory
}) {
  const { locale } = useLocale()
  const t = networkCopy[locale]
  const published = new Set(directory?.keys)
  const listed = directory ? nodes.filter(node => published.has(encode(node.key))).length : null
  const metrics: { icon: IconName; label: string; value: number | null }[] = [
    { icon: 'server', label: complete ? t.enrolledCount : t.loadedNodes, value: nodes.length },
    { icon: 'clock', label: complete ? t.awaitingReport : t.loadedAwaitingReport, value: nodes.filter(node => !node.report).length },
    { icon: 'book', label: complete ? t.listedCount : t.loadedListed, value: listed },
  ]
  return <dl className="panel node-overview">{metrics.map(({ icon, label, value }) => <div key={icon}>
    <dt><span className="metric-icon"><Icon name={icon} /></span>{label}</dt>
    <dd>{value === null ? <small>{directory === undefined ? t.checkingMirror : t.unverified}</small> : value.toLocaleString(locale)}</dd>
  </div>)}</dl>
}

export function Nodes({ page, directory, loading, loadMore }: {
  page: NodePage; directory: Directory; loading: boolean; loadMore: () => void
}) {
  const t = useCopy(networkCopy)
  const shared = useCopy(sharedCopy)
  const { entries: nodes, next_cursor: cursor } = page
  const enroll = <a className="button" href="#enroll"><Icon name="plus" size={17} />{t.enroll}</a>
  return <div className="view"><div className="page-title"><h1 id="node-heading">{t.nodes}</h1>
    {nodes.length > 0 && enroll}</div>
    {nodes.length === 0 ? <div className="panel"><EmptyState icon="network" title={t.noNodes}>
      <p>{t.registerHint}</p>{enroll}</EmptyState></div>
      : <><NodeOverview nodes={nodes} complete={cursor === null} directory={directory} />
        <div className="table-wrap panel" role="region" aria-labelledby="node-heading" tabIndex={0}>
        <table><caption className="sr-only">{t.nodeTable}</caption>
          <thead><tr>{[t.region, t.savedRelayPolicy, t.lastReport, t.directory].map(title => <th scope="col" key={title}>{title}</th>)}</tr></thead>
          <tbody>{nodes.map(node => {
            const id = encode(node.key)
            return <tr key={id}>
              <th scope="row"><a className="node-link" href={'#node/' + id}>
                <span className="row-icon"><Icon name="server" size={19} /></span>
                <span>{node.region || t.zones[node.zone]}<small>{t.zones[node.zone]} · <code>{id.slice(0, 8)}</code></small></span></a></th>
              <td data-label={t.savedRelayPolicy}><div><Badge tone={node.enabled ? 'good' : 'warning'}>{node.enabled ? t.enabled : t.paused}</Badge>
                <small>{node.anonymous ? t.anonymousAllowed : t.accountRequired} · v{node.version}</small></div></td>
              <td data-label={t.lastReport}><div><Latest report={node.report} /></div></td>
              <td data-label={t.directory}><div><Listing directory={directory} id={id} /></div></td>
            </tr>
          })}</tbody>
        </table>
      </div>
      {cursor && <div className="actions"><button type="button" className="secondary" disabled={loading}
        onClick={loadMore}>{loading ? shared.loadingMore : shared.loadMore}</button></div>}</>}
  </div>
}

export function EnrollNode({ api, saved }: { api: AuthorityClient; saved: (node: NodeDetails) => void }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  return <div className="view editor"><BackLink href="#nodes">{t.backToNodes}</BackLink><h1>{t.enroll}</h1>
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

type NodeDetailActions = { directory: Directory; api: AuthorityClient; saved: (node: RelayNode) => void }

export function LookupNode({ id, api, directory, saved }: NodeDetailActions & { id: string }) {
  const t = useCopy(networkCopy)
  const read = useCallback((signal: AbortSignal) => api.node(id, signal), [api, id])
  const resource = useResource(read)
  const node = resource.value
  if (node) return <NodeDetail node={node} api={api} directory={directory} saved={next => {
    resource.update(previous => previous && { ...previous, ...next })
    saved(next)
  }} />
  return <div className="view"><BackLink href="#nodes">{t.backToNodes}</BackLink><h1>{t.nodeDetails}</h1>
    <ReadError error={resource.error} retry={resource.retry} />
    {resource.busy && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingNode}</p>}
    {node === null && <div className="panel"><EmptyState icon="server" title={t.nodeNotFound} /></div>}
  </div>
}

export function NodeDetail({ node, directory, api, saved }: NodeDetailActions & { node: NodeDetails }) {
  const { report } = node
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
  return <div className="view"><BackLink href="#nodes">{t.backToNodes}</BackLink>
    <h1>{node.region || t.zones[node.zone]}</h1>
    <div className="node-detail-grid"><section className="panel padded"><h2>{t.nodeDetails}</h2>
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
    </section></div>
    {confirmPause && <Confirm title={t.confirmPause} confirm={t.pauseRelay} onCancel={() => setConfirmPause(false)}
      onConfirm={() => { setConfirmPause(false); change('public_relay', false) }}>
      <p>{t.pauseConsequences}</p>
    </Confirm>}
  </div>
}
