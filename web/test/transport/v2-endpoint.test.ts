import { describe, expect, it } from 'vitest'

import { canonicalV2RelayEndpoint, v2RelayEndpointForShare } from '../../src/transport/relay/v2-endpoint'
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

describe('relay share routing contract', () => {
  for (const vector of routing.cases as ShareRoutingCase[]) {
    it(vector.name, async () => {
      const pending = v2RelayEndpointForShare(vector.relayBaseUrl, b64ToBytes(vector.shareIdRawB64))
      if (!vector.accepted) {
        await expect(pending).rejects.toThrow(TypeError)
        return
      }
      const endpoint = await pending
      expect(endpoint.dialEndpoint).toBe(vector.dialEndpoint)
      expect(endpoint.relayIdentityEndpoint).toBe(vector.relayIdentityEndpoint)
      expect(endpoint.relayIdentity).toEqual(b64ToBytes(vector.relayIdentityB64!))
      const base = await canonicalV2RelayEndpoint(vector.relayBaseUrl)
      expect(endpoint.relayIdentity).toEqual(base.relayIdentity)
      expect(endpoint.relayIdentityEndpoint).toBe(base.relayIdentityEndpoint)
    })
  }
})
