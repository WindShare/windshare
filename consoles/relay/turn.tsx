import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Badge, Notice, ReadError } from '../ui/layout'
import type { RelayClient, TurnStatus, TurnUsage } from './api'

const leaseLabels = {
  active: 'Active', revocation_pending: 'Revocation pending', revoked: 'Revoked', expired: 'Expired',
} as const

function describeTurnStatus(current: TurnStatus): { label: string; description: string } {
  if (!current.ready) {
    return { label: 'Setup incomplete', description: 'Rerun deployment to finish TURN setup.' }
  }
  if (current.enabled) {
    return {
      label: 'Enabled',
      description: 'Devices can request TURN for previews and downloads. Disabling TURN withdraws active leases.',
    }
  }
  return { label: 'Disabled', description: 'New TURN leases are paused.' }
}

export function ManagedTurn({ api }: { api: RelayClient }) {
  const status = useResource(api.turnStatus)
  const action = useAction()
  const usage = useAction()
  const [estimate, setEstimate] = useState<TurnUsage | null>(null)
  const current = status.value
  const summary = current ? describeTurnStatus(current) : null
  return <div className="workspace narrow">
    <div className="page-title"><h1>Managed TURN</h1>
      <button className="secondary" onClick={status.reload} disabled={status.busy}>Refresh</button></div>
    <ReadError error={status.error} retry={status.reload} />
    {!current && status.busy && <p>Loading settings…</p>}
    {current && summary && <>
      <section className="panel padded"><div className="section-title"><h2>Service</h2>
        <Badge tone={current.ready && current.enabled ? 'good' : 'neutral'}>{summary.label}</Badge></div>
        <p>{summary.description}</p>
        <button className="secondary" disabled={action.busy} onClick={() => void action.run(async () => {
          const next = await api.setTurn(!current.enabled)
          status.update(() => next)
          return 'TURN settings saved.'
        })}>{current.enabled ? 'Disable TURN' : 'Enable TURN'}</button>
        <Notice feedback={action.feedback} />
      </section>
      <section className="panel padded"><h2>Leases</h2><dl className="metrics">
        {Object.entries(leaseLabels).map(([key, label]) => <div key={key}><dt>{label}</dt>
          <dd>{(current.leases[key as keyof typeof leaseLabels] ?? 0).toLocaleString()}</dd></div>)}
      </dl></section>
      {current.ready && current.analytics_available && <section className="panel padded"><h2>Estimated usage</h2>
        {estimate && <p>{estimate.first_date} – {estimate.last_date} UTC<br />
          {estimate.egress_bytes.toLocaleString()} bytes out · {estimate.ingress_bytes.toLocaleString()} bytes in</p>}
        <button className="secondary" disabled={usage.busy} onClick={() => void usage.run(async () => setEstimate(await api.turnUsage()))}>
          {usage.busy ? 'Loading…' : 'Query usage'}</button><Notice feedback={usage.feedback} />
      </section>}
    </>}
  </div>
}
