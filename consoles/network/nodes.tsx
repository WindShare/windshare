import { useCallback, useState } from 'react'
import { FleetControls, ProbeResults, ProbeWarnings } from './fleet/operations'
import { Verification } from './verification'
import { useAction, useResource } from '../ui/activity'
import { message } from '../ui/locale'
import { useCopy, useLocale } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { networkCopy } from './locales'
import { Confirm } from '../ui/forms'
import { BackLink, Badge, Bytes, DateStamp, EmptyState, Notice, ReadError } from '../ui/layout'
import { Icon, type IconName } from '../ui/Icon'
import type { AuthorityClient } from './api'
import { encode } from './identity'
import type { LoadReport, NodeDetails, NodePage, PolicyChange, Publication, RelayNode } from './model'

type Directory = Publication | null | undefined

const STAGING_TONES = { queued: 'neutral', running: 'neutral', passed: 'good', failed: 'warning' } as const

function Listing({ directory, id, listing }: { directory: Directory; id: string; listing?: RelayNode['listing'] }) {
  const t = useCopy(networkCopy)
  if (listing?.state.kind === 'staging') {
    const { kind } = listing.state.check
    let note = t.notListed
    if (directory === undefined) note = t.checkingMirror
    else if (directory === null) note = t.unverified
    return <><Badge tone={STAGING_TONES[kind]}>{t.states[kind]}</Badge><small>{note}</small></>
  }
  if (directory === undefined) return <span>{t.checkingMirror}</span>
  if (directory === null) return <span>{t.unverified}</span>
  const listed = directory.keys.includes(id)
  return <><Badge tone={listed ? 'info' : 'neutral'}>{listed ? t.listed : t.notListed}</Badge>
    <small>{t.directoryVersion(directory.generation, directory.serial)}</small></>
}

function Latest({ report }: { report: LoadReport | null }) {
  const t = useCopy(networkCopy)
  if (!report) return <span>{t.awaitingReport}</span>
  return <><DateStamp millis={report.reported_at * 1000} /><small>{t.sessions(report.load.active_sessions)} · {t.relayed} <Bytes value={report.load.forwarded_bytes} /></small></>
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
    {directory && <div className="actions"><Badge>{t.directoryVersion(directory.generation, directory.serial)}</Badge>
      {!directory.mirrors_current && <Badge tone="warning">{t.mirrorLag}</Badge>}
      {directory.expiring_certificates.directory !== null && <Badge tone="warning">{t.directoryCertExpiry} · <DateStamp millis={directory.expiring_certificates.directory * 1000} /></Badge>}
      {directory.expiring_certificates.ticket !== null && <Badge tone="warning">{t.ticketCertExpiry} · <DateStamp millis={directory.expiring_certificates.ticket * 1000} /></Badge>}
      {directory.silent_vantages.length > 0 && <Badge tone="warning">{t.silentVantages} · {directory.silent_vantages.join(', ')}</Badge>}
    </div>}
    {nodes.length === 0 ? <div className="panel"><EmptyState icon="network" title={t.noNodes}>
      <p>{t.registerHint}</p>{enroll}</EmptyState></div>
      : <><NodeOverview nodes={nodes} complete={cursor === null} directory={directory} />
        <div className="table-wrap panel" role="region" aria-labelledby="node-heading" tabIndex={0}>
        <table><caption className="sr-only">{t.nodeTable}</caption>
          <thead><tr>{[t.nodeDetails, t.savedRelayPolicy, t.lastReport, t.directory].map(title => <th scope="col" key={title}>{title}</th>)}</tr></thead>
          <tbody>{nodes.map(node => {
            const id = encode(node.key)
            return <tr key={id}>
              <th scope="row"><a className="node-link" href={'#node/' + id}>
                <span className="row-icon"><Icon name="server" size={19} /></span>
                <span>{node.listing?.spec.alias || node.region || t.zones[node.zone]}<small>{t.zones[node.zone]} · <code>{id.slice(0, 8)}</code></small></span></a></th>
              <td data-label={t.savedRelayPolicy}><div><Badge tone={node.admission === 'accepting' ? 'good' : 'warning'}>{t.admissions[node.admission]}</Badge>
                <small>{node.anonymous ? t.anonymousAllowed : t.accountRequired} · v{node.version}</small></div></td>
              <td data-label={t.lastReport}><div><Latest report={node.report} /></div></td>
              <td data-label={t.directory}><div><Listing directory={directory} id={id} listing={node.listing} />{directory && <ProbeWarnings node={node} directory={directory} />}</div></td>
            </tr>
          })}</tbody>
        </table>
      </div>
      {cursor && <div className="actions"><button type="button" className="secondary" disabled={loading}
        onClick={loadMore}>{loading ? shared.loadingMore : shared.loadMore}</button></div>}</>}
  </div>
}

