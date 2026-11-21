import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { PortalIcon } from '../portal/PortalIcon'
import { usePreferences } from '../preferences/use-preferences'
import { AfdianQrCode } from './AfdianQrCode'
import { DonateApiError, createCryptoIntent } from './api'
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
  parseCryptoAmountInput,
} from './format'
import { useDonateCopy } from './locales/use-donate-copy'

type DonateChannel = 'afdian' | 'kofi' | 'crypto'

const CHANNELS: readonly DonateChannel[] = ['afdian', 'kofi', 'crypto']

interface DonateActionsProps {
  readonly submittedFromCheckout: boolean
  readonly moderationEnabled: boolean
}

function resolveCryptoApiErrorMessage(
  error: unknown,
  t: ReturnType<typeof useDonateCopy>,
): string {
  if (error instanceof DonateApiError) {
    if (error.code === 'crypto_checkout_paused') return t.cryptoErrPaused
    if (error.status === 429 || error.code === 'rate_limited') return t.cryptoErrRateLimited
    if (error.code === 'invalid_amount') return t.cryptoErrInvalid
  }
  return t.cryptoErrFailed
}

function SelfHostCryptoFallback() {
  const t = useDonateCopy()
  const [copiedTicker, setCopiedTicker] = useState<string | null>(null)

  const copyAddress = async (ticker: string, address: string) => {
    try {
      await navigator.clipboard.writeText(address)
      setCopiedTicker(ticker)
      window.setTimeout(() => {
        setCopiedTicker(current => (current === ticker ? null : current))
      }, 2000)
    } catch {
      // Clipboard may be unavailable on non-HTTPS self-hosted origins.
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
        {SELF_HOST_FALLBACK_WALLETS.map(wallet => (
          <li key={wallet.ticker} className="donate-wallet-row">
            <div className="donate-wallet-meta">
              <span className="donate-wallet-ticker">{wallet.ticker}</span>
              <span className="donate-wallet-network">{wallet.network}</span>
            </div>
            <div className="donate-wallet-address-line">
              <code className="donate-wallet-address">{wallet.address}</code>
              <button
                type="button"
                className="donate-copy-btn"
                onClick={() => void copyAddress(wallet.ticker, wallet.address)}
              >
                {copiedTicker === wallet.ticker ? t.copiedAddress : t.copyAddress}
              </button>
            </div>
          </li>
        ))}
      </ul>
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

  const messageCount = countCodePoints(message)

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

    const rawAmount = customMode ? customAmount : preset
    const validated = parseCryptoAmountInput(rawAmount)
    if (!validated.ok) {
      if (validated.reason === 'below_min') setErrorText(t.cryptoErrBelowMin)
      else if (validated.reason === 'above_max') setErrorText(t.cryptoErrAboveMax)
      else setErrorText(t.cryptoErrInvalid)
      return
    }

    const controller = new AbortController()
    activeSubmissionRef.current = controller
    setErrorText(null)
    setSubmitting(true)
    try {
      const trimmedName = displayName.trim()
      const trimmedMessage = message.trim()
      const response = await createCryptoIntent({
        displayName: trimmedName.length > 0 ? trimmedName : null,
        message: trimmedMessage.length > 0 ? trimmedMessage : null,
        amountUsd: validated.normalized,
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
        <span className="donate-field-label">{t.cryptoNameLabel}</span>
        <input
          type="text"
          className="donate-input"
          value={displayName}
          maxLength={MAX_DISPLAY_NAME_CODEPOINTS * 2}
          placeholder={t.cryptoNamePlaceholder}
          onChange={event => {
            const next = event.target.value
            if (countCodePoints(next) <= MAX_DISPLAY_NAME_CODEPOINTS) {
              setDisplayName(next)
            }
          }}
        />
      </label>

      <label className="donate-field">
        <span className="donate-field-head">
          <span className="donate-field-label">{t.cryptoMessageLabel}</span>
          <span className="donate-counter" aria-hidden="true">
            {messageCount}/{MAX_MESSAGE_CODEPOINTS}
          </span>
        </span>
        <textarea
          className="donate-textarea"
          rows={3}
          value={message}
          placeholder={t.cryptoMessagePlaceholder}
          onChange={event => {
            const next = event.target.value
            if (countCodePoints(next) <= MAX_MESSAGE_CODEPOINTS) {
              setMessage(next)
            }
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
  moderationEnabled,
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
          <p>
            {moderationEnabled ? t.submittedBannerModeration : t.submittedBannerPendingChain}
          </p>
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
