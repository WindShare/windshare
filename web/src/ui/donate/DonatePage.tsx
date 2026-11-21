import { useCallback, useEffect, useRef, useState } from 'react'
import { PortalFooter } from '../portal/PortalFooter'
import { PortalHeader } from '../portal/PortalHeader'
import '../portal/portal.css'
import './donate.css'
import { fetchDonationWall } from './api'
import { DonateActions } from './DonateActions'
import { SupporterWall, type WallLoadStatus } from './SupporterWall'
import type { PublicWallEntry, PublicWallResponse } from './types'

const SILENT_REFRESH_THROTTLE_MS = 10_000

interface WallViewState {
  readonly status: WallLoadStatus
  readonly refreshing: boolean
  readonly moderationEnabled: boolean
  readonly entries: readonly PublicWallEntry[]
  readonly nowSec: number
}

function hasSubmittedStatusQuery(): boolean {
  if (typeof window === 'undefined') return false
  return new URLSearchParams(window.location.search).get('status') === 'submitted'
}

function toReadyWallState(response: PublicWallResponse): WallViewState {
  return {
    status: 'ready',
    refreshing: false,
    moderationEnabled: response.moderationEnabled,
    entries: response.entries,
    nowSec: Math.floor(Date.now() / 1000),
  }
}

function toDegradedWallState(prev: WallViewState): WallViewState {
  return {
    ...prev,
    // Keep previously loaded entries intact if a silent background refresh fails.
    status: prev.status === 'ready' ? 'ready' : 'degraded',
    refreshing: false,
  }
}

export default function DonatePage() {
  const [submittedFromCheckout] = useState(hasSubmittedStatusQuery)
  const [wall, setWall] = useState<WallViewState>(() => ({
    status: 'loading',
    refreshing: false,
    moderationEnabled: false,
    entries: [],
    nowSec: Math.floor(Date.now() / 1000),
  }))

  const lastFetchAtRef = useRef(0)
  const inFlightRef = useRef(false)
  const mountedRef = useRef(true)

  const handleManualRefresh = useCallback(() => {
    if (inFlightRef.current) return
    inFlightRef.current = true
    lastFetchAtRef.current = Date.now()
    setWall(prev => ({ ...prev, refreshing: prev.status !== 'loading' }))
    void fetchDonationWall({ cache: 'no-cache' })
      .then(response => {
        if (mountedRef.current) setWall(toReadyWallState(response))
      })
      .catch(() => {
        if (mountedRef.current) setWall(toDegradedWallState)
      })
      .finally(() => {
        inFlightRef.current = false
      })
  }, [])

  useEffect(() => {
    mountedRef.current = true

    const fetchInBackground = (bypassCache: boolean) => {
      if (inFlightRef.current) return
      inFlightRef.current = true
      lastFetchAtRef.current = Date.now()
      void fetchDonationWall(bypassCache ? { cache: 'no-cache' } : {})
        .then(response => {
          if (mountedRef.current) setWall(toReadyWallState(response))
        })
        .catch(() => {
          if (mountedRef.current) setWall(toDegradedWallState)
        })
        .finally(() => {
          inFlightRef.current = false
        })
    }

    fetchInBackground(submittedFromCheckout)

    const triggerThrottledRefresh = () => {
      if (document.visibilityState !== 'visible') return
      if (Date.now() - lastFetchAtRef.current < SILENT_REFRESH_THROTTLE_MS) return
      fetchInBackground(true)
    }

    document.addEventListener('visibilitychange', triggerThrottledRefresh)
    window.addEventListener('focus', triggerThrottledRefresh)
    return () => {
      mountedRef.current = false
      document.removeEventListener('visibilitychange', triggerThrottledRefresh)
      window.removeEventListener('focus', triggerThrottledRefresh)
    }
  }, [submittedFromCheckout])

  return (
    <div className="portal-root">
      <PortalHeader anchorPrefix="/" />
      <main className="donate-main">
        <div className="portal-container donate-grid">
          <DonateActions
            submittedFromCheckout={submittedFromCheckout}
            moderationEnabled={wall.moderationEnabled}
          />
          <SupporterWall
            status={wall.status}
            refreshing={wall.refreshing}
            entries={wall.entries}
            nowSec={wall.nowSec}
            onRefresh={handleManualRefresh}
          />
        </div>
      </main>
      <PortalFooter />
    </div>
  )
}
