import type { TurnGrant } from '../../protocol/turn-control'

export function turnRTCConfiguration(grant: TurnGrant): RTCConfiguration {
  for (const raw of grant.urls) {
    // The pinned native peer supports IPv4 TURN allocations. Keep browser/native
    // capability claims aligned; ordinary direct IPv6 remains independent.
    if (!/^turns?:[^:/?#@\s[\]]+(?::[1-9]\d{0,4})?(?:\?transport=(?:udp|tcp))?$/.test(raw) ||
      (raw.startsWith('turns:') && raw.endsWith('transport=udp'))) throw new TypeError('Unsupported TURN endpoint')
    const port = /:(\d+)(?:\?|$)/.exec(raw)?.[1]
    if (port !== undefined && Number(port) > 65535) throw new TypeError('Invalid TURN port')
  }
  return { iceTransportPolicy: 'relay', iceServers: [{ urls: [...grant.urls],
    username: grant.credentials.username, credential: grant.credentials.password }] }
}
