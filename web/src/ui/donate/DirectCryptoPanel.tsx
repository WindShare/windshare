import { useEffect, useRef, useState, type KeyboardEvent } from 'react'
import { PortalIcon } from '../portal/PortalIcon'
import { QrCodeSvg } from './QrCodeSvg'
import {
  DIRECT_CRYPTO_WALLETS,
  OFFICIAL_DONATE_PAGE_URL,
  type CryptoWalletId,
  type DirectCryptoWallet,
} from './config'
import { useDonateCopy } from './locales/use-donate-copy'

type CopyFeedbackStatus = 'copied' | 'failed'

interface CopyFeedback {
  readonly walletId: CryptoWalletId
  readonly status: CopyFeedbackStatus
}

interface CoinArtwork {
  readonly background: string
  readonly path: string
}

interface DirectCryptoPanelProps {
  readonly selectedCoin: CryptoWalletId
  readonly onSelectCoin: (coin: CryptoWalletId) => void
  readonly showSelfHostFooter: boolean
}

const COPY_FEEDBACK_MS = 2000
const COIN_GRID_COLUMNS = 3

// CC0 1.0 coin glyph geometry shared with desktop/crates/windshare-desktop/assets/payments/icons/.
const COIN_ARTWORK: Readonly<Record<CryptoWalletId, CoinArtwork>> = {
  btc: {
    background: '#F7931A',
    path: 'M23.638 14.904c-1.602 6.43-8.113 10.34-14.542 8.736C2.67 22.05-1.244 15.525.362 9.105 1.962 2.67 8.475-1.243 14.9.358c6.43 1.605 10.342 8.115 8.738 14.548v-.002zm-6.35-4.613c.24-1.59-.974-2.45-2.64-3.03l.54-2.153-1.315-.33-.525 2.107c-.345-.087-.705-.167-1.064-.25l.526-2.127-1.32-.33-.54 2.165c-.285-.067-.565-.132-.84-.2l-1.815-.45-.35 1.407s.975.225.955.236c.535.136.63.486.615.766l-1.477 5.92c-.075.166-.24.406-.614.314.015.02-.96-.24-.96-.24l-.66 1.51 1.71.426.93.242-.54 2.19 1.32.327.54-2.17c.36.1.705.19 1.05.273l-.51 2.154 1.32.33.545-2.19c2.24.427 3.93.257 4.64-1.774.57-1.637-.03-2.58-1.217-3.196.854-.193 1.5-.76 1.68-1.93h.01zm-3.01 4.22c-.404 1.64-3.157.75-4.05.53l.72-2.9c.896.23 3.757.67 3.33 2.37zm.41-4.24c-.37 1.49-2.662.735-3.405.55l.654-2.64c.744.18 3.137.524 2.75 2.084v.006z',
  },
  eth: {
    background: '#7C63CF',
    path: 'M11.944 17.97L4.58 13.62 11.943 24l7.37-10.38-7.372 4.35h.003zM12.056 0L4.69 12.223l7.365 4.354 7.365-4.35L12.056 0z',
  },
  sol: {
    background: '#6845BA',
    path: 'm23.8764 18.0313-3.962 4.1393a.9201.9201 0 0 1-.306.2106.9407.9407 0 0 1-.367.0742H.4599a.4689.4689 0 0 1-.2522-.0733.4513.4513 0 0 1-.1696-.1962.4375.4375 0 0 1-.0314-.2545.4438.4438 0 0 1 .117-.2298l3.9649-4.1393a.92.92 0 0 1 .3052-.2102.9407.9407 0 0 1 .3658-.0746H23.54a.4692.4692 0 0 1 .2523.0734.4531.4531 0 0 1 .1697.196.438.438 0 0 1 .0313.2547.4442.4442 0 0 1-.1169.2297zm-3.962-8.3355a.9202.9202 0 0 0-.306-.2106.941.941 0 0 0-.367-.0742H.4599a.4687.4687 0 0 0-.2522.0734.4513.4513 0 0 0-.1696.1961.4376.4376 0 0 0-.0314.2546.444.444 0 0 0 .117.2297l3.9649 4.1394a.9204.9204 0 0 0 .3052.2102c.1154.049.24.0744.3658.0746H23.54a.469.469 0 0 0 .2523-.0734.453.453 0 0 0 .1697-.1961.4382.4382 0 0 0 .0313-.2546.4444.4444 0 0 0-.1169-.2297zM.46 6.7225h18.7815a.9411.9411 0 0 0 .367-.0742.9202.9202 0 0 0 .306-.2106l3.962-4.1394a.4442.4442 0 0 0 .117-.2297.4378.4378 0 0 0-.0314-.2546.453.453 0 0 0-.1697-.196.469.469 0 0 0-.2523-.0734H4.7596a.941.941 0 0 0-.3658.0745.9203.9203 0 0 0-.3052.2102L.1246 5.9687a.4438.4438 0 0 0-.1169.2295.4375.4375 0 0 0 .0312.2544.4512.4512 0 0 0 .1692.196.4689.4689 0 0 0 .2518.0739z',
  },
  sui: {
    background: '#4DA2FF',
    path: 'M17.636 10.009a7.16 7.16 0 0 1 1.565 4.474 7.2 7.2 0 0 1-1.608 4.53l-.087.106-.023-.135a7 7 0 0 0-.07-.349c-.502-2.21-2.142-4.106-4.84-5.642-1.823-1.034-2.866-2.278-3.14-3.693-.177-.915-.046-1.834.209-2.62.254-.787.631-1.446.953-1.843l1.05-1.284a.46.46 0 0 1 .713 0l5.28 6.456zm1.66-1.283L12.26.123a.336.336 0 0 0-.52 0L4.704 8.726l-.023.029a9.33 9.33 0 0 0-2.07 5.872C2.612 19.803 6.816 24 12 24s9.388-4.197 9.388-9.373a9.32 9.32 0 0 0-2.07-5.871zM6.389 9.981l.63-.77.018.142q.023.17.055.34c.408 2.136 1.862 3.917 4.294 5.297 2.114 1.203 3.345 2.586 3.7 4.103a5.3 5.3 0 0 1 .109 1.801l-.004.034-.03.014A7.2 7.2 0 0 1 12 21.67c-3.976 0-7.2-3.218-7.2-7.188 0-1.705.594-3.27 1.587-4.503z',
  },
  bch: {
    background: '#37A776',
    path: 'm10.84 11.22-.688-2.568c.728-.18 2.839-1.051 3.39.506.27 1.682-1.978 1.877-2.702 2.062zm.289 1.313.755 2.829c.868-.228 3.496-.46 3.241-2.351-.433-1.666-3.125-.706-3.996-.478zM24 12c0 6.627-5.373 12-12 12S0 18.627 0 12 5.373 0 12 0s12 5.373 12 12zm-6.341.661c-.183-1.151-1.441-2.095-2.485-2.202.643-.57.969-1.401.57-2.488-.603-1.368-1.989-1.66-3.685-1.377l-.546-2.114-1.285.332.536 2.108c-.338.085-.685.158-1.029.256L9.198 5.08l-1.285.332.545 2.114c-.277.079-2.595.673-2.595.673l.353 1.377s.944-.265.935-.244c.524-.137.771.125.886.372l1.498 5.793c.018.168-.012.454-.372.551.021.012-.935.241-.935.241l.14 1.605s2.296-.588 2.598-.664l.551 2.138 1.285-.332-.551-2.153c.353-.082.697-.168 1.032-.256l.548 2.141 1.285-.332-.551-2.135c1.982-.482 3.38-1.73 3.094-3.64z',
  },
  usdt: {
    background: '#269D7B',
    path: 'M18.7538 10.5176c0 .6251-2.2379 1.1483-5.2381 1.2812l.0028.0007c-.0848.0064-.5233.0325-1.5012.0325-.7778 0-1.33-.0233-1.5237-.0325-3.0059-.1322-5.2495-.6555-5.2495-1.2819s2.2436-1.149 5.2495-1.2834v2.0442c.1965.0142.7594.0474 1.5372.0474.9334 0 1.4008-.0389 1.4849-.0466V9.2356c2.9994.1337 5.2381.657 5.2381 1.282zm5.19.5466L12.1248 22.389a.1803.1803 0 0 1-.2496 0L.0562 11.0635a.1781.1781 0 0 1-.0382-.2079l4.3762-9.1921a.1767.1767 0 0 1 .1626-.1026h14.8878a.1768.1768 0 0 1 .1612.1032l4.3762 9.1922a.1782.1782 0 0 1-.0382.2079zm-4.478-.4038c0-.8068-2.5515-1.4799-5.9473-1.6369V7.195h4.186V4.4055H6.3076V7.195h4.1852v1.8286c-3.4018.1562-5.9601.83-5.9601 1.6376 0 .8075 2.5583 1.4806 5.9601 1.6376v5.8618h3.025v-5.8639c3.394-.1563 5.948-.8295 5.948-1.6363z',
  },
}

