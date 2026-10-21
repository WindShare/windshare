export interface BrowserIdentity {
  readonly userAgent?: string
  readonly userAgentData?: {
    readonly brands?: readonly { readonly brand: string }[]
    readonly mobile?: boolean
    readonly platform?: string
  }
}

export type ReceiveBufferPolicy =
  | { readonly strategy: 'chromium-video'; readonly reason: 'chromium-desktop' }
  | { readonly strategy: 'data-only'; readonly reason: string }

/** API support alone does not establish the Chromium socket-buffer side effect. */
export function browserReceiveBufferPolicy(browser: BrowserIdentity | undefined): ReceiveBufferPolicy {
  const agent = browser?.userAgent ?? ''
  const hints = browser?.userAgentData
  const chromium = hints?.brands === undefined
    ? /\b(?:Chrome|Chromium|HeadlessChrome)\//.test(agent)
    : hints.brands.some(({ brand }) => brand === 'Chromium')
  if (!chromium) return { strategy: 'data-only', reason: 'other-browser-engine' }

  // Mobile and macOS have not been validated for this resource trade-off.
  // Browser identity selects an optimization, never access to ordinary WebRTC.
  const mobile = hints?.mobile === true || /Android|iPhone|iPad|iPod|Mobile/.test(agent)
  const platform = hints?.platform
  const desktop = platform === undefined
    ? /Windows NT|X11; Linux|X11; Ubuntu/.test(agent)
    : platform === 'Windows' || platform === 'Linux'
  if (mobile || !desktop) return { strategy: 'data-only', reason: 'unverified-platform' }
  return { strategy: 'chromium-video', reason: 'chromium-desktop' }
}
