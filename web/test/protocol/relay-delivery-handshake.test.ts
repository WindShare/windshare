import { describe, expect, it } from 'vitest'
import {
  decodeDeliveryJoin, decodeDeliveryRegisterInit, deliveryRegistrationProofPreimage,
  encodeDeliveryJoin, encodeDeliveryRegisterInit,
} from '../../src/transport/relay/delivery-handshake-codec'
import { decodeV2Challenge, decodeV2RegisterProof, type V2ChallengePurpose } from '../../src/transport/relay/v2-protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { ed25519 } from '@noble/curves/ed25519.js'
import { suite02SenderKeyHash } from '../../src/crypto/suite02-link'
import { equalBytes } from '../../src/crypto/bytes'

const vectors = loadVectorFile(new URL('../../../core/testvectors/v2-session.json', import.meta.url)).cases
const frames = vectors.find(value => value.name === 'relay-capacity-frame-decoding') as VectorCase & {
  cases: { name: string; frame: string; encodedB64: string; accepted: boolean }[]
}
const registration = vectors.find(value => value.name === 'relay-capacity-registration') as VectorCase & Record<string, string>
const proofs = vectors.find(value => value.name === 'relay-capacity-proof-authentication') as VectorCase & {
  relayIdentityB64: string
  cases: { name: string; operation: string; initB64: string; challengePurpose: V2ChallengePurpose; challengeIdB64: string; challengeNonceB64: string; challengeExpiresAt: string; proofB64: string; nowUnixSeconds: string; authenticated: boolean }[]
}

describe('staged delivery handshake', () => {
  it('matches shared decoding and encoding decisions', async () => {
    for (const test of frames.cases) {
      const raw = b64ToBytes(test.encodedB64)
      const roundTrip = async () => test.frame === 'WS2R'
        ? encodeDeliveryRegisterInit(await decodeDeliveryRegisterInit(raw))
        : encodeDeliveryJoin(decodeDeliveryJoin(raw))
      if (test.accepted) await expect(roundTrip(), test.name).resolves.toEqual(raw)
      else await expect(roundTrip(), test.name).rejects.toThrow()
    }
  })

  it('pins REGISTER and RESUME signature preimages byte for byte', async () => {
    for (const [initKey, challengeKey, preimageKey] of [
      ['registerInitB64', 'registerChallengeB64', 'preimageB64'],
      ['resumeInitB64', 'resumeChallengeB64', 'resumePreimageB64'],
    ] as const) {
      const init = await decodeDeliveryRegisterInit(b64ToBytes(registration[initKey]!))
      const challenge = decodeV2Challenge(b64ToBytes(registration[challengeKey]!))
      expect(await deliveryRegistrationProofPreimage(init, challenge, b64ToBytes(registration.relayIdentityB64!)))
        .toEqual(b64ToBytes(registration[preimageKey]!))
    }
  })

  it('binds both capacity dimensions in authenticated proofs', async () => {
    for (const test of proofs.cases) {
      if (test.operation !== 'registration') continue
      const init = await decodeDeliveryRegisterInit(b64ToBytes(test.initB64))
      const proof = decodeV2RegisterProof(b64ToBytes(test.proofB64))
      const challenge = {
        purpose: test.challengePurpose, id: b64ToBytes(test.challengeIdB64),
        nonce: b64ToBytes(test.challengeNonceB64), expiresAtUnixSeconds: BigInt(test.challengeExpiresAt),
      }
      let preimage: Uint8Array
      try { preimage = await deliveryRegistrationProofPreimage(init, challenge, b64ToBytes(proofs.relayIdentityB64)) }
      catch { expect(test.authenticated, test.name).toBe(false); continue }
      const authenticated = proof.mode === init.registration.mode && BigInt(test.nowUnixSeconds) < challenge.expiresAtUnixSeconds &&
        equalBytes(await suite02SenderKeyHash(proof.senderPublicKey), init.registration.pkHash) &&
        ed25519.verify(proof.signature, preimage, proof.senderPublicKey, { zip215: false })
      expect(authenticated, test.name).toBe(test.authenticated)
    }
  })

  it('rejects zero, negative, oversized and Number capacity values before encoding', () => {
    const shareId = b64ToBytes(registration.joinB64!).slice(8, 20)
    for (const invalid of [0n, -1n, 1n << 64n, 1 as unknown as bigint]) {
      for (const receiveCapacity of [{ frames: invalid, encodedBytes: 1n }, { frames: 1n, encodedBytes: invalid }]) {
        expect(() => encodeDeliveryJoin({ shareId, receiveCapacity })).toThrow()
      }
    }
  })
})
