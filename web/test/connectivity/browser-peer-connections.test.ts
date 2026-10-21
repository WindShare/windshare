import { describe, expect, it, vi } from 'vitest'
import { BrowserPeerConnections } from '../../src/connectivity/browser-peer/connections'
import { browserReceiveBufferPolicy } from '../../src/connectivity/browser-peer/receive-buffer-policy'
import type { PeerProviderFact } from '../../src/connectivity/peer-set/provider-facts'

const CHROMIUM_DESKTOP = {
  userAgentData: { brands: [{ brand: 'Chromium' }], mobile: false, platform: 'Windows' },
}

describe('browser receive buffer compatibility', () => {
  it('limits the side effect to desktop Chromium without limiting ordinary WebRTC', () => {
    for (const platform of ['Windows', 'Linux']) {
      expect(browserReceiveBufferPolicy({ userAgentData: {
        brands: [{ brand: 'Not/A Brand' }, { brand: 'Chromium' }], platform, mobile: false,
      } }).strategy).toBe('chromium-video')
    }
    for (const platform of ['macOS', 'Android', 'iOS', 'Unknown']) {
      expect(browserReceiveBufferPolicy({ userAgentData: {
        brands: [{ brand: 'Chromium' }], platform, mobile: false,
      } }).strategy).toBe('data-only')
    }
    for (const agent of [
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/155.0.0.0 Safari/537.36 Edg/155.0.0.0',
      'Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/155.0.0.0 Safari/537.36',
    ]) expect(browserReceiveBufferPolicy({ userAgent: agent }).strategy).toBe('chromium-video')
    for (const agent of [
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gecko/20100101 Firefox/149.0',
      'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Version/26.0 Safari/605.1.15',
      'Mozilla/5.0 (iPhone) CriOS/155.0.0.0 Mobile/15E148 Safari/604.1',
      'Mozilla/5.0 (Linux; Android 16) Chrome/155.0.0.0 Mobile Safari/537.36',
      '',
    ]) expect(browserReceiveBufferPolicy({ userAgent: agent }).strategy).toBe('data-only')
    expect(browserReceiveBufferPolicy(undefined).strategy).toBe('data-only')
    expect(browserReceiveBufferPolicy({ userAgentData: {
      ...CHROMIUM_DESKTOP.userAgentData, mobile: true,
    } }).strategy).toBe('data-only')
  })

  it('bundles receive-only media with data without mutating caller configuration', () => {
    const fixture = connectionsFixture()
    const configuration: RTCConfiguration = {
      iceServers: [{ urls: ['turn:example.test'], username: 'user', credential: 'credential' }],
      iceTransportPolicy: 'relay', bundlePolicy: 'balanced',
    }
    const before = structuredClone(configuration)
    const connection = fixture.connections.create(configuration, fixture.observe)
    expect(fixture.create).toHaveBeenCalledOnce()
    expect(fixture.create.mock.calls[0]![0]).toEqual({ ...configuration, bundlePolicy: 'max-bundle' })
    expect(configuration).toEqual(before)
    expect(fixture.peers[0]!.addTransceiver).toHaveBeenCalledExactlyOnceWith('video', { direction: 'recvonly' })
    connection.answerApplied()
    expect(fixture.facts.map((fact) => fact.stage)).toEqual(['selected', 'applied', 'negotiated'])
    expect(fixture.peers[0]!.close).not.toHaveBeenCalled()
  })

  it('discards a partially initialized peer and stays data-only for subsequent attempts', () => {
    const fixture = connectionsFixture()
    fixture.create.mockImplementationOnce((configuration) => {
      const peer = fakePeer()
      peer.addTransceiver.mockImplementation(() => { throw new DOMException('Unavailable media', 'NotSupportedError') })
      fixture.peers.push(peer)
      expect(configuration.bundlePolicy).toBe('max-bundle')
      return peer as unknown as RTCPeerConnection
    })
    const connection = fixture.connections.create({}, fixture.observe)
    expect(fixture.create).toHaveBeenCalledTimes(2)
    expect(fixture.peers[0]!.close).toHaveBeenCalledOnce()
    expect(connection.peer).toBe(fixture.peers[1])
    expect(fixture.peers[1]!.addTransceiver).not.toHaveBeenCalled()
    fixture.connections.create({}, fixture.observe)
    expect(fixture.peers[2]!.addTransceiver).not.toHaveBeenCalled()
    expect(fixture.facts).toContainEqual({
      kind: 'receive-buffer', strategy: 'chromium-video', stage: 'disabled',
      reason: 'add-transceiver-failed', errorName: 'NotSupportedError',
    })
  })

  it('keeps an accepted data connection usable when auxiliary media is rejected', () => {
    const fixture = connectionsFixture()
    const connection = fixture.connections.create({}, fixture.observe)
    fixture.peers[0]!.transceiver.currentDirection = 'inactive'
    connection.answerApplied()
    expect(fixture.peers[0]!.close).not.toHaveBeenCalled()
    fixture.connections.create({}, fixture.observe)
    expect(fixture.peers[1]!.addTransceiver).not.toHaveBeenCalled()
    expect(fixture.facts.at(-2)).toMatchObject({ stage: 'disabled', reason: 'auxiliary-video-not-negotiated' })
  })

  it('cannot let an observer failure disable a healthy connection', () => {
    const fixture = connectionsFixture()
    const connection = fixture.connections.create({}, () => { throw new Error('Broken observer') })
    expect(() => connection.answerApplied()).not.toThrow()
    expect(fixture.create).toHaveBeenCalledOnce()
    expect(fixture.peers[0]!.close).not.toHaveBeenCalled()
  })

  it('does not allocate media on other browser engines', () => {
    const create = vi.fn(() => fakePeer() as unknown as RTCPeerConnection)
    const connections = new BrowserPeerConnections({ createPeerConnection: create, browser: { userAgent: 'Firefox/149' } })
    const connection = connections.create({}, () => undefined)
    expect(connection.peer.addTransceiver).not.toHaveBeenCalled()
  })
})

function connectionsFixture() {
  const peers: ReturnType<typeof fakePeer>[] = []
  const facts: Extract<PeerProviderFact, { kind: 'receive-buffer' }>[] = []
  const create = vi.fn<(configuration: RTCConfiguration) => RTCPeerConnection>(() => {
    const peer = fakePeer()
    peers.push(peer)
    return peer as unknown as RTCPeerConnection
  })
  const connections = new BrowserPeerConnections({ createPeerConnection: create, browser: CHROMIUM_DESKTOP })
  return { peers, create, connections, facts, observe: (fact: typeof facts[number]) => { facts.push(fact) } }
}

function fakePeer() {
  const transceiver = { currentDirection: 'recvonly' }
  return { transceiver, addTransceiver: vi.fn(() => transceiver), close: vi.fn() }
}
