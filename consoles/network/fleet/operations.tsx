import { useState } from 'react'
import { useAction } from '../../ui/activity'
import { Badge, DateStamp, Notice } from '../../ui/layout'
import { Confirm } from '../../ui/forms'
import { RequestError } from '../../ui/request'
import { useCopy } from '../../ui/use-locale'
import { networkCopy } from '../locales'
import type { AuthorityClient } from '../api'
import { servesRelay, type FleetAction, type NodeDetails, type Publication } from '../model'

const CERTIFICATE_WARNING_SECONDS = 14 * 86400

export function ProbeWarnings({ node, directory }: { node: NodeDetails; directory: Publication }) {
  const t = useCopy(networkCopy)
  const listing = node.listing
  if (!listing || listing.state.kind !== 'listed') return null
  const rows = directory.vantages.filter(v => !directory.silent_vantages.includes(v.id) &&
    v.nodes[listing.spec.node_key]?.operation_id === listing.operation_id)
  const expiring = rows.some(v => {
    const expiry = v.nodes[listing.spec.node_key].tls_expires
    return expiry !== null && expiry - v.at <= CERTIFICATE_WARNING_SECONDS
  })
  const failed = rows.filter(v => {
    const s = v.nodes[listing.spec.node_key]
    return !s.tcp || !s.https || s.relay === false || s.stun === false
  })
  return <>{expiring && <Badge tone="warning">{t.tlsExpiring}</Badge>}
    {failed.length > 0 && <Badge tone="warning">{t.probeWarning} · {failed.map(v => v.id).join(', ')}</Badge>}</>
}

export function ProbeResults({ id, operation, directory }: { id: string; operation: string | undefined; directory: Publication }) {
  const t = useCopy(networkCopy)
  const rows = directory.vantages.filter(v => v.nodes[id] && v.nodes[id].operation_id === operation)
  if (!rows.length) return null
  return <section className="panel padded stack"><h2>{t.probes}</h2>
    {rows.map(v => {
      const s = v.nodes[id]
      return <div key={v.id} className="setting-row"><div><h3>{v.id}</h3><DateStamp millis={v.at * 1000} /></div><div className="actions">
        {directory.silent_vantages.includes(v.id) && <Badge tone="warning">{t.silentVantages}</Badge>}
        {Object.entries({ TCP: s.tcp, HTTPS: s.https, Relay: s.relay, STUN: s.stun }).filter(([, ok]) => ok !== null).map(([role, ok]) =>
          <Badge key={role} tone={ok ? 'good' : 'warning'}>{role} · {ok ? t.passed : t.probeFailed}</Badge>)}
        {s.tls_expires !== null && <Badge tone={s.tls_expires - v.at <= CERTIFICATE_WARNING_SECONDS ? 'warning' : 'neutral'}>
          TLS · <DateStamp millis={s.tls_expires * 1000} /></Badge>}
      </div></div>
    })}
  </section>
}

type PendingAction = { kind: 'disable' | 'remove'; earlyCutoff: boolean }

function confirmDetails(t: typeof networkCopy.en, pending: PendingAction) {
  if (pending.kind === 'remove') {
    return {
      title: t.confirmRemove,
      confirm: t.removeNode,
      body: pending.earlyCutoff ? t.removeEarlyEffect : t.removeEffect,
    }
  }
  return pending.earlyCutoff
    ? { title: t.confirmCutoff, confirm: t.disable, body: t.cutoffEffect }
    : { title: t.confirmDisable, confirm: t.disable, body: t.disableEffect }
}

function canEscalateDrainPending(
  error: unknown,
  command: FleetAction,
): command is { kind: 'disable' | 'remove'; early_cutoff: false } {
  return error instanceof RequestError && error.code === 'drain_pending' &&
    (command.kind === 'disable' || command.kind === 'remove') && !command.early_cutoff
}

