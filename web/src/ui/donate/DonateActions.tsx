import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { PortalIcon } from '../portal/PortalIcon'
import { usePreferences } from '../preferences/use-preferences'
import { DirectCryptoPanel } from './DirectCryptoPanel'
import { QrCodeSvg } from './QrCodeSvg'
import {
  classifyCryptoIntentError,
  createCryptoIntent,
  type CryptoIntentErrorKind,
} from './api'
import {
  AFDIAN_ORDER_URL,
  DEFAULT_DIRECT_WALLET_ID,
  KOFI_PAGE_URL,
  canCreateCryptoIntent,
  type CryptoWalletId,
} from './config'
import {
  CRYPTO_PRESET_AMOUNTS,
  DEFAULT_CRYPTO_AMOUNT,
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_MESSAGE_CODEPOINTS,
  countCodePoints,
  validateCryptoCheckoutInput,
  type CryptoCheckoutValidationReason,
} from './format'
import { useDonateCopy } from './locales/use-donate-copy'

type DonateChannel = 'afdian' | 'kofi' | 'crypto'
type CryptoSubMode = 'checkout' | 'direct'

const CHANNELS: readonly DonateChannel[] = ['afdian', 'kofi', 'crypto']

interface DonateActionsProps {
  readonly submittedFromCheckout: boolean
}

function resolveCryptoApiErrorMessage(
  kind: CryptoIntentErrorKind,
  t: ReturnType<typeof useDonateCopy>,
): string {
  switch (kind) {
    case 'unavailable':
      return t.cryptoErrUnavailable
    case 'rate_limited':
      return t.cryptoErrRateLimited
    case 'invalid_amount':
      return t.cryptoErrInvalid
    case 'failed':
      return t.cryptoErrFailed
  }
}

function resolveCryptoValidationError(
  reason: CryptoCheckoutValidationReason,
  t: ReturnType<typeof useDonateCopy>,
): string {
  switch (reason) {
    case 'amount_below_min':
      return t.cryptoErrBelowMin
    case 'amount_above_max':
      return t.cryptoErrAboveMax
    case 'invalid_amount':
      return t.cryptoErrInvalid
    case 'display_name_too_long':
      return t.cryptoErrNameTooLong
    case 'message_too_long':
      return t.cryptoErrMessageTooLong
  }
}

interface CryptoCheckoutFormProps {
  readonly onSwitchToDirect?: () => void
}

