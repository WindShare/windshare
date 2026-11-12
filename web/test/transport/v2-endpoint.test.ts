import { describe, expect, it } from 'vitest'
import { encodeSuite02CapabilityKey, parseSuite02CapabilityLink } from '../../src/crypto/suite02-link'

import { canonicalRelayEndpoint, relayEndpointForShare } from '../../src/transport/relay/endpoint'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface ShareRoutingCase {
  readonly name: string
  readonly relayBaseUrl: string
  readonly shareIdRawB64: string
  readonly accepted: boolean
  readonly dialEndpoint?: string
  readonly relayIdentityEndpoint?: string
  readonly relayIdentityB64?: string
}

const vectors = loadVectorFile(new URL('../../../core/testvectors/v2-session.json', import.meta.url))
const routing = vectors.cases.find(vector => vector.name === 'v2-relay-share-routing')
if (routing === undefined) throw new Error('missing relay share routing vectors')

it('connects implicit and explicit Unicode relay links with the same identity', async () => {
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(1), new Uint8Array(16).fill(2))
  const expected = await relayEndpointForShare('https://xn--mnchen-3ya.de', key.shareIdRaw)
  for (const relay of ['https://m\u00fcnchen.de', 'https://xn--mnchen-3ya.de']) {
    for (const base of [relay + '/' + key.shareId, 'https://front.example/' + key.shareId + '?r=' + encodeURIComponent(relay)]) {
      const capability = await parseSuite02CapabilityLink(base + '#' + key.encoded)
      expect(capability.relays).toHaveLength(1)
      const endpoint = await relayEndpointForShare(capability.relays[0]!, capability.shareIdRaw)
      expect(endpoint).toEqual(expected)
    }
  }
})

describe('relay share routing contract', () => {
  for (const vector of routing.cases as ShareRoutingCase[]) {
    it(vector.name, async () => {
      const pending = relayEndpointForShare(vector.relayBaseUrl, b64ToBytes(vector.shareIdRawB64))
      if (!vector.accepted) {
        await expect(pending).rejects.toThrow(TypeError)
        return
      }
      const endpoint = await pending
      expect(endpoint.dialEndpoint).toBe(vector.dialEndpoint)
      expect(endpoint.relayIdentityEndpoint).toBe(vector.relayIdentityEndpoint)
      expect(endpoint.relayIdentity).toEqual(b64ToBytes(vector.relayIdentityB64!))
      const base = await canonicalRelayEndpoint(vector.relayBaseUrl)
      expect(endpoint.relayIdentity).toEqual(base.relayIdentity)
      expect(endpoint.relayIdentityEndpoint).toBe(base.relayIdentityEndpoint)
    })
  }
})
