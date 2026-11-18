import { useLayoutEffect, useId, useRef, useState, type ReactNode } from 'react'
import { useAction } from './activity'
import { FocusPage, Notice } from './layout'

function loginTitle(initialize: boolean, username?: boolean): string {
  if (!initialize) return 'Sign in'
  return username ? 'Create administrator' : 'Set owner password'
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
  const title = loginTitle(initialize, username)
  return <FocusPage label={area} title={title}>
    <form className="stack" method="post" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(() => submit(String(data.get('password')), String(data.get('username') ?? '')))
    }}>
      {username && <div className="field"><label htmlFor="username">Username</label>
        <input id="username" name="username" autoComplete="username" autoCapitalize="none" spellCheck={false}
          maxLength={64} pattern={initialize ? '[a-zA-Z0-9._\\-]+' : undefined} required /></div>}
      <div className="field"><label htmlFor="password">{initialize ? 'New password' : 'Password'}</label>
        <div className="password"><input id="password" name="password" type={visible ? 'text' : 'password'} required
          minLength={initialize ? 12 : undefined} maxLength={256} autoComplete={initialize ? 'new-password' : 'current-password'}
          aria-describedby={initialize ? 'password-minimum' : undefined} />
          <button className="quiet" type="button" aria-label={visible ? 'Hide password' : 'Show password'}
            onClick={() => setVisible(!visible)}>{visible ? 'Hide' : 'Show'}</button></div>
        {initialize && <small id="password-minimum">Use at least 12 characters.</small>}
      </div>
      <Notice feedback={action.feedback} />
      <button disabled={action.busy}>{action.busy ? 'Signing in…' : title}</button>
      {initialize && <button className="quiet" type="button" onClick={cancelSetup}>Use existing password</button>}
    </form>
  </FocusPage>
}

export function Confirm({ title, children, confirm, onCancel, onConfirm }: {
  title: string; children: ReactNode; confirm: string; onCancel: () => void; onConfirm: () => void
}) {
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
      <button type="button" ref={cancel} autoFocus className="secondary" onClick={onCancel}>Cancel</button>
      <button type="button" className="destructive" onClick={onConfirm}>{confirm}</button>
    </div>
  </dialog>
}
