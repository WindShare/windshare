import { isPortalFragment } from '../portal/navigation'

import { requestsBrowserTrace, TRACE_QUERY_PARAMETER } from './intake'

export interface V2CapturedLocation {
  readonly capabilityInput: string | null
  readonly pageUrl: string
  readonly diagnosticsRequested?: boolean
}

export interface BrowserLocationCapture extends V2CapturedLocation {
  readonly diagnosticsRequested: boolean
}

export interface V2LocationCaptureOptions {
  readonly onSecurityMilestone?: (milestone: 'location-cleared') => void
}

type LocationPort = Pick<Window, 'location' | 'history'>
type LocationEventsPort = LocationPort & Pick<Window, 'addEventListener' | 'removeEventListener'>

export function captureV2Location(
  windowPort: LocationPort = window,
  options: V2LocationCaptureOptions = {},
): BrowserLocationCapture {
  const input = windowPort.location.href
  const sanitized = new URL(input)
  const capabilityInput = sanitized.hash.length > 1 && !isPortalFragment(sanitized.hash)
    ? input : null
  const diagnosticsRequested = requestsBrowserTrace(input)
  const hasDiagnosticsParameter = sanitized.searchParams.has(TRACE_QUERY_PARAMETER)
  if (hasDiagnosticsParameter) sanitized.searchParams.delete(TRACE_QUERY_PARAMETER)
  if (capabilityInput !== null) sanitized.hash = ''
  if (capabilityInput !== null || hasDiagnosticsParameter) {
    // Consume activation with the credentials so reload neither re-enables a stopped
    // capture nor renews its deadline. Portal anchors retain their navigation meaning.
    windowPort.history.replaceState(windowPort.history.state, '', sanitized)
  }
  if (capabilityInput !== null) {
    try {
      options.onSecurityMilestone?.('location-cleared')
    } catch {
      // Observers cannot prevent the captured capability from reaching its owner.
    }
  }
  sanitized.hash = ''
  return Object.freeze({ capabilityInput, pageUrl: sanitized.href, diagnosticsRequested })
}

/** One document owns its listener even when React remounts or the page enters bfcache. */
export function observeV2Location(
  windowPort: LocationEventsPort,
  accept: (captured: BrowserLocationCapture) => void,
): () => void {
  const changed = () => {
    // Queued hashchange events may describe a URL already replaced by a newer link.
    const captured = captureV2Location(windowPort)
    if (captured.capabilityInput !== null || captured.diagnosticsRequested) accept(captured)
  }
  windowPort.addEventListener('hashchange', changed)
  windowPort.addEventListener('pageshow', changed)
  return () => {
    windowPort.removeEventListener('hashchange', changed)
    windowPort.removeEventListener('pageshow', changed)
  }
}
