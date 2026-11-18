import { formValues, type Draft, type usePolicyEditor } from './policy-editor'
import { ValidatedForm } from '../ui/ValidatedForm'
import { message, MessageError } from '../ui/locale'
import { useLocale } from '../ui/use-locale'
import { networkCopy } from './locales'
import { Notice } from '../ui/layout'
import type { AuthorityClient } from './api'
import { GIB, type Settings, type Snapshot } from './model'

type NumberField = Exclude<keyof Draft, 'mode'>

export function PolicySettings({ snapshot, api, saved, editor }: {
  editor: ReturnType<typeof usePolicyEditor>
  snapshot: Snapshot; api: AuthorityClient; saved: (settings: Settings) => void
}) {
  const { locale } = useLocale()
  const t = networkCopy[locale]
  const { draft, setDraft, action } = editor
  const value = draft ?? formValues(snapshot.settings)
  function field(key: NumberField, label: string, min: number, max?: number) {
    return <div className="field"><label htmlFor={key}>{label}</label><input id={key} type="number" required min={min}
      step={key === 'gib' ? 'any' : 1} max={max ?? (key === 'gib' ? Number.MAX_SAFE_INTEGER / GIB : Number.MAX_SAFE_INTEGER)} value={value[key]}
      onChange={event => setDraft({ ...value, [key]: event.target.value })} /></div>
  }
  return <div className="view editor"><div className="page-title"><h1>{t.networkPolicy}</h1></div>
    <div className="panel padded account-count"><span>{t.registeredAccounts}</span><strong>{snapshot.account_count.toLocaleString(locale)}</strong>
      {snapshot.reviews.length > 0 && <p>{t.milestone}</p>}</div>
    <ValidatedForm className="panel padded" onSubmit={event => {
      event.preventDefault()
      void action.run(async () => {
        const settings: Settings = {
          registration: value.mode, pow_bits: Number(value.bits), daily_bytes: Math.round(Number(value.gib) * GIB),
          source_rate: { burst: Number(value.sourceBurst), refill_seconds: Number(value.sourceRefill) },
          global_rate: { burst: Number(value.globalBurst), refill_seconds: Number(value.globalRefill) },
        }
        if (!Number.isSafeInteger(settings.daily_bytes) || settings.daily_bytes <= 0) throw new MessageError(message(networkCopy, 'invalidAllowance'))
        await api.settings(settings)
        saved(settings)
        setDraft(null)
        return message(networkCopy, 'settingsSaved')
      })
    }}><fieldset disabled={action.busy} className="stack"><legend>{t.registration}</legend>
        <div className="field"><label htmlFor="registration">{t.registrationMethod}</label><select id="registration" value={value.mode}
          onChange={event => setDraft({ ...value, mode: event.target.value as Settings['registration'] })}>
          <option value="automatic">{t.automatic}</option><option value="proof_of_work">{t.proofOfWork}</option>
          <option value="email" disabled={!snapshot.email_configured}>
            {snapshot.email_configured ? t.verifyEmail : t.mailRequired}</option>
        </select></div>
        {value.mode === 'proof_of_work' && field('bits', t.difficulty, 16, 30)}
        {field('gib', t.allowance, 1 / GIB)}
        <fieldset className="rate-grid"><legend>{t.rateLimits}</legend>
          {field('sourceBurst', t.sourceBurst, 1)}{field('sourceRefill', t.sourceRefill, 1)}
          {field('globalBurst', t.networkBurst, 1)}{field('globalRefill', t.networkRefill, 1)}
        </fieldset>
        <div className="actions"><button disabled={!draft || (value.mode === 'email' && !snapshot.email_configured)}>
          {action.busy ? t.saving : t.savePolicy}</button>
          {draft && <button className="quiet" type="button" onClick={() => setDraft(null)}>{t.discard}</button>}</div>
      </fieldset><Notice feedback={action.feedback} />
    </ValidatedForm></div>
}
