import type { CapabilityLink } from '../contracts'
import { CryptoError, parseCapabilityLink } from '../crypto'

export interface NavigationPort {
  currentUrl(): string
  eraseFragment(urlWithoutFragment: string): void
}

export type InitialCapability =
  | { readonly kind: 'ready'; readonly capability: CapabilityLink }
  | { readonly kind: 'needs-key'; readonly bareUrl: string }
  | { readonly kind: 'invalid'; readonly message: string }

export type CapabilityParser = (rawUrl: string) => CapabilityLink

const INVALID_LINK_MESSAGE = 'This is not a valid WindShare link.'
const HISTORY_FAILURE_MESSAGE = 'The secret could not be removed from browser history.'

/** Parse first, then erase in the same synchronous turn before any async join. */
export function consumeLocationCapability(
  navigation: NavigationPort,
  parser: CapabilityParser = parseCapabilityLink,
): InitialCapability {
  const rawUrl = navigation.currentUrl()
  const fragmentAt = rawUrl.indexOf('#')
  const bareUrl = fragmentAt === -1 ? rawUrl : rawUrl.slice(0, fragmentAt)
  let result: InitialCapability

  try {
    result = { kind: 'ready', capability: parser(rawUrl) }
  } catch (error) {
    result =
      error instanceof CryptoError && error.code === 'missing-key'
        ? { kind: 'needs-key', bareUrl }
        : { kind: 'invalid', message: INVALID_LINK_MESSAGE }
  } finally {
    if (fragmentAt !== -1) {
      try {
        navigation.eraseFragment(bareUrl)
      } catch {
        result = { kind: 'invalid', message: HISTORY_FAILURE_MESSAGE }
      }
    }
  }
  return result
}

export function browserNavigation(browserWindow: Window): NavigationPort {
  return Object.freeze({
    currentUrl: () => browserWindow.location.href,
    eraseFragment: (urlWithoutFragment: string) => {
      browserWindow.history.replaceState(
        browserWindow.history.state,
        '',
        urlWithoutFragment,
      )
    },
  })
}