type NodeDetailActions = { refresh: () => Promise<void>; directory: Directory; api: AuthorityClient; saved: (node: RelayNode) => void }

export function LookupNode({ id, api, directory, saved, refresh }: NodeDetailActions & { id: string }) {
  const t = useCopy(networkCopy)
  const read = useCallback((signal: AbortSignal) => api.node(id, signal), [api, id])
  const resource = useResource(read)
  const node = resource.value
  if (node) return <NodeDetail node={node} api={api} directory={directory} refresh={refresh} saved={next => {
    resource.update(previous => previous && { ...previous, ...next })
    saved(next)
  }} />
  return <div className="view"><BackLink href="#nodes">{t.backToNodes}</BackLink><h1>{t.nodeDetails}</h1>
    <ReadError error={resource.error} retry={resource.retry} />
    {resource.busy && <p className="resource-loading" role="status"><Icon name="loader" />{t.loadingNode}</p>}
    {node === null && <div className="panel"><EmptyState icon="server" title={t.nodeNotFound} /></div>}
  </div>
}

export function NodeDetail({ node, directory, api, saved, refresh }: NodeDetailActions & { node: NodeDetails }) {
  const { report } = node
  const [confirmPause, setConfirmPause] = useState(false)
  const t = useCopy(networkCopy)
  const action = useAction()
  const id = encode(node.key)
  function change(change: PolicyChange) {
    void action.run(async () => {
      saved(await api.policy(node.key, change))
      return message(networkCopy, 'nodePolicySaved')
    })
  }
  return <div className="view"><BackLink href="#nodes">{t.backToNodes}</BackLink>
    <h1>{node.listing?.spec.alias || node.region || t.zones[node.zone]}</h1>
    <div className="node-detail-grid"><section className="panel padded"><h2>{t.nodeDetails}</h2>
      <dl className="facts"><dt>{t.region}</dt><dd>{node.region}</dd><dt>{t.zone}</dt><dd>{t.zones[node.zone]}</dd><dt>{t.publicKey}</dt><dd><code>{id}</code></dd>
        <dt>{t.directory}</dt><dd><Listing directory={directory} id={id} listing={node.listing} /></dd>
        <dt>{t.lastReport}</dt><dd>{report ? <DateStamp millis={report.reported_at * 1000} /> : t.awaitingReport}</dd>
        {report?.load.relay_budget && <><dt>{t.relayBudget}</dt><dd><Bytes value={report.load.relay_budget.bytes} /> · {t.resetsDay(report.load.relay_budget.reset_day_utc)}</dd></>}
        {report && <><dt>{t.appliedPolicy}</dt><dd>v{report.load.applied_policy_version} / v{node.version}</dd>
          <dt>{t.residentShares}</dt><dd>{report.load.resident_shares}</dd>
          <dt>{t.loadSnapshot} · <DateStamp millis={report.created_at * 1000} /></dt><dd>
          {t.sessions(report.load.active_sessions)} · {t.relayed} <Bytes value={report.load.forwarded_bytes} /> · {t.refusals(report.load.admission_refusals)}</dd></>}
      </dl></section>
    <section className="panel padded"><div className="section-title"><h2>{t.savedNodePolicy}</h2><Badge>v{node.version}</Badge></div>
      <div className="setting-row"><div><h3>{t.publicRelay}</h3><p>{t.admissions[node.admission]}</p></div>
        {!node.listing && <button className="secondary" disabled={action.busy} onClick={() => node.admission === 'disabled'
          ? change({ field: 'admission', value: 'accepting' }) : setConfirmPause(true)}>{node.admission === 'disabled' ? t.enableRelay : t.disable}</button>}</div>
      <div className="setting-row"><div><h3>{t.anonymousSharing}</h3><p>{node.anonymous ? t.allowed : t.accountRequired}</p></div>
        <button className="secondary" disabled={action.busy} onClick={() => change({ field: 'anonymous_sharing', value: !node.anonymous })}>
          {node.anonymous ? t.requireAccount : t.allowAnonymous}</button></div>
      <Notice feedback={action.feedback} />
    </section></div>
    {node.listing && <Verification node={node} api={api} refresh={refresh} />}
    {node.listing && <FleetControls node={node} api={api} refresh={refresh} />}
    {directory && <ProbeResults operation={node.listing?.operation_id} id={id} directory={directory} />}
    {confirmPause && <Confirm title={t.confirmPause} confirm={t.disable} onCancel={() => setConfirmPause(false)}
      onConfirm={() => { setConfirmPause(false); change({ field: 'admission', value: 'disabled' }) }}>
      <p>{t.pauseConsequences}</p>
    </Confirm>}
  </div>
}
