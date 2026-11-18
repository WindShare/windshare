import { useEffect, useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction, useResource } from '../ui/activity'
import { BackLink, Notice, ReadError } from '../ui/layout'
import { FocusPage } from '../ui/FocusPage'
import { Icon } from '../ui/Icon'
import { useCopy } from '../ui/use-locale'
import { relayCopy } from './locales'
import { RequestError } from '../ui/request'
import type { RelayClient } from './api'

export function CodeEntry() {
  const t = useCopy(relayCopy)
  return <section className="panel pairing-entry" aria-labelledby="pair-heading">
    <div className="pairing-intro"><span className="pairing-icon"><Icon name="pair" size={26} /></span>
      <div><h2 id="pair-heading">{t.pairDevice}</h2><p id="pair-hint">{t.codeHint}</p></div></div>
    <ValidatedForm action="/" method="get" className="pairing-form">
      <label className="sr-only" htmlFor="pair-code">{t.deviceCode}</label>
      <input id="pair-code" name="code" className="code-input" aria-describedby="pair-hint" placeholder="XXXX-XXXX" maxLength={9}
        autoComplete="off" autoCapitalize="characters" spellCheck={false} required />
      <button>{t.reviewDevice}<Icon name="arrow" size={16} /></button>
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
  if (approved) return <FocusPage icon="check" label={t.pairing} title={t.approved}>
    <p className="lead" role="status">{t.approvedHint}</p>
    <BackLink href="/">{t.backToAccess}</BackLink>
  </FocusPage>
  if (expired && !action.busy) return <FocusPage icon="clock" label={t.pairing} title={t.pairingExpired}>
    <p className="lead">{t.restartPairing}</p><BackLink href="/">{t.backToAccess}</BackLink>
  </FocusPage>
  if (!device && requests.error) return <FocusPage icon="alert" label={t.pairing} title={t.lookupFailed}>
    <ReadError error={requests.error} retry={requests.reload} /><BackLink href="/">{t.differentCode}</BackLink>
  </FocusPage>
  if (!device) return <FocusPage icon={requests.busy ? 'loader' : 'pair'} label={t.pairing} title={requests.busy ? t.lookingUpDevice : t.noRequest}>
    {requests.value && <><p className="lead">{t.checkCode}</p>
      <button className="secondary" disabled={requests.busy} onClick={requests.reload}>{t.checkAgain}</button></>}
    <BackLink href="/">{t.differentCode}</BackLink>
  </FocusPage>
  return <FocusPage icon="pair" label={t.pairing} title={device.device_name}>
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
    })}><Icon name={action.busy ? 'loader' : 'check'} size={17} />{action.busy ? t.approving : t.approveDevice}</button>
  </FocusPage>
}
