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
    entries: response.entries,
    nowSec: Math.floor(Date.now() / 1000),
  }
}

function toDegradedWallState(prev: WallViewState): WallViewState {
  return {
    ...prev,
    // Keep previously loaded entries intact if a subsequent refresh fails.
    status: prev.status === 'ready' ? 'ready' : 'degraded',
    refreshing: false,
  }
}

function toRefreshingWallState(prev: WallViewState): WallViewState {
  if (prev.status === 'loading' || prev.refreshing) return prev
  return { ...prev, refreshing: true }
}

export default function DonatePage() {
  const [submittedFromCheckout] = useState(hasSubmittedStatusQuery)
  const [wall, setWall] = useState<WallViewState>(() => ({
    status: 'loading',
    refreshing: false,
    entries: [],
    nowSec: Math.floor(Date.now() / 1000),
  }))

  const lastFetchAtRef = useRef(0)
  // Single synchronous owner for the in-flight wall request so manual clicks
  // promote a silent refresh into visible progress and unmount aborts cleanly.
  const activeRequestRef = useRef<AbortController | null>(null)

  const startWallFetch = useCallback((bypassCache: boolean) => {
    if (activeRequestRef.current !== null) return

    const controller = new AbortController()
    activeRequestRef.current = controller
    lastFetchAtRef.current = Date.now()
    void fetchDonationWall({
      ...(bypassCache ? { cache: 'no-cache' } : {}),
      signal: controller.signal,
    })
      .then(response => {
        if (!controller.signal.aborted) setWall(toReadyWallState(response))
      })
      .catch(() => {
        if (!controller.signal.aborted) setWall(toDegradedWallState)
      })
      .finally(() => {
        if (activeRequestRef.current === controller) activeRequestRef.current = null
      })
  }, [])

  const handleManualRefresh = useCallback(() => {
    // Promote an already in-flight silent refresh to visible progress instead
    // of swallowing the click or discarding its RTT with an abort-and-retry.
    setWall(toRefreshingWallState)
    startWallFetch(true)
  }, [startWallFetch])

  useEffect(() => {
    startWallFetch(submittedFromCheckout)

    const triggerThrottledRefresh = () => {
      if (document.visibilityState !== 'visible') return
      if (Date.now() - lastFetchAtRef.current < SILENT_REFRESH_THROTTLE_MS) return
      startWallFetch(true)
    }

    document.addEventListener('visibilitychange', triggerThrottledRefresh)
    window.addEventListener('focus', triggerThrottledRefresh)
    return () => {
      activeRequestRef.current?.abort()
      activeRequestRef.current = null
      document.removeEventListener('visibilitychange', triggerThrottledRefresh)
      window.removeEventListener('focus', triggerThrottledRefresh)
    }
  }, [submittedFromCheckout, startWallFetch])

  return (
    <div className="portal-root">
      <PortalHeader anchorPrefix="/" />
      <main className="donate-main">
        <div className="portal-container donate-grid">
          <DonateActions submittedFromCheckout={submittedFromCheckout} />
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
