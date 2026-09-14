import { describe, expect, it, vi } from 'vitest'
import { captureV2Location, observeV2Location } from '../../src/ui/capability/location'
import { PORTAL_SECTIONS } from '../../src/ui/portal/navigation'

describe('document capability intake', () => {
  it('preserves every portal anchor at startup and during same-document navigation', () => {
    for (const section of Object.values(PORTAL_SECTIONS)) {
      const port = locationPort(`https://receiver.invalid/s/share#${section.id}`)
      const accepted = vi.fn()
      expect(captureV2Location(port).capabilityInput).toBeNull()
      const close = observeV2Location(port, accepted)
      port.dispatchEvent(new Event('hashchange'))
      expect(port.location.href).toContain(`#${section.id}`)
      expect(accepted).not.toHaveBeenCalled()
      expect(port.history.replaceState).not.toHaveBeenCalled()
      close()
    }
  })

  it('erases each incoming capability before handoff and consumes queued events only once', () => {
    const port = locationPort('https://receiver.invalid/s/share')
    const accepted = vi.fn((captured: ReturnType<typeof captureV2Location>) => {
      expect(new URL(port.location.href).hash).toBe('')
      expect(captured.capabilityInput).toBe('https://receiver.invalid/s/share#new-key')
    })
    const close = observeV2Location(port, accepted)
    port.location.href += '#old-key'
    // The browser can queue multiple events before the application handles the latest URL.
    port.location.href = 'https://receiver.invalid/s/share#new-key'
    port.dispatchEvent(new Event('hashchange'))
    port.dispatchEvent(new Event('hashchange'))
    expect(accepted).toHaveBeenCalledTimes(1)
    expect(port.history.state).toEqual({ scroll: 'preserved' })
    close()
    port.location.href += '#after-dispose'
    port.dispatchEvent(new Event('hashchange'))
    expect(accepted).toHaveBeenCalledTimes(1)
  })

  it('captures a restored page location and ignores empty fragments', () => {
    const port = locationPort('https://receiver.invalid/s/share')
    const accepted = vi.fn()
    const close = observeV2Location(port, accepted)
    port.dispatchEvent(new Event('hashchange'))
    expect(accepted).not.toHaveBeenCalled()
    port.location.href += '#restored-key'
    port.dispatchEvent(new Event('pageshow'))
    expect(accepted).toHaveBeenCalledTimes(1)
    close()
    port.location.href += '#closed'
    port.dispatchEvent(new Event('pageshow'))
    expect(accepted).toHaveBeenCalledTimes(1)
  })

  it('keeps credential erasure authoritative when an observer fails', () => {
    const port = locationPort('https://receiver.invalid/s/share#invalid-key')
    const captured = captureV2Location(port, { onSecurityMilestone: () => { throw new Error('observer failed') } })
    expect(captured.capabilityInput).not.toBeNull()
    expect(new URL(port.location.href).hash).toBe('')
  })
})

function locationPort(href: string) {
  const events = new EventTarget()
  const location = { href }
  return {
    location,
    history: {
      state: { scroll: 'preserved' },
      replaceState: vi.fn((_state: unknown, _unused: string, url: string | URL | null | undefined) => {
        location.href = String(url)
      }),
    },
    addEventListener: events.addEventListener.bind(events) as Window['addEventListener'],
    removeEventListener: events.removeEventListener.bind(events) as Window['removeEventListener'],
    dispatchEvent: events.dispatchEvent.bind(events),
  } as unknown as Window & { history: { replaceState: ReturnType<typeof vi.fn> } }
}
