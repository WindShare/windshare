import { useLayoutEffect, useId, useRef, useState, type ReactNode } from 'react'
import { ValidatedForm } from './ValidatedForm'
import { useCopy } from './use-locale'
import { message, MessageError } from './locale'
import { sharedCopy } from './locales'
import { useAction } from './activity'
import { Notice } from './layout'
import { EntryPage } from './FocusPage'
import { Icon } from './Icon'

const MIN_PASSWORD_LENGTH = 12
const MAX_PASSWORD_LENGTH = 256

function loginTitle(t: typeof sharedCopy.en, initialize: boolean, username?: boolean): string {
  if (!initialize) return t.signIn
  return username ? t.createAdmin : t.ownerSetupTitle
}

function PasswordField({ id, name, label, visible, onToggle, autoComplete, minLength, hint, disabled }: {
  id: string
  name: string
  label: string
  visible: boolean
  onToggle?: () => void
  autoComplete: 'current-password' | 'new-password'
  minLength?: number
  hint?: string
  disabled?: boolean
}) {
  const t = useCopy(sharedCopy)
  const hintId = hint ? `${id}-hint` : undefined
  const toggleLabel = visible ? t.hidePassword : t.showPassword
  return <div className="field"><label htmlFor={id}>{label}</label>
    <div className={onToggle ? 'password' : undefined}>
      <input id={id} name={name} type={visible ? 'text' : 'password'} required disabled={disabled}
        minLength={minLength} maxLength={MAX_PASSWORD_LENGTH} autoComplete={autoComplete}
        aria-describedby={hintId} />
      {onToggle && <button className="quiet reveal" type="button" aria-label={toggleLabel}
        title={toggleLabel} onClick={onToggle}>
        <Icon name={visible ? 'eyeOff' : 'eye'} size={18} /></button>}
    </div>
    {hint && <small id={hintId}>{hint}</small>}
  </div>
}

export function Login({ area, username, initialize, expired, submit, cancelSetup }: {
  area: string
  username?: boolean
  initialize: boolean
  expired?: boolean
  submit: (password: string, name: string) => Promise<void>
  cancelSetup: () => void
}) {
  const [visible, setVisible] = useState(false)
  const action = useAction()
  const t = useCopy(sharedCopy)
  const title = loginTitle(t, initialize, username)
  const feedback = action.feedback ?? (expired && !initialize
    ? { kind: 'warning' as const, message: message(sharedCopy, 'sessionExpired') }
    : null)
  return <EntryPage icon="shield" label={area} title={title}>
    <ValidatedForm className="stack" method="post" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(async () => {
        const password = String(data.get('password'))
        if (initialize && password !== String(data.get('confirmPassword') ?? '')) {
          throw new MessageError(message(sharedCopy, 'confirmationMismatch'))
        }
        await submit(password, String(data.get('username') ?? ''))
      })
    }}>
      {username && <div className="field"><label htmlFor="username">{t.username}</label>
        <input id="username" name="username" autoComplete="username" autoCapitalize="none" spellCheck={false}
          maxLength={64} title={initialize ? t.usernameFormat : undefined} pattern={initialize ? '[a-zA-Z0-9._\\-]+' : undefined} required /></div>}
      <PasswordField id="password" name="password" label={initialize ? t.newPassword : t.password}
        visible={visible} onToggle={() => setVisible(!visible)}
        autoComplete={initialize ? 'new-password' : 'current-password'}
        minLength={initialize ? MIN_PASSWORD_LENGTH : undefined}
        hint={initialize ? t.minLength(MIN_PASSWORD_LENGTH) : undefined} />
      {initialize && <PasswordField id="confirm-password" name="confirmPassword" label={t.confirmPassword}
        visible={visible} autoComplete="new-password" minLength={MIN_PASSWORD_LENGTH} />}
      <Notice feedback={feedback} />
      <button disabled={action.busy}>{action.busy ? t.signingIn : title}<Icon name={action.busy ? 'loader' : 'arrow'} size={16} /></button>
      {initialize && <button className="quiet" type="button" onClick={cancelSetup}>{t.usePassword}</button>}
    </ValidatedForm>
  </EntryPage>
}

export function ChangePasswordDialog({ onCancel, onSubmit }: {
  onCancel: () => void
  onSubmit: (currentPassword: string, newPassword: string) => Promise<void>
}) {
  const [visible, setVisible] = useState(false)
  const action = useAction()
  const t = useCopy(sharedCopy)
  const ref = useRef<HTMLDialogElement>(null)
  const id = useId()
  useLayoutEffect(() => {
    const dialog = ref.current!
    dialog.showModal()
    return () => dialog.close()
  }, [])
  return <dialog ref={ref} aria-labelledby={id} onCancel={event => {
    event.preventDefault()
    if (!action.busy) onCancel()
  }}>
    <h2 id={id}>{t.changePassword}</h2>
    <ValidatedForm className="stack dialog-form" method="post" onSubmit={event => {
      event.preventDefault()
      const data = new FormData(event.currentTarget)
      void action.run(async () => {
        const currentPassword = String(data.get('currentPassword'))
        const newPassword = String(data.get('newPassword'))
        if (newPassword !== String(data.get('confirmPassword') ?? '')) {
          throw new MessageError(message(sharedCopy, 'confirmationMismatch'))
        }
        await onSubmit(currentPassword, newPassword)
      })
    }}>
      <PasswordField id="current-password" name="currentPassword" label={t.currentPassword}
        visible={visible} onToggle={() => setVisible(!visible)} autoComplete="current-password" disabled={action.busy} />
      <PasswordField id="new-password" name="newPassword" label={t.newPassword}
        visible={visible} autoComplete="new-password" minLength={MIN_PASSWORD_LENGTH}
        hint={t.minLength(MIN_PASSWORD_LENGTH)} disabled={action.busy} />
      <PasswordField id="confirm-new-password" name="confirmPassword" label={t.confirmPassword}
        visible={visible} autoComplete="new-password" minLength={MIN_PASSWORD_LENGTH} disabled={action.busy} />
      <Notice feedback={action.feedback} />
      <div className="dialog-actions">
        <button type="button" className="secondary" disabled={action.busy} onClick={onCancel}>{t.cancel}</button>
        <button disabled={action.busy}>
          {action.busy && <Icon name="loader" size={16} />}
          {action.busy ? t.updating : t.savePassword}
        </button>
      </div>
    </ValidatedForm>
  </dialog>
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
