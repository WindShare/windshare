import { describe, expect, it } from 'vitest'

import type { CapabilityLink } from '../../src/contracts'
import { CryptoError, encodeCapabilityKey } from '../../src/crypto'
import {
  browserNavigation,
  consumeLocationCapability,
  type NavigationPort,
} from '../../src/ui/capability-source'

const SHARE_ID = 'AAECAwQFBgcI'
const KEY = encodeCapabilityKey(1, Uint8Array.from({ length: 16 }, (_, index) => index))
const BARE_URL = `https://windshare.test/v1/ws/${SHARE_ID}?r=https%3A%2F%2Frelay.test`

function navigation(url: string, events: string[] = []): NavigationPort {
  return {
    currentUrl: () => url,
    eraseFragment: (bare) => events.push(`erase:${bare}`),
  }
}

describe('capability source', () => {
  it('parses the capability before synchronously erasing its fragment', () => {
    const events: string[] = []
    const result = consumeLocationCapability(
      navigation(`${BARE_URL}#${KEY}`, events),
      () => {
        events.push('parse')
        return { shareId: SHARE_ID } as unknown as CapabilityLink
      },
    )

    expect(result.kind).toBe('ready')
    expect(events).toEqual(['parse', `erase:${BARE_URL}`])
  })

  it('erases even malformed fragments and never reflects parser text', () => {
    const events: string[] = []
    const secret = 'DO-NOT-REFLECT-THIS-SECRET'
    const result = consumeLocationCapability(
      navigation(`${BARE_URL}#${secret}`, events),
      () => {
        throw new CryptoError('malformed-key', `bad ${secret}`)
      },
    )

    expect(result).toEqual({ kind: 'invalid', message: 'This is not a valid WindShare link.' })
    expect(JSON.stringify(result)).not.toContain(secret)
    expect(events).toEqual([`erase:${BARE_URL}`])
  })

  it('requests a separate key for a valid link with no fragment', () => {
    const result = consumeLocationCapability(navigation(BARE_URL))

    expect(result).toEqual({ kind: 'needs-key', bareUrl: BARE_URL })
  })

  it('fails closed when browser history cannot erase the secret', () => {
    const result = consumeLocationCapability({
      currentUrl: () => `${BARE_URL}#${KEY}`,
      eraseFragment: () => {
        throw new Error('history unavailable')
      },
    })

    expect(result).toEqual({
      kind: 'invalid',
      message: 'The secret could not be removed from browser history.',
    })
  })

  it('adapts browser location and history without changing history state', () => {
    const calls: unknown[][] = []
    const state = { retained: true }
    const adapted = browserNavigation({
      location: { href: `${BARE_URL}#${KEY}` },
      history: {
        state,
        replaceState: (...args: unknown[]) => calls.push(args),
      },
    } as unknown as Window)

    expect(adapted.currentUrl()).toBe(`${BARE_URL}#${KEY}`)
    adapted.eraseFragment(BARE_URL)
    expect(calls).toEqual([[state, '', BARE_URL]])
  })
})