function CryptoCheckoutForm({ onSwitchToDirect }: CryptoCheckoutFormProps) {
  const t = useDonateCopy()
  const [preset, setPreset] = useState<string>(DEFAULT_CRYPTO_AMOUNT)
  const [customMode, setCustomMode] = useState(false)
  const [customAmount, setCustomAmount] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [message, setMessage] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [errorText, setErrorText] = useState<string | null>(null)
  const [errorKind, setErrorKind] = useState<CryptoIntentErrorKind | null>(null)
  // Single synchronous owner for an in-flight intent request or pending redirect
  // so re-entry, tab unmount, and bfcache restore share one lifecycle handle.
  const activeSubmissionRef = useRef<AbortController | null>(null)

  // Soft-limit validation instead of onChange interception or UTF-16 maxLength:
  // avoids dropping oversized pastes and interrupting CJK IME composition near the cap.
  const displayNameCount = countCodePoints(displayName)
  const displayNameOverLimit = displayNameCount > MAX_DISPLAY_NAME_CODEPOINTS
  const messageCount = countCodePoints(message)
  const messageOverLimit = messageCount > MAX_MESSAGE_CODEPOINTS

  const clearError = () => {
    setErrorText(null)
    setErrorKind(null)
  }

  useEffect(() => {
    const handlePageShow = (event: PageTransitionEvent) => {
      if (!event.persisted) return
      activeSubmissionRef.current?.abort()
      activeSubmissionRef.current = null
      setSubmitting(false)
    }
    window.addEventListener('pageshow', handlePageShow)
    return () => {
      window.removeEventListener('pageshow', handlePageShow)
      activeSubmissionRef.current?.abort()
      activeSubmissionRef.current = null
    }
  }, [])

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (activeSubmissionRef.current !== null) return

    const validated = validateCryptoCheckoutInput({
      rawAmount: customMode ? customAmount : preset,
      displayName,
      message,
    })
    if (!validated.ok) {
      setErrorText(resolveCryptoValidationError(validated.reason, t))
      setErrorKind(null)
      return
    }

    const controller = new AbortController()
    activeSubmissionRef.current = controller
    clearError()
    setSubmitting(true)
    try {
      const response = await createCryptoIntent({
        displayName: validated.displayName,
        message: validated.message,
        amountUsd: validated.amountUsd,
      }, controller.signal)
      if (controller.signal.aborted) return
      window.location.assign(response.invoiceUrl)
    } catch (error) {
      if (controller.signal.aborted) return
      activeSubmissionRef.current = null
      const kind = classifyCryptoIntentError(error)
      setErrorKind(kind)
      setErrorText(resolveCryptoApiErrorMessage(kind, t))
      setSubmitting(false)
    }
  }

  return (
    <form className="donate-crypto-form" onSubmit={event => void handleSubmit(event)} noValidate>
      <div className="donate-field">
        <span className="donate-field-label">{t.cryptoAmountLabel}</span>
        <div className="donate-amount-grid" role="group" aria-label={t.cryptoAmountLabel}>
          {CRYPTO_PRESET_AMOUNTS.map(amount => (
            <button
              key={amount}
              type="button"
              className="donate-amount-chip"
              aria-pressed={!customMode && preset === amount}
              onClick={() => {
                setCustomMode(false)
                setPreset(amount)
                clearError()
              }}
            >
              ${amount}
            </button>
          ))}
          <button
            type="button"
            className="donate-amount-chip"
            aria-pressed={customMode}
            onClick={() => {
              setCustomMode(true)
              clearError()
            }}
          >
            {t.cryptoCustom}
          </button>
        </div>
        {customMode && (
          <input
            type="text"
            inputMode="decimal"
            className="donate-input"
            value={customAmount}
            placeholder={t.cryptoCustomPlaceholder}
            aria-label={t.cryptoCustom}
            onChange={event => {
              setCustomAmount(event.target.value)
              clearError()
            }}
          />
        )}
      </div>

      <label className="donate-field">
        <span className="donate-field-head">
          <span className="donate-field-label">{t.cryptoNameLabel}</span>
          {displayNameOverLimit && (
            <span className="donate-counter donate-counter-over" aria-hidden="true">
              {displayNameCount}/{MAX_DISPLAY_NAME_CODEPOINTS}
            </span>
          )}
        </span>
        <input
          type="text"
          className="donate-input"
          value={displayName}
          aria-invalid={displayNameOverLimit || undefined}
          placeholder={t.cryptoNamePlaceholder}
          onChange={event => {
            setDisplayName(event.target.value)
            clearError()
          }}
        />
      </label>

      <label className="donate-field">
        <span className="donate-field-head">
          <span className="donate-field-label">{t.cryptoMessageLabel}</span>
          <span
            className={messageOverLimit ? 'donate-counter donate-counter-over' : 'donate-counter'}
            aria-hidden="true"
          >
            {messageCount}/{MAX_MESSAGE_CODEPOINTS}
          </span>
        </span>
        <textarea
          className="donate-textarea"
          rows={3}
          value={message}
          aria-invalid={messageOverLimit || undefined}
          placeholder={t.cryptoMessagePlaceholder}
          onChange={event => {
            setMessage(event.target.value)
            clearError()
          }}
        />
      </label>

      {errorText !== null && (
        <div className="donate-error-block" role="alert">
          <p className="donate-error">{errorText}</p>
          {errorKind === 'unavailable' && onSwitchToDirect !== undefined && (
            <button
              type="button"
              className="donate-inline-action"
              onClick={onSwitchToDirect}
            >
              <span>{t.cryptoSwitchToDirect}</span>
              <PortalIcon name="arrow" size={14} />
            </button>
          )}
        </div>
      )}

      <button type="submit" className="portal-btn-primary" disabled={submitting}>
        <span>{submitting ? t.cryptoSubmitting : t.cryptoCta}</span>
        {!submitting && <PortalIcon name="arrow" size={18} />}
      </button>

      <p className="donate-note">{t.cryptoNotice}</p>
    </form>
  )
}

