import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { BackLink, Notice } from '../ui/layout'
import { FocusPage } from '../ui/FocusPage'
import { Icon } from '../ui/Icon'
import { relayCopy } from './locales'
import type { ServiceKeyPhase, useServiceKeyCreation } from './use-service-key-creation'

export function ServiceKeyEntry({ phase }: { phase: ServiceKeyPhase }) {
  const t = useCopy(relayCopy)
  const entry = {
    idle: { icon: 'plus', label: t.createServiceKey },
    creating: { icon: 'loader', label: t.creatingServiceKey },
    ready: { icon: 'key', label: t.viewNewKey },
  } as const
  const { icon, label } = entry[phase]
  return <a className="button secondary" href="#service-key"><Icon name={icon} size={16} />
    <span aria-live="polite">{label}</span>
  </a>
}

export function ServiceKey({ creation }: { creation: ReturnType<typeof useServiceKeyCreation> }) {
  const t = useCopy(relayCopy)
  const copy = useAction()
  const { state } = creation
  if (state.phase === 'ready') {
    const { credential } = state
    return <FocusPage icon="check" label={t.access} title={t.keyCreated}>
      <p className="lead">{t.saveKeyBeforeFinishing}</p>
      <dl className="facts"><dt>{t.relayAddress}</dt><dd><code>{credential.url}</code></dd>
        <dt>{t.accessKey}</dt><dd><code>{credential.access_key}</code></dd></dl>
      <button type="button" disabled={copy.busy} onClick={() => void copy.run(async () => {
        try {
          await navigator.clipboard.writeText(JSON.stringify(credential, null, 2))
        } catch {
          throw new MessageError(message(relayCopy, 'copyFailed'))
        }
        return message(relayCopy, 'relayCredentialCopied')
      })}><Icon name="copy" size={16} />{t.copyRelayCredential}</button>
      <p>{t.installWithCLI}<br /><code>{'wind relay add <url> --access-key <key>'}</code></p>
      <Notice feedback={copy.feedback} />
      <button type="button" className="secondary" onClick={() => {
        creation.finish()
        location.hash = 'access'
      }}>{t.finishKeyCreation}</button>
    </FocusPage>
  }

  const pending = state.phase === 'creating'
  return <FocusPage icon="key" label={t.access} title={t.createServiceKey}>
    <ValidatedForm className="stack" onSubmit={event => {
      event.preventDefault()
      const name = String(new FormData(event.currentTarget).get('name')).trim()
      creation.create(name)
    }}>
      <div className="field"><label htmlFor="key-name">{t.credentialName}</label>
        <input id="key-name" name="name" maxLength={128} required disabled={pending} /></div>
      <button disabled={pending}>{pending ? t.creating : t.createKey}</button>
    </ValidatedForm>
    <Notice feedback={creation.feedback} /><BackLink href="#access">{t.backToAccess}</BackLink>
  </FocusPage>
}
