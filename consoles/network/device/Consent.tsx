import { useState } from 'react'
import { FocusPage } from '../../ui/FocusPage'
import { Frame, Notice, SessionWait } from '../../ui/layout'
import { ValidatedForm } from '../../ui/ValidatedForm'
import { useAction, useResource } from '../../ui/activity'
import { useCopy } from '../../ui/use-locale'
import { message, MessageError } from '../../ui/locale'
import { request } from '../../ui/request'
import { sharedCopy } from '../../ui/locales'
import { deviceCopy, deviceErrors } from '../../ui/locales/device'
import { takeFragment } from '../../ui/location'
import { derivePasswordVerifier, isValidLoginPassword, normalizeUsername } from '../auth/password'

const invitation = takeFragment('invite') ?? ''
interface Preview { origin: string; device: string; code: string; username: string | null }
const preview = (signal: AbortSignal) => request<Preview>('/v1/device/preview', { signal, data: { invitation }, messages: deviceErrors })

function Consent({ device }: { device: Preview }) {
  const t = useCopy(deviceCopy)
  const fields = useCopy(sharedCopy)
  const [enterLogin, setEnterLogin] = useState(!device.username)
  const action = useAction()
  const approveLabel = enterLogin ? t.approve : `${t.approveAs} ${device.username}`
  return <FocusPage icon="pair" title={device.device} label={new URL(device.origin).host}>
    <div className="pair-code"><span>{t.match}</span><strong>{device.code}</strong></div>
    <ValidatedForm className="stack" method="post" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      const enteredUsername = String(data.get('username') ?? '')
      const enteredPassword = String(data.get('password') ?? '')
      void action.run(async () => {
        let login: { username: string; password_verifier: string } | undefined
        if (enterLogin) {
          const canonical = normalizeUsername(enteredUsername)
          if (!canonical || !isValidLoginPassword(enteredPassword)) {
            throw new MessageError(message(deviceCopy, 'denied'))
          }
          login = {
            username: canonical,
            password_verifier: await derivePasswordVerifier('account', canonical, enteredPassword),
          }
        }
        const { approval } = await request<{ approval: string }>('/v1/device/approve', { data: { invitation, login, username: enterLogin ? undefined : device.username }, messages: deviceErrors })
        const destination = new URL('/device/complete', device.origin)
        destination.hash = new URLSearchParams({ approval }).toString()
        location.replace(destination)
      })
    }}>
      {enterLogin && <>
        <div className="field"><label htmlFor="username">{fields.username}</label>
          <input id="username" name="username" autoComplete="username" autoCapitalize="none" spellCheck={false} maxLength={64} required /></div>
        <div className="field"><label htmlFor="password">{fields.password}</label>
          <input id="password" name="password" type="password" autoComplete="current-password" maxLength={256} required /></div>
      </>}
      <Notice feedback={action.feedback} />
      <button disabled={action.busy}>{action.busy ? t.working : approveLabel}</button>
      {!enterLogin && <button className="quiet" type="button" disabled={action.busy} onClick={() => setEnterLogin(true)}>{t.switch}</button>}
    </ValidatedForm>
  </FocusPage>
}

export function DevicePage() {
  const t = useCopy(deviceCopy)
  const state = useResource(preview)
  return <Frame area={t.title}>{state.value ? <Consent device={state.value} /> : <SessionWait error={state.error} retry={state.reload} />}</Frame>
}
