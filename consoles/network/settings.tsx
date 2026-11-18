import { formValues, type Draft, type usePolicyEditor } from './policy-editor'
import { Notice } from '../ui/layout'
import type { AuthorityClient } from './api'
import { GIB, type Settings, type Snapshot } from './model'

type NumberField = Exclude<keyof Draft, 'mode'>

export function PolicySettings({ snapshot, api, saved, editor }: {
  editor: ReturnType<typeof usePolicyEditor>
  snapshot: Snapshot; api: AuthorityClient; saved: (settings: Settings) => void
}) {
  const { draft, setDraft, action } = editor
  const value = draft ?? formValues(snapshot.settings)
  function field(key: NumberField, label: string, min: number, max?: number) {
    return <div className="field"><label htmlFor={key}>{label}</label><input id={key} type="number" required min={min}
      step={key === 'gib' ? 'any' : 1} max={max ?? (key === 'gib' ? Number.MAX_SAFE_INTEGER / GIB : Number.MAX_SAFE_INTEGER)} value={value[key]}
      onChange={event => setDraft({ ...value, [key]: event.target.value })} /></div>
  }
  return <div className="view editor"><div className="page-title"><h1>Network policy</h1></div>
    <div className="panel padded account-count"><span>Registered accounts</span><strong>{snapshot.account_count.toLocaleString()}</strong>
      {snapshot.reviews.length > 0 && <p>A registration milestone has been reached. Review registration policy and node capacity.</p>}</div>
    <form className="panel padded" onSubmit={event => {
      event.preventDefault()
      void action.run(async () => {
        const settings: Settings = {
          registration: value.mode, pow_bits: Number(value.bits), daily_bytes: Math.round(Number(value.gib) * GIB),
          source_rate: { burst: Number(value.sourceBurst), refill_seconds: Number(value.sourceRefill) },
          global_rate: { burst: Number(value.globalBurst), refill_seconds: Number(value.globalRefill) },
        }
        if (!Number.isSafeInteger(settings.daily_bytes) || settings.daily_bytes <= 0) throw new Error('Enter a daily allowance within the supported range.')
        await api.settings(settings)
        saved(settings)
        setDraft(null)
        return 'Network policy saved.'
      })
    }}><fieldset disabled={action.busy} className="stack"><legend>Account registration</legend>
        <div className="field"><label htmlFor="registration">Registration method</label><select id="registration" value={value.mode}
          onChange={event => setDraft({ ...value, mode: event.target.value as Settings['registration'] })}>
          <option value="automatic">Automatic</option><option value="proof_of_work">Proof of work</option>
          <option value="email" disabled={!snapshot.email_configured}>
            {snapshot.email_configured ? 'Email verification' : 'Email verification · mail service required'}</option>
        </select></div>
        {value.mode === 'proof_of_work' && field('bits', 'Proof-of-work difficulty (bits)', 16, 30)}
        {field('gib', 'Daily relay allowance per account (GiB)', 1 / GIB)}
        <fieldset className="rate-grid"><legend>Registration rate limits</legend>
          {field('sourceBurst', 'Burst per source', 1)}{field('sourceRefill', 'Seconds per new account, per source', 1)}
          {field('globalBurst', 'Network-wide burst', 1)}{field('globalRefill', 'Seconds per new account, network-wide', 1)}
        </fieldset>
        <div className="actions"><button disabled={!draft || (value.mode === 'email' && !snapshot.email_configured)}>
          {action.busy ? 'Saving…' : 'Save policy'}</button>
          {draft && <button className="quiet" type="button" onClick={() => setDraft(null)}>Discard changes</button>}</div>
      </fieldset><Notice feedback={action.feedback} />
    </form></div>
}
