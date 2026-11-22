import { usePreferences } from '../preferences/use-preferences'
import {
  formatDonationAmount,
  formatIsoTimestamp,
  formatProviderLabel,
  formatRelativeTime,
} from './format'
import { useDonateCopy } from './locales/use-donate-copy'
import type { PublicWallEntry, TopSupporterRank } from './types'

export type WallLoadStatus = 'loading' | 'ready' | 'degraded'

interface SupporterWallProps {
  readonly status: WallLoadStatus
  readonly refreshing: boolean
  readonly entries: readonly PublicWallEntry[]
  readonly topRankById?: ReadonlyMap<string, TopSupporterRank>
  readonly nowSec: number
  readonly onRefresh: () => void
}

const SKELETON_ROWS = [0, 1, 2, 3] as const

export function SupporterWall({
  status,
  refreshing,
  entries,
  topRankById,
  nowSec,
  onRefresh,
}: SupporterWallProps) {
  const { language } = usePreferences()
  const t = useDonateCopy()

  return (
    <section
      className="donate-card donate-wall-card"
      aria-label={t.wallRegion}
      aria-busy={status === 'loading'}
    >
      <div className="donate-wall-header">
        <div className="donate-wall-title-group">
          <h2 className="donate-wall-heading">{t.wallHeading}</h2>
          {status === 'ready' && entries.length > 0 && (
            <span className="donate-wall-count">{entries.length}</span>
          )}
        </div>
        <button
          type="button"
          className="donate-refresh-btn"
          disabled={status === 'loading' || refreshing}
          onClick={onRefresh}
        >
          {refreshing ? t.wallRefreshing : t.wallRefresh}
        </button>
      </div>

      {status === 'loading' && (
        <div className="donate-skeleton-list" aria-hidden="true">
          {SKELETON_ROWS.map(key => (
            <div key={key} className="donate-skeleton-row">
              <div className="donate-skeleton-bar donate-skeleton-head" />
              <div className="donate-skeleton-bar donate-skeleton-line" />
            </div>
          ))}
        </div>
      )}

      {status === 'degraded' && (
        <div className="donate-wall-state">
          <p className="donate-wall-state-title">{t.wallDegradedTitle}</p>
          <p className="donate-wall-state-note">{t.wallDegradedNote}</p>
          <button
            type="button"
            className="portal-button donate-retry-btn"
            onClick={onRefresh}
            disabled={refreshing}
          >
            {t.wallRetry}
          </button>
        </div>
      )}

      {status === 'ready' && entries.length === 0 && (
        <div className="donate-wall-state">
          <p className="donate-wall-state-title">{t.wallEmptyTitle}</p>
          <p className="donate-wall-state-note">{t.wallEmptyNote}</p>
        </div>
      )}

      {status === 'ready' && entries.length > 0 && (
        <ul className="donate-wall-list">
          {entries.map(entry => {
            const isAnonymous = entry.displayName === null
            const name = entry.displayName ?? t.anonymous
            const amount = formatDonationAmount(
              entry.amountMinor,
              entry.currency,
              entry.minorDigits,
            )
            const relative = formatRelativeTime(entry.donatedAt, nowSec, language)
            const iso = formatIsoTimestamp(entry.donatedAt)
            const topRank = topRankById?.get(entry.id)

            return (
              <li
                key={entry.id}
                className={entry.message === null ? 'donate-entry donate-entry-compact' : 'donate-entry'}
              >
                <div className="donate-entry-head">
                  <div className="donate-entry-identity">
                    {topRank !== undefined && (
                      <span
                        className="donate-entry-rank"
                        data-rank={topRank}
                        aria-label={t.topRankLabel(topRank)}
                      >
                        #{topRank}
                      </span>
                    )}
                    <span className={isAnonymous ? 'donate-entry-name is-anonymous' : 'donate-entry-name'}>
                      {name}
                    </span>
                    <span className="donate-entry-amount">{amount}</span>
                  </div>
                  <div className="donate-entry-meta">
                    <span className="donate-entry-provider">{formatProviderLabel(entry.provider, t)}</span>
                    <time className="donate-entry-time" dateTime={iso} title={iso}>{relative}</time>
                  </div>
                </div>
                {entry.message !== null && (
                  <p className="donate-entry-message">{entry.message}</p>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}