function CoinIcon({ id, size }: { readonly id: CryptoWalletId; readonly size: number }) {
  const artwork = COIN_ARTWORK[id]
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="9" fill={artwork.background} />
      <g transform="translate(6 6) scale(.833333)" fill="#FFFFFF">
        <path d={artwork.path} />
      </g>
    </svg>
  )
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

function resolveNextCoinIndex(key: string, index: number, total: number): number | null {
  switch (key) {
    case 'ArrowRight':
      return (index + 1) % total
    case 'ArrowLeft':
      return (index - 1 + total) % total
    case 'ArrowDown':
      return (index + COIN_GRID_COLUMNS) % total
    case 'ArrowUp':
      return (index - COIN_GRID_COLUMNS + total) % total
    case 'Home':
      return 0
    case 'End':
      return total - 1
    default:
      return null
  }
}

function resolveCopyLabel(
  status: CopyFeedbackStatus | null,
  ticker: string,
  t: ReturnType<typeof useDonateCopy>,
): string {
  if (status === 'copied') return t.copiedCoinAddress(ticker)
  if (status === 'failed') return t.copyAddressFailed
  return t.copyCoinAddress(ticker)
}

function findWallet(selectedCoin: CryptoWalletId): DirectCryptoWallet {
  const matched = DIRECT_CRYPTO_WALLETS.find(wallet => wallet.id === selectedCoin)
  if (matched !== undefined) return matched
  const fallback = DIRECT_CRYPTO_WALLETS[0]
  if (fallback === undefined) {
    throw new Error('DIRECT_CRYPTO_WALLETS must contain at least one wallet')
  }
  return fallback
}