export function DonateActions({
  submittedFromCheckout,
}: DonateActionsProps) {
  const { language } = usePreferences()
  const t = useDonateCopy()
  const [selectedChannel, setSelectedChannel] = useState<DonateChannel | null>(null)
  const [cryptoMode, setCryptoMode] = useState<CryptoSubMode>('checkout')
  const [selectedCoin, setSelectedCoin] = useState<CryptoWalletId>(DEFAULT_DIRECT_WALLET_ID)
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([])

  const defaultChannel: DonateChannel = language === 'zh-CN' ? 'afdian' : 'kofi'
  const activeChannel = selectedChannel ?? defaultChannel
  const hostedCryptoAllowed = typeof window !== 'undefined'
    && canCreateCryptoIntent(window.location.origin, { allowLocalhost: import.meta.env.DEV })

  const handleTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    let nextIndex: number | null = null
    if (event.key === 'ArrowRight') nextIndex = (index + 1) % CHANNELS.length
    else if (event.key === 'ArrowLeft') nextIndex = (index - 1 + CHANNELS.length) % CHANNELS.length
    else if (event.key === 'Home') nextIndex = 0
    else if (event.key === 'End') nextIndex = CHANNELS.length - 1
    if (nextIndex === null) return

    event.preventDefault()
    const target = CHANNELS[nextIndex]
    if (target !== undefined) {
      setSelectedChannel(target)
      tabRefs.current[nextIndex]?.focus()
    }
  }

  const tabLabels: Readonly<Record<DonateChannel, string>> = {
    afdian: t.tabAfdian,
    kofi: t.tabKofi,
    crypto: t.tabCrypto,
  }

  return (
    <section className="donate-card donate-actions-card" aria-label={t.actionRegion}>
      <header className="donate-header">
        <h1 className="donate-title">{t.title}</h1>
        <p className="donate-subtitle">{t.subtitle}</p>
      </header>

      {submittedFromCheckout && (
        <div className="donate-submitted-banner" role="status">
          <div className="donate-submitted-title">
            <PortalIcon name="check" size={16} />
            <strong>{t.submittedBannerTitle}</strong>
          </div>
          <p>{t.submittedBannerBody}</p>
        </div>
      )}

      <div className="donate-tabs" role="tablist" aria-label={t.channelsLabel}>
        {CHANNELS.map((channel, index) => {
          const isSelected = activeChannel === channel
          return (
            <button
              key={channel}
              ref={node => { tabRefs.current[index] = node }}
              id={`donate-tab-${channel}`}
              type="button"
              role="tab"
              aria-selected={isSelected}
              aria-controls={`donate-panel-${channel}`}
              tabIndex={isSelected ? 0 : -1}
              className="donate-tab-btn"
              onClick={() => setSelectedChannel(channel)}
              onKeyDown={event => handleTabKeyDown(event, index)}
            >
              {tabLabels[channel]}
            </button>
          )
        })}
      </div>

      <div className="donate-panel-body">
        {activeChannel === 'afdian' && (
          <div
            id="donate-panel-afdian"
            role="tabpanel"
            aria-labelledby="donate-tab-afdian"
            className="donate-panel-stack"
          >
            <a
              href={AFDIAN_ORDER_URL}
              target="_blank"
              rel="noopener noreferrer"
              className="portal-btn-primary donate-cta-link"
            >
              <span>{t.afdianCta}</span>
              <PortalIcon name="arrow" size={18} />
            </a>
            <div className="donate-qr-box">
              <div className="donate-qr-frame">
                <QrCodeSvg value={AFDIAN_ORDER_URL} label={t.afdianQrLabel} />
              </div>
              <span className="donate-qr-caption">{t.afdianScanHint}</span>
            </div>
            <p className="donate-note">{t.afdianNotice}</p>
          </div>
        )}

        {activeChannel === 'kofi' && (
          <div
            id="donate-panel-kofi"
            role="tabpanel"
            aria-labelledby="donate-tab-kofi"
            className="donate-panel-stack"
          >
            <a
              href={KOFI_PAGE_URL}
              target="_blank"
              rel="noopener noreferrer"
              className="portal-btn-primary donate-cta-link"
            >
              <span>{t.kofiCta}</span>
              <PortalIcon name="arrow" size={18} />
            </a>
            <p className="donate-note">{t.kofiNotice}</p>
          </div>
        )}

        {activeChannel === 'crypto' && (
          <div
            id="donate-panel-crypto"
            role="tabpanel"
            aria-labelledby="donate-tab-crypto"
            className="donate-panel-stack"
          >
            {hostedCryptoAllowed ? (
              <>
                <div
                  className="donate-crypto-modes"
                  role="group"
                  aria-label={t.cryptoModeLabel}
                >
                  <button
                    type="button"
                    className="donate-crypto-mode-btn"
                    aria-pressed={cryptoMode === 'checkout'}
                    onClick={() => setCryptoMode('checkout')}
                  >
                    {t.cryptoModeCheckout}
                  </button>
                  <button
                    type="button"
                    className="donate-crypto-mode-btn"
                    aria-pressed={cryptoMode === 'direct'}
                    onClick={() => setCryptoMode('direct')}
                  >
                    {t.cryptoModeDirect}
                  </button>
                </div>
                {cryptoMode === 'checkout' ? (
                  <CryptoCheckoutForm onSwitchToDirect={() => setCryptoMode('direct')} />
                ) : (
                  <DirectCryptoPanel
                    selectedCoin={selectedCoin}
                    onSelectCoin={setSelectedCoin}
                    showSelfHostFooter={false}
                  />
                )}
              </>
            ) : (
              <DirectCryptoPanel
                selectedCoin={selectedCoin}
                onSelectCoin={setSelectedCoin}
                showSelfHostFooter
              />
            )}
          </div>
        )}
      </div>
    </section>
  )
}
