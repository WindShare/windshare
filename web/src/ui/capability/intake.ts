import type { V2CapturedLocation } from './location'

export const TRACE_QUERY_PARAMETER = 'trace'
const TRACE_QUERY_ENABLED = '1'

export function requestsBrowserTrace(input: string): boolean {
  try { return new URL(input).searchParams.get(TRACE_QUERY_PARAMETER) === TRACE_QUERY_ENABLED } catch {
    // Bare keys and malformed links still belong to normal capability validation.
    return false
  }
}

/** Presentation intent is consumed before joining, independently of link validation. */
export class BrowserCapabilityIntake {
  readonly #startDiagnostics: () => void

  constructor(startDiagnostics: () => void) {
    this.#startDiagnostics = startDiagnostics
  }

  accept(captured: V2CapturedLocation): void {
    if (captured.diagnosticsRequested === true ||
        (captured.capabilityInput !== null && requestsBrowserTrace(captured.capabilityInput))) {
      this.#startDiagnostics()
    }
  }
}
