import { usePreferences } from '../preferences/use-preferences'
import {
  formatDonationAmount,
  formatIsoTimestamp,
  formatProviderLabel,
  formatRelativeTime,
} from './format'
import { useDonateCopy } from './locales/use-donate-copy'
import type { PublicWallEntry, TopSupporterRank } from './types'

const MAX_DISPLAYED_TOP_SUPPORTERS = 3

interface TopSupportersProps {
  readonly entries: readonly PublicWallEntry[]
  readonly nowSec: number
}

export function TopSupporters({ entries, nowSec }: TopSupportersProps) {
  const { language } = usePreferences()
  const t = useDonateCopy()
  const visible = entries.slice(0, MAX_DISPLAYED_TOP_SUPPORTERS)
  if (visible.length === 0) return null

  return (
    <section className="donate-card donate-top-card" aria-label={t.topRegion}>
      <div className="donate-top-header">
        <div className="donate-top-title-group">
          <span className="donate-top-emblem" aria-hidden="true">
            <svg
              width="14"
              height="14"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M12 2.75 14.55 8.6l6.2.55-4.7 4.05 1.4 6.05L12 16l-5.45 3.25 1.4-6.05-4.7-4.05 6.2-.55L12 2.75Z" />
            </svg>
          </span>
          <h2 className="donate-wall-heading">{t.topHeading}</h2>
        </div>
        <span className="donate-top-pill">{t.topBadge}</span>
      </div>

      <ol className="donate-top-list">
        {visible.map((entry, index) => {
          const rank = (index + 1) as TopSupporterRank
          const isAnonymous = entry.displayName === null
          const name = entry.displayName ?? t.anonymous
          const amount = formatDonationAmount(
            entry.amountMinor,
            entry.currency,
            entry.minorDigits,
          )
          const relative = formatRelativeTime(entry.donatedAt, nowSec, language)
          const iso = formatIsoTimestamp(entry.donatedAt)
          const itemClass = entry.message === null
            ? 'donate-top-item donate-top-item-compact'
            : 'donate-top-item'

          return (
            <li key={entry.id} className={itemClass} data-rank={rank}>
              <div className="donate-top-row">
                <span className="donate-top-rank" aria-label={t.topRankLabel(rank)}>
                  {rank}
                </span>
                <span className={isAnonymous ? 'donate-top-name is-anonymous' : 'donate-top-name'}>
                  {name}
                </span>
                <span className="donate-top-amount">{amount}</span>
              </div>
              {entry.message !== null && (
                <p className="donate-top-message">{entry.message}</p>
              )}
              <div className="donate-top-meta">
                <span className="donate-entry-provider">{formatProviderLabel(entry.provider, t)}</span>
                <span className="donate-top-dot" aria-hidden="true">·</span>
                <time className="donate-entry-time" dateTime={iso} title={iso}>{relative}</time>
              </div>
            </li>
          )
        })}
      </ol>
    </section>
  )
}