export function DirectCryptoPanel({
  selectedCoin,
  onSelectCoin,
  showSelfHostFooter,
}: DirectCryptoPanelProps) {
  const t = useDonateCopy()
  const [feedback, setFeedback] = useState<CopyFeedback | null>(null)
  const coinRefs = useRef<Array<HTMLButtonElement | null>>([])
  const addressRef = useRef<HTMLElement | null>(null)
  const resetTimerRef = useRef<number | null>(null)
  const copyAttemptRef = useRef(0)

  const activeWallet = findWallet(selectedCoin)
  const copyStatus = feedback?.walletId === activeWallet.id ? feedback.status : null
  const copyLabel = resolveCopyLabel(copyStatus, activeWallet.ticker, t)

  const clearPendingCopy = () => {
    copyAttemptRef.current += 1
    if (resetTimerRef.current !== null) {
      window.clearTimeout(resetTimerRef.current)
      resetTimerRef.current = null
    }
  }

  useEffect(() => () => {
    clearPendingCopy()
  }, [])

  const selectWallet = (id: CryptoWalletId) => {
    if (id === selectedCoin) return
    clearPendingCopy()
    setFeedback(null)
    onSelectCoin(id)
  }

  const handleCoinKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    const nextIndex = resolveNextCoinIndex(event.key, index, DIRECT_CRYPTO_WALLETS.length)
    if (nextIndex === null) return
    event.preventDefault()
    const target = DIRECT_CRYPTO_WALLETS[nextIndex]
    if (target !== undefined) {
      selectWallet(target.id)
      coinRefs.current[nextIndex]?.focus()
    }
  }

  const handleCopyAddress = async () => {
    clearPendingCopy()
    const attempt = copyAttemptRef.current
    const walletId = activeWallet.id
    try {
      await navigator.clipboard.writeText(activeWallet.address)
      if (attempt !== copyAttemptRef.current) return
      setFeedback({ walletId, status: 'copied' })
      resetTimerRef.current = window.setTimeout(() => {
        resetTimerRef.current = null
        setFeedback(null)
      }, COPY_FEEDBACK_MS)
    } catch {
      if (attempt !== copyAttemptRef.current) return
      setFeedback({ walletId, status: 'failed' })
      // Non-HTTPS self-hosted origins lack navigator.clipboard; select the address
      // so the donor can copy it directly with Ctrl+C or long-press.
      selectElementText(addressRef.current)
    }
  }

  return (
    <div className="donate-direct-panel">
      <div
        className="donate-coin-grid"
        role="radiogroup"
        aria-label={t.cryptoCoinSelectorLabel}
      >
        {DIRECT_CRYPTO_WALLETS.map((wallet, index) => {
          const isSelected = wallet.id === activeWallet.id
          return (
            <button
              key={wallet.id}
              ref={node => { coinRefs.current[index] = node }}
              type="button"
              role="radio"
              aria-checked={isSelected}
              tabIndex={isSelected ? 0 : -1}
              className="donate-coin-btn"
              onClick={() => selectWallet(wallet.id)}
              onKeyDown={event => handleCoinKeyDown(event, index)}
            >
              <CoinIcon id={wallet.id} size={20} />
              <span className="donate-coin-ticker">{wallet.ticker}</span>
            </button>
          )
        })}
      </div>

      <div className="donate-wallet-card">
        <div className="donate-wallet-head">
          <CoinIcon id={activeWallet.id} size={24} />
          <strong className="donate-wallet-ticker">{activeWallet.ticker}</strong>
          <span className="donate-wallet-network-pill">{activeWallet.network}</span>
        </div>

        <div className="donate-wallet-qr-col">
          <div className="donate-wallet-qr-frame">
            <QrCodeSvg
              value={activeWallet.address}
              label={t.cryptoDirectQrLabel(activeWallet.ticker, activeWallet.network)}
            />
          </div>
          <span className="donate-wallet-qr-hint">{t.cryptoDirectScanHint}</span>
        </div>

        <div className="donate-wallet-address-box">
          <code ref={addressRef} className="donate-wallet-address">
            {activeWallet.address}
          </code>
        </div>

        <button
          type="button"
          className="donate-wallet-copy-btn"
          data-status={copyStatus ?? undefined}
          onClick={() => void handleCopyAddress()}
        >
          <PortalIcon name={copyStatus === 'copied' ? 'check' : 'clipboard'} size={15} />
          <span>{copyLabel}</span>
        </button>
      </div>

      {copyStatus === 'failed' && (
        <p className="donate-error" role="alert">{t.copyAddressFailedHint}</p>
      )}

      {showSelfHostFooter && (
        <div className="donate-selfhost-footer">
          <p className="donate-note">{t.selfHostFooterNote}</p>
          <a
            href={OFFICIAL_DONATE_PAGE_URL}
            target="_blank"
            rel="noopener noreferrer"
            className="portal-text-link donate-selfhost-link"
          >
            <span>{t.selfHostOfficialCta}</span>
            <PortalIcon name="arrow" size={14} />
          </a>
        </div>
      )}
    </div>
  )
}
