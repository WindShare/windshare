import { isPortalFragment } from '../portal/navigation'

export interface V2CapturedLocation {
  readonly capabilityInput: string | null
  readonly pageUrl: string
}

export interface V2LocationCaptureOptions {
  readonly onSecurityMilestone?: (milestone: 'location-cleared') => void
}

type LocationPort = Pick<Window, 'location' | 'history'>
type LocationEventsPort = LocationPort & Pick<Window, 'addEventListener' | 'removeEventListener'>

export function captureV2Location(
  windowPort: LocationPort = window,
  options: V2LocationCaptureOptions = {},
): V2CapturedLocation {
  const input = windowPort.location.href
  const sanitized = new URL(input)
  const capabilityInput = sanitized.hash.length > 1 && !isPortalFragment(sanitized.hash)
    ? input : null
  sanitized.hash = ''
  if (capabilityInput !== null) {
    // Erase credentials synchronously, before parsing, discovery, or a navigation decision.
    windowPort.history.replaceState(windowPort.history.state, '', sanitized)
    try {
      options.onSecurityMilestone?.('location-cleared')
    } catch {
      // Observers cannot prevent the captured capability from reaching its owner.
    }
  }
  return Object.freeze({ capabilityInput, pageUrl: sanitized.href })
}

/** One document owns its listener even when React remounts or the page enters bfcache. */
export function observeV2Location(
  windowPort: LocationEventsPort,
  accept: (captured: V2CapturedLocation) => void,
): () => void {
  const changed = () => {
    // Queued hashchange events may describe a URL already replaced by a newer link.
    const captured = captureV2Location(windowPort)
    if (captured.capabilityInput !== null) accept(captured)
  }
  windowPort.addEventListener('hashchange', changed)
  windowPort.addEventListener('pageshow', changed)
  return () => {
    windowPort.removeEventListener('hashchange', changed)
    windowPort.removeEventListener('pageshow', changed)
  }
}
