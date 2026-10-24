import { describe, expect, it } from 'vitest'

import { openShareDescriptor } from '../../src/catalog/records'
import { equalBytes } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { decodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import { isTerminalRecoveryFailure } from '../../src/receiver/recovery-failure'
import {
  decodeChallenge,
  decodeDescriptorUpload,
  decodeRegisterInit,
  decodeRegisterProof,
  registrationProofPreimage,
} from '../../src/transport/relay/protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface UploadCase extends VectorCase {
  readonly registerInitB64: string
  readonly preimageB64: string
  readonly registerProofB64: string
  readonly descriptorUploadB64: string
  readonly relayUploadAuthorized: boolean
  readonly receiverObjectValid: boolean
}

interface UploadVector extends VectorCase {
  readonly relayIdentityB64: string
  readonly registerChallengeB64: string
  readonly cases: readonly UploadCase[]
}

const sessionVectors = loadVectorFile(new URL('../../../core/testvectors/v2-session.json', import.meta.url))
const vector = sessionVectors.cases.find(
  candidate => candidate.name === 'relay-descriptor-upload-authorization',
) as UploadVector
const identity = loadVectorFile(new URL('../../../core/testvectors/v2-identity.json', import.meta.url))
  .cases.find(candidate => candidate.name === 'suite-02-link-and-keys') as VectorCase & { keyString: string }

describe('descriptor upload authorization boundary', () => {
  for (const test of vector.cases) {
    it(test.name, async () => {
      const init = await decodeRegisterInit(b64ToBytes(test.registerInitB64))
      const challenge = decodeChallenge(b64ToBytes(vector.registerChallengeB64))
      const proof = decodeRegisterProof(b64ToBytes(test.registerProofB64))
      const preimage = await registrationProofPreimage(init, challenge, b64ToBytes(vector.relayIdentityB64))
      expect(preimage).toEqual(b64ToBytes(test.preimageB64))
      expect(await createEd25519Verifier(proof.senderPublicKey).verify(preimage, proof.signature)).toBe(true)

      const object = decodeDescriptorUpload(b64ToBytes(test.descriptorUploadB64))
      expect(equalBytes(await sha256(object), init.descriptorDigest)).toBe(test.relayUploadAuthorized)

      const capability = await decodeSuite02CapabilityKey(identity.keyString)
      const opened = openShareDescriptor(object, capability)
      if (test.receiverObjectValid) {
        await expect(opened).resolves.toMatchObject({ shareInstance: init.shareInstance })
      } else {
        const failure: unknown = await opened.catch((cause: unknown) => cause)
        expect(failure).toBeInstanceOf(Error)
        expect(isTerminalRecoveryFailure(failure)).toBe(true)
      }
    })
  }
})
