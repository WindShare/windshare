import { useState } from 'react'
import { useAction, useResource } from '../ui/activity'
import { Icon } from '../ui/Icon'
import { Notice, ReadError } from '../ui/layout'
import { message, MessageError } from '../ui/locale'
import { useCopy, useLocale } from '../ui/use-locale'
import { ValidatedForm } from '../ui/ValidatedForm'
import type { ContributionSettings, RelayClient } from './api'
import {
  BYTES_PER_MEGABIT,
  GIB,
  MAX_CONNECTIONS,
  MAX_DAILY_GIB,
  MAX_RATE_MBPS,
  draftFromPolicy,
  formatCeilingLimit,
  parseContributionDraft,
} from './contribution-policy'
import { relayCopy } from './locales'

function Limits({ api, settings }: { api: RelayClient; settings: ContributionSettings }) {
  const { locale } = useLocale()
  const t = relayCopy[locale]
  const action = useAction()
  const [draft, setDraft] = useState(() => draftFromPolicy(settings.policy))
  const limits = [
    { key: 'daily_bytes', label: t.dailyBudget, unit: GIB, unitLabel: 'GiB', min: 0, max: MAX_DAILY_GIB },
    { key: 'bytes_per_second', label: t.publicRate, unit: BYTES_PER_MEGABIT, unitLabel: 'Mbps', min: 1 / BYTES_PER_MEGABIT, max: MAX_RATE_MBPS },
    { key: 'connections', label: t.publicConnections, unit: 1, unitLabel: undefined, min: 1, max: MAX_CONNECTIONS },
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
      <label className="check"><input type="checkbox" role="switch" checked={draft.enabled}
        onChange={event => setDraft(current => ({ ...current, enabled: event.target.checked }))} />{t.contribution}</label>
      {limits.map(({ key, label, unit, unitLabel, min, max }) => <label className="field" key={key}>{label}
        <input type="number" required min={min} max={max} step={unit === 1 ? 1 : 'any'} value={draft[key]}
          onChange={event => setDraft(current => ({ ...current, [key]: event.target.value }))} />
        {settings.ceiling[key] != null && <small>{t.ceiling} {formatCeilingLimit(settings.ceiling[key], unit, unitLabel, locale)}</small>}
      </label>)}
      <label className="field">{t.hostUplink}
        <input type="number" min={1 / BYTES_PER_MEGABIT} max={MAX_RATE_MBPS} step="any" value={draft.host_uplink}
          onChange={event => setDraft(current => ({ ...current, host_uplink: event.target.value }))} />
      </label>
      <div className="actions"><button type="submit">{t.saveLimits}</button></div>
    </fieldset>
    <Notice feedback={action.feedback} />
  </ValidatedForm>
}

export function Contribution({ api }: { api: RelayClient }) {
  const t = useCopy(relayCopy)
  const resource = useResource(api.contribution)
  return <section className="panel padded">
    <ReadError error={resource.error} retry={resource.retry} />
    {!resource.value && resource.busy && <p className="pending" role="status"><Icon name="loader" />{t.loadingContribution}</p>}
    {resource.value && <Limits api={api} settings={resource.value} />}
  </section>
}
