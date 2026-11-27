import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Notice, ReadError } from '../ui/layout'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { ValidatedForm } from '../ui/ValidatedForm'
import type { ContributionSettings, RelayClient } from './api'
import {
  BYTES_PER_MEGABIT,
  GIB,
  MAX_CONNECTIONS,
  MAX_DAILY_GIB,
  MAX_RATE_MBPS,
  draftFromPolicy,
  parseContributionDraft,
} from './contribution-policy'
import { relayCopy } from './locales'

function Limits({ api, settings }: { api: RelayClient; settings: ContributionSettings }) {
  const t = useCopy(relayCopy)
  const action = useAction()
  const [draft, setDraft] = useState(() => draftFromPolicy(settings.policy))
  const limits = [
    { key: 'daily_bytes', label: t.dailyBudget, unit: GIB, min: 0, max: MAX_DAILY_GIB },
    { key: 'bytes_per_second', label: t.publicRate, unit: BYTES_PER_MEGABIT, min: 1 / BYTES_PER_MEGABIT, max: MAX_RATE_MBPS },
    { key: 'connections', label: t.publicConnections, unit: 1, min: 1, max: MAX_CONNECTIONS },
  ] as const
  return <ValidatedForm className="stack" onSubmit={event => {
    event.preventDefault()
    void action.run(async () => {
      const policy = parseContributionDraft(draft)
      if (!policy) throw new MessageError(message(relayCopy, 'invalidValues'))
      const receipt = await api.saveContribution(policy)
      setDraft(draftFromPolicy(policy))
      return message(relayCopy, receipt.disabled_notice ? 'disabledNotice' : 'limitsSaved')
    })
  }}>
    <fieldset disabled={action.busy} className="stack">
      <label><input type="checkbox" role="switch" checked={draft.enabled}
        onChange={event => setDraft(current => ({ ...current, enabled: event.target.checked }))} /> {t.contribution}</label>
      {limits.map(({ key, label, unit, min, max }) => <label className="field" key={key}>{label}
        <input type="number" required min={min} max={max} step={unit === 1 ? 1 : 'any'} value={draft[key]}
          onChange={event => setDraft(current => ({ ...current, [key]: event.target.value }))} />
        {settings.ceiling[key] != null && <small>{t.ceiling} {settings.ceiling[key] / unit}</small>}
      </label>)}
      <label className="field">{t.hostUplink}
        <input type="number" min={1 / BYTES_PER_MEGABIT} max={MAX_RATE_MBPS} step="any" value={draft.host_uplink}
          onChange={event => setDraft(current => ({ ...current, host_uplink: event.target.value }))} />
      </label>
      <button type="submit" className="primary">{t.saveLimits}</button>
    </fieldset>
    <Notice feedback={action.feedback} />
  </ValidatedForm>
}

export function Contribution({ api }: { api: RelayClient }) {
  const resource = useResource(api.contribution)
  return <section className="panel">
    <ReadError error={resource.error} retry={resource.retry} />
    {resource.value && <Limits api={api} settings={resource.value} />}
  </section>
}
