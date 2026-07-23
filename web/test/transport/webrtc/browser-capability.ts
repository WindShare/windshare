import { test, type Page } from '@playwright/test'

import type * as PeerOffer from '../../../src/connectivity/peer-offer'

const PEER_OFFER_PATH = '/src/connectivity/peer-offer.ts'

export async function requireNativePeerConnection(page: Page): Promise<void> {
  await page.goto('/')
  const available = await page.evaluate(async (path) => {
    const peerOffer = await import(path) as typeof PeerOffer
    return peerOffer.browserPeerConnectionAvailable()
  }, PEER_OFFER_PATH)
  // Native DataChannel semantics have no truthful substitute when the engine omits WebRTC.
  test.skip(
    !available,
    'The active browser runtime does not expose a native RTCPeerConnection',
  )
}

export async function requireConfinedPionIceCandidate(
  page: Page,
  browserName: string,
): Promise<void> {
  // Chromium can establish this confined pair through a peer-reflexive route;
  // Firefox requires a compatible local candidate when the host VPN owns its default route.
  if (browserName !== 'firefox') return
  const available = await page.evaluate(async () => {
    const peer = new RTCPeerConnection({ iceServers: [] })
    const channel = peer.createDataChannel('windshare-loopback-capability')
    try {
      await peer.setLocalDescription(await peer.createOffer())
      if (peer.iceGatheringState !== 'complete') {
        await new Promise<void>((resolve) => {
          const changed = () => {
            if (peer.iceGatheringState !== 'complete') return
            peer.removeEventListener('icegatheringstatechange', changed)
            resolve()
          }
          peer.addEventListener('icegatheringstatechange', changed)
        })
      }
      const lines = peer.localDescription?.sdp.split(/\r?\n/u) ?? []
      return lines.some((line) => {
        if (!line.startsWith('a=candidate:')) return false
        const address = line.split(' ')[4]
        return address === '::1' || address?.startsWith('127.') === true
      })
    } finally {
      channel.close()
      peer.close()
    }
  })
  // The helper intentionally has no wildcard mDNS listener or non-loopback ICE socket.
  test.skip(
    !available,
    'The active browser cannot publish a literal loopback ICE candidate to the confined Pion fixture',
  )
}
