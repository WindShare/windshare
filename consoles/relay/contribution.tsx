import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Notice, ReadError } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import { message } from '../ui/locale'
import { relayCopy } from './locales'
import type { ContributionSettings, RelayClient } from './api'

const GIB = 1024 ** 3
const BYTES_PER_MEGABIT = 125_000

function Limits({ api, settings }: { api: RelayClient; settings: ContributionSettings }) {
  const t = useCopy(relayCopy)
  const action = useAction()
  const [policy, setPolicy] = useState(settings.policy)
  const limits = [
    { key: 'daily_bytes', label: t.dailyBudget, unit: GIB, min: 0 },
    { key: 'bytes_per_second', label: t.publicRate, unit: BYTES_PER_MEGABIT, min: 1 },
    { key: 'connections', label: t.publicConnections, unit: 1, min: 1 },
  ] as const
  return <form className="stack" onSubmit={event => {
    event.preventDefault()
    void action.run(async () => {
      const receipt = await api.saveContribution(policy)
      return message(relayCopy, receipt.disabled_notice ? 'disabledNotice' : 'limitsSaved')
    })
  }}>
    <label><input type="checkbox" role="switch" checked={policy.enabled} disabled={action.busy}
      onChange={event => setPolicy({ ...policy, enabled: event.target.checked })} /> {t.contribution}</label>
    {limits.map(({ key, label, unit, min }) => <label className="field" key={key}>{label}
      <input type="number" required min={min / unit} step={unit === 1 ? 1 : 'any'} value={policy[key] / unit}
        onChange={event => setPolicy({ ...policy, [key]: Math.round(Number(event.target.value) * unit) })} />
      {settings.ceiling[key] != null && <small>{t.ceiling} {settings.ceiling[key] / unit}</small>}
    </label>)}
    <label className="field">{t.hostUplink}
      <input type="number" min={1 / BYTES_PER_MEGABIT} step="any" value={policy.host_uplink === null ? '' : policy.host_uplink / BYTES_PER_MEGABIT}
        onChange={event => setPolicy({ ...policy, host_uplink: event.target.value ? Math.round(Number(event.target.value) * BYTES_PER_MEGABIT) : null })} />
    </label>
    <button type="submit" className="primary" disabled={action.busy}>{t.saveLimits}</button>
    <Notice feedback={action.feedback} />
  </form>
}

export function Contribution({ api }: { api: RelayClient }) {
  const resource = useResource(api.contribution)
  return <section className="panel">
    <ReadError error={resource.error} retry={resource.retry} />
    {resource.value && <Limits api={api} settings={resource.value} />}
  </section>
}
