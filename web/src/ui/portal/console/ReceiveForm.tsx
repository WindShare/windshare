import { useEffect, useRef, useState, type FormEvent } from 'react'
import type { ReceiverController } from '../../controller'
import { PortalIcon } from '../PortalIcon'
import { usePortalCopy, type PortalMessageKey } from '../locales/use-portal-copy'

type ClipboardStatus = 'idle' | 'reading' | 'pasted' | 'empty' | 'unavailable'

const CLIPBOARD_MESSAGES: Record<ClipboardStatus, PortalMessageKey> = {
  idle: 'clipboardIdle', reading: 'clipboardReading', pasted: 'clipboardPasted',
  empty: 'clipboardEmpty', unavailable: 'clipboardUnavailable',
}

export function ReceiveForm({ controller }: {
  readonly controller: Pick<ReceiverController, 'submitKey'>
}) {
  const t = usePortalCopy()
  const [keyInput, setKeyInput] = useState('')
  const [clipboardStatus, setClipboardStatus] = useState<ClipboardStatus>('idle')
  const input = useRef<HTMLInputElement>(null)
  const pasteRequest = useRef(0)

  useEffect(() => () => { pasteRequest.current++ }, [])

  const handlePaste = async () => {
    const request = ++pasteRequest.current
    setClipboardStatus('reading')
    try {
      const text = (await navigator.clipboard.readText()).trim()
      // Typing or submitting supersedes a pending clipboard permission request.
      if (request !== pasteRequest.current) return
      if (text) setKeyInput(text)
      setClipboardStatus(text ? 'pasted' : 'empty')
    } catch {
      if (request !== pasteRequest.current) return
      setClipboardStatus('unavailable')
    }
    input.current?.focus()
  }

  const receive = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!keyInput.trim()) return
    const key = keyInput.trim()
    pasteRequest.current++
    setKeyInput('')
    setClipboardStatus('idle')
    controller.submitKey(key)
  }

  return (
    <form className="portal-key-form" onSubmit={receive}>
      <div className="portal-panel-heading">
        <h2>{t.receiveTitle}</h2>
        <p>{t.receiveDescription}</p>
      </div>
      <div className="portal-field">
        <label htmlFor="portal-share-key">{t.inputLabel}</label>
        <div className="portal-key-input-wrapper">
          <PortalIcon name="link" size={18} />
          <input ref={input} id="portal-share-key" type="text" className="portal-key-input"
            placeholder={t.inputPlaceholder} value={keyInput}
            onChange={event => {
              pasteRequest.current++
              setKeyInput(event.target.value)
              setClipboardStatus('idle')
            }}
            aria-describedby="portal-clipboard-feedback" autoComplete="off"
            autoCapitalize="off" autoCorrect="off" spellCheck={false} required />
          <button type="button" className="portal-btn-paste" onClick={() => { void handlePaste() }}
            disabled={clipboardStatus === 'reading'} aria-label={t.pasteLabel}>
            <PortalIcon name={clipboardStatus === 'pasted' ? 'check' : 'clipboard'} size={16} />
            <span>{clipboardStatus === 'reading' ? t.reading : t.paste}</span>
          </button>
        </div>
        <p id="portal-clipboard-feedback" className="portal-field-feedback" role="status"
          data-state={clipboardStatus}>{t[CLIPBOARD_MESSAGES[clipboardStatus]]}</p>
      </div>
      <button type="submit" className="portal-btn-primary" disabled={!keyInput.trim()}>
        {t.receive}<PortalIcon name="arrow" size={18} />
      </button>
      <div className="portal-privacy-note"><PortalIcon name="shield" size={15} />
        <span>{t.privacy}</span>
      </div>
    </form>
  )
}
