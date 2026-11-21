import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { PortalIcon } from '../portal/PortalIcon'
import { usePreferences } from '../preferences/use-preferences'
import { AfdianQrCode } from './AfdianQrCode'
import { classifyCryptoIntentError, createCryptoIntent } from './api'
import {
  AFDIAN_ORDER_URL,
  KOFI_PAGE_URL,
  OFFICIAL_DONATE_PAGE_URL,
  SELF_HOST_FALLBACK_WALLETS,
  canCreateCryptoIntent,
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
type CopyFeedbackStatus = 'copied' | 'failed'

interface WalletCopyFeedback {
  readonly ticker: string
  readonly status: CopyFeedbackStatus
}

const CHANNELS: readonly DonateChannel[] = ['afdian', 'kofi', 'crypto']
const COPY_FEEDBACK_MS = 2000

interface DonateActionsProps {
  readonly submittedFromCheckout: boolean
}

function resolveCryptoApiErrorMessage(
  error: unknown,
  t: ReturnType<typeof useDonateCopy>,
): string {
  switch (classifyCryptoIntentError(error)) {
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

function selectElementText(node: HTMLElement | null): void {
  if (node === null || typeof window === 'undefined') return
  const selection = window.getSelection()
  if (selection === null) return
  const range = document.createRange()
  range.selectNodeContents(node)
  selection.removeAllRanges()
  selection.addRange(range)
}

function resolveWalletCopyLabel(
  status: CopyFeedbackStatus | null,
  t: ReturnType<typeof useDonateCopy>,
): string {
  if (status === 'copied') return t.copiedAddress
  if (status === 'failed') return t.copyAddressFailed
  return t.copyAddress
}

function SelfHostCryptoFallback() {
  const t = useDonateCopy()
  const [feedback, setFeedback] = useState<WalletCopyFeedback | null>(null)
  const addressRefs = useRef<Map<string, HTMLElement>>(new Map())
  const resetTimerRef = useRef<number | null>(null)
  const copyAttemptRef = useRef(0)

  useEffect(() => () => {
    // Invalidate any in-flight clipboard write and cancel pending reset on unmount.
    copyAttemptRef.current += 1
    if (resetTimerRef.current !== null) {
      window.clearTimeout(resetTimerRef.current)
      resetTimerRef.current = null
    }
  }, [])

  const copyAddress = async (ticker: string, address: string) => {
    copyAttemptRef.current += 1
    const attempt = copyAttemptRef.current
    if (resetTimerRef.current !== null) {
      window.clearTimeout(resetTimerRef.current)
      resetTimerRef.current = null
    }

    try {
      await navigator.clipboard.writeText(address)
      if (attempt !== copyAttemptRef.current) return
      setFeedback({ ticker, status: 'copied' })
      resetTimerRef.current = window.setTimeout(() => {
        resetTimerRef.current = null
        setFeedback(null)
      }, COPY_FEEDBACK_MS)
    } catch {
      if (attempt !== copyAttemptRef.current) return
      setFeedback({ ticker, status: 'failed' })
      // Non-HTTPS self-hosted origins lack navigator.clipboard; select the address
      // so the user can copy it directly with Ctrl+C or long-press.
      selectElementText(addressRefs.current.get(ticker) ?? null)
    }
  }

  return (
    <div className="donate-panel-stack">
      <div className="donate-selfhost-head">
        <p className="donate-note">{t.selfHostCryptoNote}</p>
        <a
          href={OFFICIAL_DONATE_PAGE_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="portal-button donate-secondary-btn"
        >
          <span>{t.selfHostOfficialCta}</span>
          <PortalIcon name="arrow" size={16} />
        </a>
      </div>
      <ul className="donate-wallet-list" aria-label={t.selfHostCryptoTitle}>
        {SELF_HOST_FALLBACK_WALLETS.map(wallet => {
          const status = feedback?.ticker === wallet.ticker ? feedback.status : null
          const label = resolveWalletCopyLabel(status, t)
          return (
            <li key={wallet.ticker} className="donate-wallet-row">
              <div className="donate-wallet-meta">
                <span className="donate-wallet-ticker">{wallet.ticker}</span>
                <span className="donate-wallet-network">{wallet.network}</span>
              </div>
              <div className="donate-wallet-address-line">
                <code
                  ref={node => {
                    if (node === null) addressRefs.current.delete(wallet.ticker)
                    else addressRefs.current.set(wallet.ticker, node)
                  }}
                  className="donate-wallet-address"
                >
                  {wallet.address}
                </code>
                <button
                  type="button"
                  className="donate-copy-btn"
                  data-status={status ?? undefined}
                  aria-label={`${label} ${wallet.ticker}`}
                  onClick={() => void copyAddress(wallet.ticker, wallet.address)}
                >
                  {label}
                </button>
              </div>
            </li>
          )
        })}
      </ul>
      {feedback?.status === 'failed' && (
        <p className="donate-error" role="alert">{t.copyAddressFailedHint}</p>
      )}
    </div>
  )
}

function CryptoCheckoutForm() {
  const t = useDonateCopy()
  const [preset, setPreset] = useState<string>(DEFAULT_CRYPTO_AMOUNT)
  const [customMode, setCustomMode] = useState(false)
  const [customAmount, setCustomAmount] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [message, setMessage] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [errorText, setErrorText] = useState<string | null>(null)
  // Single synchronous owner for an in-flight intent request or pending redirect
  // so re-entry, tab unmount, and bfcache restore share one lifecycle handle.
  const activeSubmissionRef = useRef<AbortController | null>(null)

  // Soft-limit validation instead of onChange interception or UTF-16 maxLength:
  // avoids dropping oversized pastes and interrupting CJK IME composition near the cap.
  const displayNameCount = countCodePoints(displayName)
  const displayNameOverLimit = displayNameCount > MAX_DISPLAY_NAME_CODEPOINTS
  const messageCount = countCodePoints(message)
  const messageOverLimit = messageCount > MAX_MESSAGE_CODEPOINTS

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
      return
    }

    const controller = new AbortController()
    activeSubmissionRef.current = controller
    setErrorText(null)
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
      setErrorText(resolveCryptoApiErrorMessage(error, t))
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
                setErrorText(null)
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
              setErrorText(null)
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
              setErrorText(null)
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
            setErrorText(null)
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
            setErrorText(null)
          }}
        />
      </label>

      {errorText !== null && (
        <p className="donate-error" role="alert">{errorText}</p>
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
                <AfdianQrCode label={t.afdianQrLabel} />
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
            <p className="donate-region-note">{t.kofiRegionNote}</p>
          </div>
        )}

        {activeChannel === 'crypto' && (
          <div
            id="donate-panel-crypto"
            role="tabpanel"
            aria-labelledby="donate-tab-crypto"
            className="donate-panel-stack"
          >
            {hostedCryptoAllowed ? <CryptoCheckoutForm /> : <SelfHostCryptoFallback />}
          </div>
        )}
      </div>
    </section>
  )
}