function FleetConfirmDialogs({ pending, aborting, onCancelPending, onConfirmPending, onCancelAbort, onConfirmAbort }: {
  pending: PendingAction | null
  aborting: string | null
  onCancelPending: () => void
  onConfirmPending: (action: PendingAction) => void
  onCancelAbort: () => void
  onConfirmAbort: (maintenanceId: string) => void
}) {
  const t = useCopy(networkCopy)
  const details = pending ? confirmDetails(t, pending) : null
  return <>
    {pending && details && <Confirm title={details.title} confirm={details.confirm}
      onCancel={onCancelPending} onConfirm={() => onConfirmPending(pending)}>
      <p>{details.body}</p></Confirm>}
    {aborting && <Confirm title={t.confirmAbortMaintenance} confirm={t.abortMaintenance}
      onCancel={onCancelAbort} onConfirm={() => onConfirmAbort(aborting)}>
      <p>{t.abortMaintenanceEffect}</p></Confirm>}
  </>
}

export function FleetControls({ node, api, refresh }: { node: NodeDetails; api: AuthorityClient; refresh: () => Promise<void> }) {
  const t = useCopy(networkCopy)
  const action = useAction()
  const [pending, setPending] = useState<PendingAction | null>(null)
  const [aborting, setAborting] = useState<string | null>(null)
  const listing = node.listing
  if (!listing || listing.state.kind === 'staging') return null
  const disabled = listing.state.kind === 'disabled'
  const maintenance = listing.maintenance
  const hasRelay = servesRelay(node)
  function run(command: FleetAction) {
    if (!listing) return
    void action.run(async () => {
      try {
        await api.manage(listing.spec.node_key, listing.operation_id, command)
      } catch (error) {
        if (canEscalateDrainPending(error, command)) {
          // Escalate a stale-snapshot drain conflict straight into the early-cutoff confirmation
          // without leaving a raw error banner behind the dialog.
          setPending({ kind: command.kind, earlyCutoff: true })
          return
        }
        throw error
      }
      if (command.kind === 'remove') location.hash = 'nodes'
      await refresh()
    })
  }
  const next = disabled ? 'remove' : 'disable'
  return <section className="panel padded stack"><h2>{t.fleetOperations}</h2>
    {maintenance && <Badge tone="info">{disabled ? t.disableAfterMaintenance : t.maintenance} · {maintenance}</Badge>}
    <div className="actions">
      {hasRelay && listing.state.kind === 'listed' && listing.state.status !== 'draining' &&
        <button type="button" className="secondary" disabled={action.busy} onClick={() => run({ kind: 'drain' })}>{t.drain}</button>}
      {listing.state.kind === 'listed' && listing.state.status === 'draining' &&
        <button type="button" disabled={action.busy} onClick={() => run({ kind: 'resume' })}>{t.resume}</button>}
      {maintenance && <button type="button" className="secondary" disabled={action.busy}
        onClick={() => setAborting(maintenance)}>{t.abortMaintenance}</button>}
      {disabled && <button type="button" className="secondary" disabled={action.busy || Boolean(maintenance)} onClick={() => run({ kind: 'verify' })}>{t.verifyAgain}</button>}
      {listing.state.kind === 'listed' && listing.state.withdrawn.map(role => <button key={role} type="button" className="secondary" disabled={action.busy}
        onClick={() => run({ kind: 'restore_role', role })}>{t.restoreService(role.toUpperCase())}</button>)}
      <button type="button" className="secondary danger-text" disabled={action.busy || (disabled && Boolean(maintenance))}
        onClick={() => setPending({ kind: next, earlyCutoff: !node.cutoff_ready })}>{disabled ? t.removeNode : t.disable}</button>
    </div>
    <FleetConfirmDialogs pending={pending} aborting={aborting}
      onCancelPending={() => setPending(null)}
      onConfirmPending={confirmed => { setPending(null); run({ kind: confirmed.kind, early_cutoff: confirmed.earlyCutoff }) }}
      onCancelAbort={() => setAborting(null)}
      onConfirmAbort={maintenanceId => { run({ kind: 'abort_maintenance', maintenance_id: maintenanceId }); setAborting(null) }} />
    <Notice feedback={action.feedback} />
  </section>
}
