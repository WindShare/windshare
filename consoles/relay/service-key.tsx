import { useState } from 'react'
import { ValidatedForm } from '../ui/ValidatedForm'
import { useAction } from '../ui/activity'
import { message, MessageError } from '../ui/locale'
import { useCopy } from '../ui/use-locale'
import { BackLink, Notice } from '../ui/layout'
import { FocusPage } from '../ui/FocusPage'
import { Icon } from '../ui/Icon'
import { relayCopy } from './locales'
import { formatRelayAddCommand } from './service-key-command'
import type { ServiceKeyPhase, useServiceKeyCreation } from './use-service-key-creation'

type CopyFieldId = 'url' | 'key' | 'cli'
type CopyFeedbackKey = 'relayAddressCopied' | 'accessKeyCopied' | 'cliCommandCopied'

interface CopyItem {
  id: CopyFieldId
  label: string
  value: string
  copyLabel: string
  copiedMessage: CopyFeedbackKey
}

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
  const [copied, setCopied] = useState<CopyFieldId | null>(null)
  const { state } = creation
  if (state.phase === 'ready') {
    const { credential } = state
    const items: CopyItem[] = [
      {
        id: 'url',
        label: t.relayAddress,
        value: credential.url,
        copyLabel: t.copyRelayAddress,
        copiedMessage: 'relayAddressCopied',
      },
      {
        id: 'key',
        label: t.accessKey,
        value: credential.access_key,
        copyLabel: t.copyAccessKey,
        copiedMessage: 'accessKeyCopied',
      },
      {
        id: 'cli',
        label: t.cliCommand,
        value: formatRelayAddCommand(credential),
        copyLabel: t.copyCLICommand,
        copiedMessage: 'cliCommandCopied',
      },
    ]
    const copyValue = (item: CopyItem) => void copy.run(async () => {
      try {
        await navigator.clipboard.writeText(item.value)
      } catch {
        setCopied(null)
        throw new MessageError(message(relayCopy, 'copyFailed'))
      }
      setCopied(item.id)
      return message(relayCopy, item.copiedMessage)
    })
    return <FocusPage icon="check" label={t.access} title={t.keyCreated}>
      <p className="lead">{t.saveKeyBeforeFinishing}</p>
      <div className="copy-fields">
        {items.map(item => {
          const active = copied === item.id
          return <div className="copy-field" key={item.id}>
            <div className="copy-field-head">
              <span className="copy-field-label">{item.label}</span>
              <button type="button" className="secondary copy-button" aria-label={item.copyLabel}
                disabled={copy.busy} onClick={() => copyValue(item)}>
                <Icon name={active ? 'check' : 'copy'} size={15} />
                <span>{active ? t.copied : t.copy}</span>
              </button>
            </div>
            <code className="copy-field-value">{item.value}</code>
          </div>
        })}
      </div>
      <Notice feedback={copy.feedback} />
      <button type="button" className="secondary" onClick={() => {
        setCopied(null)
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
