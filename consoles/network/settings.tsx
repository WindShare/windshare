import type { NumberField, usePolicyEditor } from './policy-editor'
import { ValidatedForm } from '../ui/ValidatedForm'
import { message, MessageError } from '../ui/locale'
import { useLocale } from '../ui/use-locale'
import { networkCopy } from './locales'
import { Notice } from '../ui/layout'
import { Icon } from '../ui/Icon'
import type { AuthorityClient } from './api'
import { GIB, type PolicySnapshot, type Settings, type Snapshot } from './model'

export function PolicySettings({ snapshot, api, saved, editor }: {
  editor: ReturnType<typeof usePolicyEditor>
  snapshot: Snapshot; api: AuthorityClient; saved: (policy: PolicySnapshot) => void
}) {
  const { locale } = useLocale()
  const t = networkCopy[locale]
  const { value, changes, showDifficulty, edit, discard, action } = editor
  const hasEdits = Object.keys(changes).length > 0
  const modes = { automatic: t.automatic, proof_of_work: t.proofOfWork, email: t.verifyEmail }
  function field(key: NumberField, label: string, min: number, max?: number) {
    return <div className="field"><label htmlFor={key}>{label}</label><input id={key} type="number" required min={min}
      step={key === 'gib' ? 'any' : 1} max={max ?? (key === 'gib' ? Number.MAX_SAFE_INTEGER / GIB : Number.MAX_SAFE_INTEGER)} value={value[key]}
      onChange={event => edit(key, event.target.value)} /></div>
  }
  return <div className="view"><div className="page-title"><h1>{t.networkPolicy}</h1>
    <dl className="account-count"><dt><Icon name="user" size={17} />{t.registeredAccounts}</dt>
      <dd>{snapshot.account_count.toLocaleString(locale)}</dd></dl></div>
    {snapshot.reviews.length > 0 && <p className="milestone">{t.milestone}</p>}
    <ValidatedForm className="panel padded" onSubmit={event => {
      event.preventDefault()
      void action.run(async () => {
        const { daily_bytes } = changes
        if (daily_bytes !== undefined && (!Number.isSafeInteger(daily_bytes) || daily_bytes <= 0)) {
          throw new MessageError(message(networkCopy, 'invalidAllowance'))
        }
        saved(await api.updateSettings(changes))
        discard()
        return message(networkCopy, 'settingsSaved')
      })
    }}><fieldset disabled={action.busy} className="policy-fields" aria-label={t.networkPolicy}>
        <fieldset className="stack"><legend>{t.registration}</legend>
          <div className="field"><label htmlFor="registration">{t.registrationMethod}</label>
            <select id="registration" value={value.mode}
              onChange={event => edit('mode', event.target.value as Settings['registration'])}>
              <option value="by_account_count">{t.byAccountCount}</option>
              <option value="automatic">{t.automatic}</option><option value="proof_of_work">{t.proofOfWork}</option>
              <option value="email" disabled={!snapshot.email_configured}>
                {snapshot.email_configured ? t.verifyEmail : t.mailRequired}</option>
            </select></div>
          <p className="muted">{t.defaultRegistrationMode(modes[snapshot.registration_mode])}</p>
          {showDifficulty && field('bits', t.difficulty, 16, 30)}
          {field('gib', t.allowance, 1 / GIB)}
        </fieldset>
        <fieldset className="rate-grid"><legend>{t.rateLimits}</legend>
          {field('sourceBurst', t.sourceBurst, 1)}{field('sourceRefill', t.sourceRefill, 1)}
          {field('globalBurst', t.networkBurst, 1)}{field('globalRefill', t.networkRefill, 1)}
        </fieldset>
        <div className="actions"><button disabled={!hasEdits || (value.mode === 'email' && !snapshot.email_configured)}>
          {action.busy ? t.saving : t.savePolicy}</button>
          {hasEdits && <button className="quiet" type="button" onClick={discard}>{t.discard}</button>}</div>
      </fieldset><Notice feedback={action.feedback} />
    </ValidatedForm></div>
}
