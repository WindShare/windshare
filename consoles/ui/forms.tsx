import { useLayoutEffect, useId, useRef, useState, type ReactNode } from 'react'
import { ValidatedForm } from './ValidatedForm'
import { useCopy } from './use-locale'
import { sharedCopy } from './locales'
import { useAction } from './activity'
import { FocusPage, Notice } from './layout'

const MIN_PASSWORD_LENGTH = 12

function loginTitle(t: typeof sharedCopy.en, initialize: boolean, username?: boolean): string {
  if (!initialize) return t.signIn
  return username ? t.createAdmin : t.ownerSetupTitle
}

export function Login({ area, username, initialize, submit, cancelSetup }: {
  area: string
  username?: boolean
  initialize: boolean
  submit: (password: string, name: string) => Promise<void>
  cancelSetup: () => void
}) {
  const [visible, setVisible] = useState(false)
  const action = useAction()
  const t = useCopy(sharedCopy)
  const title = loginTitle(t, initialize, username)
  return <FocusPage label={area} title={title}>
    <ValidatedForm className="stack" method="post" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(() => submit(String(data.get('password')), String(data.get('username') ?? '')))
    }}>
      {username && <div className="field"><label htmlFor="username">{t.username}</label>
        <input id="username" name="username" autoComplete="username" autoCapitalize="none" spellCheck={false}
          maxLength={64} title={initialize ? t.usernameFormat : undefined} pattern={initialize ? '[a-zA-Z0-9._\\-]+' : undefined} required /></div>}
      <div className="field"><label htmlFor="password">{initialize ? t.newPassword : t.password}</label>
        <div className="password"><input id="password" name="password" type={visible ? 'text' : 'password'} required
          minLength={initialize ? MIN_PASSWORD_LENGTH : undefined} maxLength={256} autoComplete={initialize ? 'new-password' : 'current-password'}
          aria-describedby={initialize ? 'password-minimum' : undefined} />
          <button className="quiet" type="button" aria-label={visible ? t.hidePassword : t.showPassword}
            onClick={() => setVisible(!visible)}>{visible ? t.hide : t.show}</button></div>
        {initialize && <small id="password-minimum">{t.minLength(MIN_PASSWORD_LENGTH)}</small>}
      </div>
      <Notice feedback={action.feedback} />
      <button disabled={action.busy}>{action.busy ? t.signingIn : title}</button>
      {initialize && <button className="quiet" type="button" onClick={cancelSetup}>{t.usePassword}</button>}
    </ValidatedForm>
  </FocusPage>
}

export function Confirm({ title, children, confirm, onCancel, onConfirm }: {
  title: string; children: ReactNode; confirm: string; onCancel: () => void; onConfirm: () => void
}) {
  const t = useCopy(sharedCopy)
  const ref = useRef<HTMLDialogElement>(null)
  const cancel = useRef<HTMLButtonElement>(null)
  const id = useId()
  useLayoutEffect(() => {
    const dialog = ref.current!
    dialog.showModal()
    cancel.current?.focus()
    return () => dialog.close()
  }, [])
  return <dialog ref={ref} aria-labelledby={id} onCancel={event => { event.preventDefault(); onCancel() }}>
    <h2 id={id}>{title}</h2>{children}
    <div className="dialog-actions">
      <button type="button" ref={cancel} autoFocus className="secondary" onClick={onCancel}>{t.cancel}</button>
      <button type="button" className="destructive" onClick={onConfirm}>{confirm}</button>
    </div>
  </dialog>
}
