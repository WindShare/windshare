import { describe, expect, it } from 'vitest'
import { BrowserPeerRoute } from '../../src/connectivity/peer-route/route'

class PairTransport extends EventTarget {
  pair: { local: { type: unknown }; remote: { type: unknown } } | null = {
    local: { type: 'host' }, remote: { type: 'host' },
  }

  getSelectedCandidatePair() { return this.pair }

  change(type: unknown): void {
    this.pair = type === null ? null : { local: { type: 'host' }, remote: { type } }
    this.dispatchEvent(new Event('selectedcandidatepairchange'))
  }
}

describe('browser peer route authority', () => {
  it('uses the live selected pair instead of missing or contradictory stats', () => {
    const transport = new PairTransport()
    const changes: unknown[] = []
    const route = new BrowserPeerRoute(() => transport, (value) => changes.push(value))
    route.refresh()
    route.observeStats(undefined)
    route.observeStats('turn')
    expect(route.current).toBe('direct')
    expect(changes).toEqual(['direct'])
    route.close()
  })

  it('publishes native route changes and actual pair loss immediately', () => {
    const transport = new PairTransport()
    const changes: unknown[] = []
    const route = new BrowserPeerRoute(() => transport, (value) => changes.push(value))
    route.refresh()
    transport.change('relay')
    expect(route.current).toBe('turn')
    transport.change(null)
    route.observeStats('direct')
    expect(route.current).toBeUndefined()
    expect(changes).toEqual(['direct', 'turn', undefined])
    route.close()
  })

  it('discovers a late ICE transport and releases replaced and closed listeners', () => {
    let transport: PairTransport | undefined
    const route = new BrowserPeerRoute(() => transport, () => undefined)
    route.refresh()
    expect(route.current).toBeUndefined()
    const previous = new PairTransport()
    transport = previous
    route.refresh()
    expect(route.current).toBe('direct')
    transport = new PairTransport()
    transport.change('relay')
    route.refresh()
    previous.change(null)
    expect(route.current).toBe('turn')
    route.close()
    transport.change(null)
    route.refresh()
    expect(route.current).toBe('turn')
  })

  it('uses positive stats evidence only when the provider has no native pair API', () => {
    const transport = new EventTarget()
    const route = new BrowserPeerRoute(() => transport, () => undefined)
    route.observeStats('turn')
    route.observeStats(undefined)
    expect(route.current).toBe('turn')
    route.observeStats('direct')
    expect(route.current).toBe('direct')
    route.close()
  })

  it.each([undefined, null, 'unknown', 'invalid'])('does not guess a route for candidate type %s', (type) => {
    const transport = new PairTransport()
    transport.pair!.remote.type = type
    const route = new BrowserPeerRoute(() => transport, () => undefined)
    route.observeStats('direct')
    expect(route.current).toBeUndefined()
    route.close()
  })

  it('handles a native pair getter failure without trusting stale stats', () => {
    const transport = new PairTransport()
    const route = new BrowserPeerRoute(() => transport, () => undefined)
    route.refresh()
    transport.getSelectedCandidatePair = () => { throw new Error('transport unavailable') }
    route.observeStats('direct')
    expect(route.current).toBeUndefined()
    route.close()
  })
})
