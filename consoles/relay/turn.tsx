import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Badge, Notice, ReadError } from '../ui/layout'
import { message } from '../ui/locale'
import { useLocale } from '../ui/use-locale'
import { sharedCopy } from '../ui/locales'
import { relayCopy } from './locales'
import type { RelayClient, TurnStatus, TurnUsage } from './api'

const leaseLabels = {
  active: 'active', revocation_pending: 'revocationPending', revoked: 'revoked', expired: 'expired',
} as const satisfies Record<keyof TurnStatus['leases'], keyof typeof relayCopy.en>

function describeTurnStatus(current: TurnStatus, t: typeof relayCopy.en): { label: string; description: string } {
  if (!current.ready) {
    return { label: t.setupIncomplete, description: t.finishTurnSetup }
  }
  if (current.enabled) {
    return {
      label: t.enabled,
      description: t.turnEnabledHint,
    }
  }
  return { label: t.disabled, description: t.turnDisabledHint }
}

function UsageEstimate({ estimate }: { estimate: TurnUsage }) {
  const { locale } = useLocale()
  // Analytics covers whole UTC days, including for administrators west of UTC.
  const dates = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeZone: 'UTC' })
  const range = dates.formatRange(new Date(estimate.first_date), new Date(estimate.last_date))
  return <p>{range} UTC<br />
    {relayCopy[locale].traffic(estimate.egress_bytes.toLocaleString(locale), estimate.ingress_bytes.toLocaleString(locale))}</p>
}

export function ManagedTurn({ api }: { api: RelayClient }) {
  const { locale } = useLocale()
  const t = relayCopy[locale]
  const status = useResource(api.turnStatus)
  const action = useAction()
  const usage = useAction()
  const [estimate, setEstimate] = useState<TurnUsage | null>(null)
  const current = status.value
  const summary = current ? describeTurnStatus(current, t) : null
  return <div className="workspace narrow">
    <div className="page-title"><h1>{t.managedTurn}</h1>
      <button className="secondary" onClick={status.reload} disabled={status.busy}>{sharedCopy[locale].refresh}</button></div>
    <ReadError error={status.error} retry={status.reload} />
    {!current && status.busy && <p>{t.loadingSettings}</p>}
    {current && summary && <>
      <section className="panel padded"><div className="section-title"><h2>{t.service}</h2>
        <Badge tone={current.ready && current.enabled ? 'good' : 'neutral'}>{summary.label}</Badge></div>
        <p>{summary.description}</p>
        <button className="secondary" disabled={action.busy} onClick={() => void action.run(async () => {
          const next = await api.setTurn(!current.enabled)
          status.update(() => next)
          return message(relayCopy, 'turnSaved')
        })}>{current.enabled ? t.disableTurn : t.enableTurn}</button>
        <Notice feedback={action.feedback} />
      </section>
      <section className="panel padded"><h2>{t.leases}</h2><dl className="metrics">
        {Object.entries(leaseLabels).map(([key, label]) => <div key={key}><dt>{t[label]}</dt>
          <dd>{(current.leases[key as keyof typeof leaseLabels] ?? 0).toLocaleString(locale)}</dd></div>)}
      </dl></section>
      {current.ready && current.analytics_available && <section className="panel padded"><h2>{t.estimatedUsage}</h2>
        {estimate && <UsageEstimate estimate={estimate} />}
        <button className="secondary" disabled={usage.busy} onClick={() => void usage.run(async () => setEstimate(await api.turnUsage()))}>
          {usage.busy ? t.loading : t.queryUsage}</button><Notice feedback={usage.feedback} />
      </section>}
    </>}
  </div>
}
