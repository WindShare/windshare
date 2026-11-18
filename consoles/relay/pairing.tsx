import { useEffect, useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { FocusPage, Notice, ReadError } from '../ui/layout'
import { useCopy } from '../ui/use-locale'
import { relayCopy } from './locales'
import { RequestError } from '../ui/request'
import type { RelayClient } from './api'

export function CodeEntry() {
  const t = useCopy(relayCopy)
  return <section className="panel pairing-entry" aria-labelledby="pair-heading">
    <div><h2 id="pair-heading">{t.pairDevice}</h2><p>{t.codeHint}</p></div>
    <ValidatedForm action="/" method="get" className="inline-form">
      <label className="sr-only" htmlFor="pair-code">{t.deviceCode}</label>
      <input id="pair-code" name="code" className="code-input" placeholder="XXXX-XXXX" maxLength={9}
        autoComplete="off" autoCapitalize="characters" spellCheck={false} required />
      <button>{t.reviewDevice}</button>
    </ValidatedForm>
  </section>
}

export function Pairing({ api, code }: { api: RelayClient; code: string }) {
  const t = useCopy(relayCopy)
  const requests = useResource(api.enrollments)
  const device = requests.value?.find(entry => entry.user_code === code)
  const [approved, setApproved] = useState(false)
  const [expired, setExpired] = useState(false)
  const action = useAction()
  useEffect(() => {
    if (!device) return
    const timer = window.setTimeout(() => setExpired(true), Math.max(0, device.expires_at - Date.now()))
    return () => window.clearTimeout(timer)
  }, [device])
  if (approved) return <FocusPage label={t.pairing} title={t.approved}>
    <p className="lead" role="status">{t.approvedHint}</p>
    <a href="/">{t.backToAccess}</a>
  </FocusPage>
  if (expired && !action.busy) return <FocusPage label={t.pairing} title={t.pairingExpired}>
    <p className="lead">{t.restartPairing}</p><a href="/">{t.backToAccess}</a>
  </FocusPage>
  if (!device) return <FocusPage label={t.pairing} title={requests.value ? t.noRequest : t.lookingUpDevice}>
    {requests.value && <p className="lead">{t.checkCode}</p>}
    <ReadError error={requests.error} retry={requests.reload} />
    {requests.value && !requests.error && <button className="secondary" disabled={requests.busy} onClick={requests.reload}>{t.checkAgain}</button>}
    <div className="foot-link"><a href="/">{t.differentCode}</a></div>
  </FocusPage>
  return <FocusPage label={t.pairing} title={device.device_name}>
    <p className="lead">{t.matchCode}</p>
    <div className="pair-code"><span>{t.deviceCode}</span><strong>{device.user_code}</strong></div>
    <Notice feedback={action.feedback} />
    <button className="wide" disabled={action.busy} onClick={() => void action.run(async () => {
      try {
        await api.approve(device)
        setApproved(true)
      } catch (error) {
        if (error instanceof RequestError && error.code === 'expired_token') setExpired(true)
        else throw error
      }
    })}>{action.busy ? t.approving : t.approveDevice}</button>
  </FocusPage>
}
