import { decode } from 'cborg'
import { describe, expect, it } from 'vitest'

import {
  createBlockRecordObjectBinding,
  createCatalogPageObjectBinding,
  createDescriptorObjectBinding,
  createDirectoryErrorObjectBinding,
  createOfflineCommitObjectBinding,
  createRevisionObjectBinding,
  openDescriptorObjectBootstrap,
  openSenderObject,
  senderObjectAuthenticationData,
  senderObjectSignaturePreimage,
  SenderObjectBinding,
} from '../../src/crypto/sender-object'
import {
  deriveSuite02CatalogKey,
  deriveSuite02DescriptorKey,
  deriveSuite02FileObjectKey,
  deriveSuite02FileSegmentKey,
  deriveSuite02RevisionKey,
  deriveSuite02SessionAuthKey,
} from '../../src/crypto/suite02-key-derivation'
import {
  decodeSuite02CapabilityKey,
  encodeSuite02CapabilityKey,
  parseSuite02CapabilityLink,
  suite02SenderKeyHash,
} from '../../src/crypto/suite02-link'
import {
  V2LaneResponseAuthority,
  V2_LANE_REJECT,
  decodeV2LaneHello,
  encodeV2LaneHello,
  v2LaneAcceptBody,
  v2LaneRejectBody,
  verifyV2LaneAccept,
  verifyV2LaneReject,
} from '../../src/session/v2-lane-codec'
import { canonicalV2RelayEndpoint } from '../../src/transport/relay/v2-endpoint'
import {
  V2_CHALLENGE_PURPOSE,
  V2_REGISTRATION_MODE,
  V2_RELAY_ERROR,
  decodeV2Challenge,
  decodeV2DescriptorDelivery,
  decodeV2DescriptorUpload,
  decodeV2Join,
  decodeV2OpaqueRoute,
  decodeV2RegisterInit,
  decodeV2RegisterProof,
  decodeV2Registered,
  decodeV2RelayError,
  decodeV2ResumeCredential,
  decodeV2SessionRetired,
  decodeV2StopInit,
  decodeV2StopProof,
  decodeV2Stopped,
  encodeV2Challenge,
  encodeV2DescriptorDelivery,
  encodeV2DescriptorUpload,
  encodeV2Join,
  encodeV2OpaqueRoute,
  encodeV2RegisterInit,
  encodeV2RegisterProof,
  encodeV2Registered,
  encodeV2RelayError,
  encodeV2ResumeCredential,
  encodeV2SessionRetired,
  encodeV2StopInit,
  encodeV2StopProof,
  encodeV2Stopped,
  v2RegistrationProofPreimage,
  v2StopProofPreimage,
  type V2Challenge,
  type V2RegisterInit,
  type V2StopInit,
} from '../../src/transport/relay/v2-protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface IdentityVector extends VectorCase {
  readonly readSecretB64: string
  readonly senderPublicKeyB64: string
  readonly pkHashB64: string
  readonly shareInstanceB64: string
  readonly shareIdRawB64: string
  readonly shareId: string
  readonly keyString: string
  readonly descriptorKeyB64: string
  readonly catalogKeyB64: string
  readonly fileIdB64: string
  readonly fileObjectKeyB64: string
  readonly fileRevisionB64: string
  readonly revisionKeyB64: string
  readonly segment: string
  readonly fileSegmentKeyB64: string
}

interface SenderObjectVector extends VectorCase {
  readonly domain: string
  readonly contextB64: string
  readonly keyB64: string
  readonly canonicalCborB64: string
  readonly aadB64: string
  readonly signaturePreimageB64: string
  readonly objectB64: string
}

interface RegistrationVector extends VectorCase {
  readonly relayBaseUrl: string
  readonly dialEndpoint: string
  readonly relayIdentityEndpoint: string
  readonly descriptorDigestB64: string
  readonly resumeTokenB64: string
  readonly resumeTokenHashB64: string
  readonly relayIdentityB64: string
  readonly challengeIdB64: string
  readonly challengeNonceB64: string
  readonly expiresAt: string
  readonly preimageB64: string
  readonly signatureB64: string
  readonly registerInitB64: string
  readonly registerChallengeB64: string
  readonly registerProofB64: string
  readonly descriptorUploadB64: string
  readonly registeredB64: string
  readonly joinB64: string
  readonly relaySessionIdB64: string
  readonly descriptorDeliveryB64: string
  readonly resumeChallengeIdB64: string
  readonly resumeChallengeNonceB64: string
  readonly resumeExpiresAt: string
  readonly resumeInitB64: string
  readonly resumeChallengeB64: string
  readonly resumePreimageB64: string
  readonly resumeSignatureB64: string
  readonly resumeProofB64: string
  readonly resumeCredentialB64: string
  readonly stopIdB64: string
  readonly stopChallengeIdB64: string
  readonly stopChallengeNonceB64: string
  readonly stopExpiresAt: string
  readonly stopInitB64: string
  readonly stopChallengeB64: string
  readonly stopPreimageB64: string
  readonly stopSignatureB64: string
  readonly stopProofB64: string
  readonly stoppedB64: string
  readonly opaqueRelaySessionIdB64: string
  readonly opaqueCiphertextB64: string
  readonly opaqueRouteB64: string
  readonly sessionRetiredB64: string
  readonly sessionRetiredRelaySessionIdB64: string
  readonly stoppedErrorB64: string
}

interface LaneVector extends VectorCase {
  readonly shareInstanceB64: string
  readonly protocolSessionIdB64: string
  readonly attachedLaneId: number
  readonly attachedLaneEpoch: number
  readonly grantOperationIdB64: string
  readonly attachNonceB64: string
  readonly receiverToSenderKeyB64: string
  readonly laneHelloB64: string
  readonly laneAckBodyB64: string
  readonly laneAckB64: string
  readonly laneRejectCode: number
  readonly laneRejectRetryAfterMilliseconds: number
  readonly laneRejectBodyB64: string
  readonly laneRejectB64: string
}

interface RelayEndpointCase {
  readonly relayBaseUrl: string
  readonly accepted: boolean
  readonly dialEndpoint?: string
  readonly relayIdentityEndpoint?: string
  readonly relayIdentityB64?: string
}

interface RelayEndpointVector extends VectorCase {
  readonly cases: readonly RelayEndpointCase[]
}

interface TranscriptVector extends VectorCase {
  readonly sessionAuthKeyB64: string
}

const identity = loadVectorFile(
  new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
).cases[0] as IdentityVector
const senderObjects = loadVectorFile(
  new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
).cases as SenderObjectVector[]
const sessionCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases
const semanticCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-semantics.json', import.meta.url),
).cases

function bytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}

function named<T extends VectorCase>(name: string): T {
  const result = sessionCases.find((candidate) => candidate.name === name)
  if (result === undefined) throw new Error(`missing vector ${name}`)
  return result as T
}

function mapBody(encoded: string): Map<number, unknown> {
  return mapBodyBytes(bytes(encoded))
}

function mapBodyBytes(encoded: Uint8Array): Map<number, unknown> {
  const decoded = decode(encoded, { useMaps: true })
  if (!(decoded instanceof Map)) throw new TypeError('sender object body must be a map')
  return decoded as Map<number, unknown>
}

function mapBytes(body: Map<number, unknown>, key: number, length?: number): Uint8Array {
  const value = body.get(key)
  if (!(value instanceof Uint8Array) || (length !== undefined && value.byteLength !== length)) {
    throw new TypeError(`CBOR key ${key} has an invalid byte value`)
  }
  return value
}

function mapNumber(body: Map<number, unknown>, key: number): number {
  const value = body.get(key)
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new TypeError(`CBOR key ${key} has an invalid numeric value`)
  }
  return value
}

async function verifyEd25519(
  preimage: Uint8Array,
  signature: Uint8Array,
  publicKey = bytes(identity.senderPublicKeyB64),
): Promise<boolean> {
  const key = await crypto.subtle.importKey('raw', publicKey, 'Ed25519', false, ['verify'])
  return crypto.subtle.verify('Ed25519', key, Uint8Array.from(signature), Uint8Array.from(preimage))
}

function flip(value: Uint8Array, offset = 0): Uint8Array<ArrayBuffer> {
  const result = value.slice()
  result[offset] = result[offset]! ^ 1
  return result
}

async function objectBindingAndKey(
  vector: SenderObjectVector,
): Promise<{ readonly binding: SenderObjectBinding; readonly key: Uint8Array }> {
  const body = mapBody(vector.canonicalCborB64)
  const readSecret = bytes(identity.readSecretB64)
  switch (vector.domain) {
    case 'windshare/v2 object/descriptor':
      return {
        binding: await createDescriptorObjectBinding(
          bytes(identity.pkHashB64),
          bytes(identity.shareIdRawB64),
        ),
        key: await deriveSuite02DescriptorKey(readSecret, bytes(identity.pkHashB64)),
      }
    case 'windshare/v2 object/catalog-page': {
      const shareInstance = mapBytes(body, 1, 16)
      return {
        binding: createCatalogPageObjectBinding(
          shareInstance,
          mapBytes(body, 2, 16),
          mapNumber(body, 4),
        ),
        key: await deriveSuite02CatalogKey(readSecret, shareInstance),
      }
    }
    case 'windshare/v2 object/directory-error': {
      const shareInstance = mapBytes(body, 1, 16)
      return {
        binding: createDirectoryErrorObjectBinding(shareInstance, mapBytes(body, 2, 16)),
        key: await deriveSuite02CatalogKey(readSecret, shareInstance),
      }
    }
    case 'windshare/v2 object/file-revision': {
      const shareInstance = mapBytes(body, 1, 16)
      const fileId = mapBytes(body, 2, 16)
      return {
        binding: createRevisionObjectBinding(shareInstance, fileId),
        key: await deriveSuite02FileObjectKey(readSecret, shareInstance, fileId),
      }
    }
    case 'windshare/v2 object/block-record': {
      const shareInstance = mapBytes(body, 1, 16)
      const fileId = mapBytes(body, 2, 16)
      const revision = mapBytes(body, 3, 16)
      const localBlock = BigInt(mapNumber(body, 4))
      const data = mapBytes(body, 5)
      const fileObjectKey = await deriveSuite02FileObjectKey(readSecret, shareInstance, fileId)
      const revisionKey = await deriveSuite02RevisionKey(fileObjectKey, revision)
      const descriptor = mapBody(senderObjects[0]!.canonicalCborB64)
      const segment = (localBlock * BigInt(mapNumber(descriptor, 5))) / (16n << 30n)
      return {
        binding: createBlockRecordObjectBinding(
          shareInstance,
          fileId,
          revision,
          localBlock,
          data.byteLength,
        ),
        key: await deriveSuite02FileSegmentKey(revisionKey, segment),
      }
    }
    case 'windshare/v2 object/offline-commit': {
      const shareInstance = mapBytes(body, 1, 16)
      return {
        binding: createOfflineCommitObjectBinding(shareInstance),
        key: await deriveSuite02DescriptorKey(readSecret, bytes(identity.pkHashB64)),
      }
    }
    default:
      throw new TypeError(`unknown sender object domain ${vector.domain}`)
  }
}

async function hostileObjectBindings(
  vector: SenderObjectVector,
): Promise<readonly SenderObjectBinding[]> {
  const body = mapBody(vector.canonicalCborB64)
  const shareInstance = vector.domain === 'windshare/v2 object/descriptor'
    ? undefined
    : mapBytes(body, 1, 16)
  switch (vector.domain) {
    case 'windshare/v2 object/descriptor': {
      const hostilePKHash = flip(bytes(identity.pkHashB64))
      const hostileRoute = await encodeSuite02CapabilityKey(
        bytes(identity.readSecretB64),
        hostilePKHash,
      )
      return [await createDescriptorObjectBinding(hostilePKHash, hostileRoute.shareIdRaw)]
    }
    case 'windshare/v2 object/catalog-page': {
      const directory = mapBytes(body, 2, 16)
      const page = mapNumber(body, 4)
      return [
        createCatalogPageObjectBinding(flip(shareInstance!), directory, page),
        createCatalogPageObjectBinding(shareInstance!, flip(directory), page),
        createCatalogPageObjectBinding(shareInstance!, directory, page + 1),
      ]
    }
    case 'windshare/v2 object/directory-error': {
      const directory = mapBytes(body, 2, 16)
      return [
        createDirectoryErrorObjectBinding(flip(shareInstance!), directory),
        createDirectoryErrorObjectBinding(shareInstance!, flip(directory)),
      ]
    }
    case 'windshare/v2 object/file-revision': {
      const fileId = mapBytes(body, 2, 16)
      return [
        createRevisionObjectBinding(flip(shareInstance!), fileId),
        createRevisionObjectBinding(shareInstance!, flip(fileId)),
      ]
    }
    case 'windshare/v2 object/block-record': {
      const fileId = mapBytes(body, 2, 16)
      const revision = mapBytes(body, 3, 16)
      const block = BigInt(mapNumber(body, 4))
      const dataLength = mapBytes(body, 5).byteLength
      return [
        createBlockRecordObjectBinding(flip(shareInstance!), fileId, revision, block, dataLength),
        createBlockRecordObjectBinding(shareInstance!, flip(fileId), revision, block, dataLength),
        createBlockRecordObjectBinding(shareInstance!, fileId, flip(revision), block, dataLength),
        createBlockRecordObjectBinding(shareInstance!, fileId, revision, block + 1n, dataLength),
        createBlockRecordObjectBinding(shareInstance!, fileId, revision, block, dataLength + 1),
      ]
    }
    case 'windshare/v2 object/offline-commit':
      return [createOfflineCommitObjectBinding(flip(shareInstance!))]
    default:
      throw new TypeError(`unknown sender object domain ${vector.domain}`)
  }
}

describe('suite-02 Web runtime contract', () => {
  it('derives capability identity and all six HKDF branches independently', async () => {
    const capability = await decodeSuite02CapabilityKey(identity.keyString)
    expect(capability.readSecret).toEqual(bytes(identity.readSecretB64))
    expect(capability.pkHash).toEqual(bytes(identity.pkHashB64))
    expect(capability.shareIdRaw).toEqual(bytes(identity.shareIdRawB64))
    expect(capability.shareId).toBe(identity.shareId)
    expect(await suite02SenderKeyHash(bytes(identity.senderPublicKeyB64))).toEqual(capability.pkHash)
    const encoded = await encodeSuite02CapabilityKey(capability.readSecret, capability.pkHash)
    expect(encoded.encoded).toBe(identity.keyString)
    const parsed = await parseSuite02CapabilityLink(
      `https://share.example/s/${identity.shareId}?r=https%3A%2F%2Frelay.example#${identity.keyString}`,
    )
    expect(parsed.shareId).toBe(identity.shareId)
    expect(parsed.relayHints).toEqual(['https://relay.example'])

    const fileObject = await deriveSuite02FileObjectKey(
      capability.readSecret,
      bytes(identity.shareInstanceB64),
      bytes(identity.fileIdB64),
    )
    const revision = await deriveSuite02RevisionKey(fileObject, bytes(identity.fileRevisionB64))
    expect(await deriveSuite02DescriptorKey(capability.readSecret, capability.pkHash)).toEqual(
      bytes(identity.descriptorKeyB64),
    )
    expect(
      await deriveSuite02CatalogKey(capability.readSecret, bytes(identity.shareInstanceB64)),
    ).toEqual(bytes(identity.catalogKeyB64))
    expect(fileObject).toEqual(bytes(identity.fileObjectKeyB64))
    expect(revision).toEqual(bytes(identity.revisionKeyB64))
    expect(await deriveSuite02FileSegmentKey(revision, BigInt(identity.segment))).toEqual(
      bytes(identity.fileSegmentKeyB64),
    )
    const transcript = named<TranscriptVector>('sender-authenticated-x25519-transcript')
    expect(
      await deriveSuite02SessionAuthKey(capability.readSecret, bytes(identity.shareInstanceB64)),
    ).toEqual(bytes(transcript.sessionAuthKeyB64))

    await expect(decodeSuite02CapabilityKey(`${identity.keyString}=`)).rejects.toThrow()
    await expect(
      parseSuite02CapabilityLink(
        `https://share.example/s/${flip(capability.shareIdRaw)[0]}#${identity.keyString}`,
      ),
    ).rejects.toThrow()
    await expect(deriveSuite02FileSegmentKey(revision, -1n)).rejects.toThrow()
    await expect(deriveSuite02DescriptorKey(new Uint8Array(16), new Uint8Array(15))).rejects.toThrow()
  })

  it('reconstructs and authenticates sender objects from semantic bindings', async () => {
    const senderPublicKey = bytes(identity.senderPublicKeyB64)
    for (const vector of senderObjects) {
      const { binding, key } = await objectBindingAndKey(vector)
      const object = bytes(vector.objectB64)
      const ciphertextLength = new DataView(object.buffer, object.byteOffset).getUint32(4, false)
      const prefix = object.slice(0, 20 + ciphertextLength)
      expect(binding.context).toEqual(bytes(vector.contextB64))
      expect(key).toEqual(bytes(vector.keyB64))
      expect(await senderObjectAuthenticationData(binding, object.subarray(0, 8))).toEqual(
        bytes(vector.aadB64),
      )
      expect(await senderObjectSignaturePreimage(binding, prefix)).toEqual(
        bytes(vector.signaturePreimageB64),
      )
      const plaintext = vector === senderObjects[0]
        ? await openDescriptorObjectBootstrap(binding, key, object, (opened) => {
          return mapBytes(mapBodyBytes(opened), 7, 32)
        })
        : await openSenderObject(binding, key, senderPublicKey, object)
      expect(plaintext).toEqual(bytes(vector.canonicalCborB64))

      await expect(openSenderObject(binding, key, senderPublicKey, flip(object, 20))).rejects.toThrow()
      await expect(openSenderObject(binding, key, senderPublicKey, flip(object, object.length - 1))).rejects.toThrow()
      for (const hostileBinding of await hostileObjectBindings(vector)) {
        await expect(
          openSenderObject(hostileBinding, key, senderPublicKey, object),
        ).rejects.toThrow()
      }
      await expect(openSenderObject(binding, flip(key), senderPublicKey, object)).rejects.toThrow()
    }
    await expect(
      createDescriptorObjectBinding(bytes(identity.pkHashB64), flip(bytes(identity.shareIdRawB64))),
    ).rejects.toThrow()
    expect(
      () => new SenderObjectBinding(Symbol('forged'), 'windshare/v2 object/descriptor', new Uint8Array(29), 16 << 10),
    ).toThrow(/typed factory/u)
  })

})

describe('suite-02 relay and lane runtime contract', () => {
  it('reconstructs purpose-bound relay controls and opaque routing', async () => {
    const vector = named<RegistrationVector>('fresh-relay-registration-proof')
    const endpointVector = named<RelayEndpointVector>('v2-relay-endpoint-normalization')
    for (const endpointCase of endpointVector.cases) {
      if (!endpointCase.accepted) {
        await expect(canonicalV2RelayEndpoint(endpointCase.relayBaseUrl)).rejects.toThrow()
        continue
      }
      const normalized = await canonicalV2RelayEndpoint(endpointCase.relayBaseUrl)
      expect(normalized.dialEndpoint).toBe(endpointCase.dialEndpoint)
      expect(normalized.relayIdentityEndpoint).toBe(endpointCase.relayIdentityEndpoint)
      expect(normalized.relayIdentity).toEqual(bytes(endpointCase.relayIdentityB64!))
    }
    const endpoint = await canonicalV2RelayEndpoint(vector.relayBaseUrl)
    expect(endpoint.dialEndpoint).toBe(vector.dialEndpoint)
    expect(endpoint.relayIdentityEndpoint).toBe(vector.relayIdentityEndpoint)
    expect(endpoint.relayIdentity).toEqual(bytes(vector.relayIdentityB64))
    expect(endpoint.relayIdentityEndpoint).not.toContain('?')

    const init: V2RegisterInit = {
      mode: V2_REGISTRATION_MODE.fresh,
      shareId: bytes(identity.shareIdRawB64),
      shareInstance: bytes(identity.shareInstanceB64),
      pkHash: bytes(identity.pkHashB64),
      descriptorDigest: bytes(vector.descriptorDigestB64),
      resumeTokenHash: bytes(vector.resumeTokenHashB64),
    }
    const challenge: V2Challenge = {
      purpose: V2_CHALLENGE_PURPOSE.register,
      id: bytes(vector.challengeIdB64),
      nonce: bytes(vector.challengeNonceB64),
      expiresAtUnixSeconds: BigInt(vector.expiresAt),
    }
    const registerInit = await encodeV2RegisterInit(init)
    expect(registerInit).toEqual(bytes(vector.registerInitB64))
    expect(await decodeV2RegisterInit(registerInit)).toEqual(init)
    expect(encodeV2Challenge(challenge)).toEqual(bytes(vector.registerChallengeB64))
    expect(decodeV2Challenge(bytes(vector.registerChallengeB64))).toEqual(challenge)
    const registerPreimage = await v2RegistrationProofPreimage(
      init,
      challenge,
      endpoint.relayIdentity,
    )
    expect(registerPreimage).toEqual(bytes(vector.preimageB64))
    expect(await verifyEd25519(registerPreimage, bytes(vector.signatureB64))).toBe(true)
    const registerProof = encodeV2RegisterProof({
      mode: init.mode,
      senderPublicKey: bytes(identity.senderPublicKeyB64),
      signature: bytes(vector.signatureB64),
    })
    expect(registerProof).toEqual(bytes(vector.registerProofB64))
    expect(decodeV2RegisterProof(registerProof).mode).toBe(V2_REGISTRATION_MODE.fresh)

    const resumeInit = { ...init, mode: V2_REGISTRATION_MODE.resume } as const
    const resumeChallenge: V2Challenge = {
      purpose: V2_CHALLENGE_PURPOSE.resume,
      id: bytes(vector.resumeChallengeIdB64),
      nonce: bytes(vector.resumeChallengeNonceB64),
      expiresAtUnixSeconds: BigInt(vector.resumeExpiresAt),
    }
    expect(await encodeV2RegisterInit(resumeInit)).toEqual(bytes(vector.resumeInitB64))
    expect(encodeV2Challenge(resumeChallenge)).toEqual(bytes(vector.resumeChallengeB64))
    const resumePreimage = await v2RegistrationProofPreimage(
      resumeInit,
      resumeChallenge,
      endpoint.relayIdentity,
    )
    expect(resumePreimage).toEqual(bytes(vector.resumePreimageB64))
    expect(await verifyEd25519(resumePreimage, bytes(vector.resumeSignatureB64))).toBe(true)
    expect(
      encodeV2RegisterProof({
        mode: V2_REGISTRATION_MODE.resume,
        senderPublicKey: bytes(identity.senderPublicKeyB64),
        signature: bytes(vector.resumeSignatureB64),
      }),
    ).toEqual(bytes(vector.resumeProofB64))
    expect(encodeV2ResumeCredential(bytes(vector.resumeTokenB64))).toEqual(
      bytes(vector.resumeCredentialB64),
    )
    expect(decodeV2ResumeCredential(bytes(vector.resumeCredentialB64))).toEqual(
      bytes(vector.resumeTokenB64),
    )

    const stopInit: V2StopInit = {
      shareId: init.shareId,
      shareInstance: init.shareInstance,
      pkHash: init.pkHash,
      relayIdentity: endpoint.relayIdentity,
      stopId: bytes(vector.stopIdB64),
    }
    const stopChallenge: V2Challenge = {
      purpose: V2_CHALLENGE_PURPOSE.stop,
      id: bytes(vector.stopChallengeIdB64),
      nonce: bytes(vector.stopChallengeNonceB64),
      expiresAtUnixSeconds: BigInt(vector.stopExpiresAt),
    }
    expect(await encodeV2StopInit(stopInit)).toEqual(bytes(vector.stopInitB64))
    expect(await decodeV2StopInit(bytes(vector.stopInitB64))).toEqual(stopInit)
    expect(encodeV2Challenge(stopChallenge)).toEqual(bytes(vector.stopChallengeB64))
    const stopPreimage = await v2StopProofPreimage(stopInit, stopChallenge)
    expect(stopPreimage).toEqual(bytes(vector.stopPreimageB64))
    expect(await verifyEd25519(stopPreimage, bytes(vector.stopSignatureB64))).toBe(true)
    expect(
      encodeV2StopProof({
        senderPublicKey: bytes(identity.senderPublicKeyB64),
        signature: bytes(vector.stopSignatureB64),
      }),
    ).toEqual(bytes(vector.stopProofB64))
    expect(decodeV2StopProof(bytes(vector.stopProofB64)).signature).toEqual(
      bytes(vector.stopSignatureB64),
    )

    const descriptor = bytes(senderObjects[0]!.objectB64)
    expect(encodeV2DescriptorUpload(descriptor)).toEqual(bytes(vector.descriptorUploadB64))
    expect(decodeV2DescriptorUpload(bytes(vector.descriptorUploadB64))).toEqual(descriptor)
    expect(
      encodeV2DescriptorDelivery({
        relaySessionId: bytes(vector.relaySessionIdB64),
        object: descriptor,
      }),
    ).toEqual(bytes(vector.descriptorDeliveryB64))
    expect(decodeV2DescriptorDelivery(bytes(vector.descriptorDeliveryB64)).object).toEqual(
      descriptor,
    )
    expect(encodeV2Join(init.shareId)).toEqual(bytes(vector.joinB64))
    expect(decodeV2Join(bytes(vector.joinB64))).toEqual(init.shareId)
    expect(
      encodeV2Registered({
        shareId: init.shareId,
        shareInstance: init.shareInstance,
        descriptorDigest: init.descriptorDigest,
      }),
    ).toEqual(bytes(vector.registeredB64))
    expect(decodeV2Registered(bytes(vector.registeredB64)).shareInstance).toEqual(
      init.shareInstance,
    )
    expect(encodeV2Stopped(stopInit.stopId)).toEqual(bytes(vector.stoppedB64))
    expect(decodeV2Stopped(bytes(vector.stoppedB64))).toEqual(stopInit.stopId)
    const opaque = {
      relaySessionId: bytes(vector.opaqueRelaySessionIdB64),
      ciphertext: bytes(vector.opaqueCiphertextB64),
    }
    expect(encodeV2OpaqueRoute(opaque)).toEqual(bytes(vector.opaqueRouteB64))
    expect(decodeV2OpaqueRoute(bytes(vector.opaqueRouteB64))).toEqual(opaque)
    const sessionRetired = {
      relaySessionId: bytes(vector.sessionRetiredRelaySessionIdB64),
    }
    expect(encodeV2SessionRetired(sessionRetired)).toEqual(bytes(vector.sessionRetiredB64))
    expect(decodeV2SessionRetired(bytes(vector.sessionRetiredB64))).toEqual(sessionRetired)
    expect(encodeV2RelayError({ code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 0 })).toEqual(
      bytes(vector.stoppedErrorB64),
    )
    expect(decodeV2RelayError(bytes(vector.stoppedErrorB64)).code).toBe(V2_RELAY_ERROR.stopped)
    expect(semanticCases.find((candidate) => candidate.name === 'relay-route-lifecycle')).toEqual({
      name: 'relay-route-lifecycle',
      crashGraceSeconds: '60',
      sessionTombstoneSeconds: '60',
      routeBudgetCounts: ['starting', 'live', 'crash-grace', 'stopped-tombstone'],
      sessionBudgetCounts: ['active', 'ended-id-tombstone'],
      sessionBudgetScopes: ['global', 'per-share'],
      stopStoreOutcomes: ['committed', 'definitely-not-committed', 'unknown'],
      explicitStop: [
        'per-route-storage-transaction',
        'durable-tombstone-before-ack',
        'exact-participant-cleanup-before-ack',
        'unknown-durability-fail-closed',
        'same-stop-id-resolution',
        'no-crash-grace',
        'permanent-reject-same-instance',
      ],
      unexpectedDisconnect: [
        'drop-sessions',
        'enter-bounded-crash-grace',
        'immediate-retirement-during-stop-commit',
      ],
      stoppedTombstoneRetention: 'until-future-authenticated-refresh',
    })

    const registrationAxes: readonly {
      readonly candidateInit: V2RegisterInit
      readonly candidateChallenge: V2Challenge
      readonly candidateRelayIdentity: Uint8Array
    }[] = [
      { candidateInit: { ...init, shareInstance: flip(init.shareInstance) }, candidateChallenge: challenge, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: { ...init, descriptorDigest: flip(init.descriptorDigest) }, candidateChallenge: challenge, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: { ...init, resumeTokenHash: flip(init.resumeTokenHash) }, candidateChallenge: challenge, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: init, candidateChallenge: { ...challenge, id: flip(challenge.id) }, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: init, candidateChallenge: { ...challenge, nonce: flip(challenge.nonce) }, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: init, candidateChallenge: { ...challenge, expiresAtUnixSeconds: challenge.expiresAtUnixSeconds + 1n }, candidateRelayIdentity: endpoint.relayIdentity },
      { candidateInit: init, candidateChallenge: challenge, candidateRelayIdentity: flip(endpoint.relayIdentity) },
    ]
    for (const axis of registrationAxes) {
      const candidate = await v2RegistrationProofPreimage(
        axis.candidateInit,
        axis.candidateChallenge,
        axis.candidateRelayIdentity,
      )
      expect(await verifyEd25519(candidate, bytes(vector.signatureB64))).toBe(false)
    }
    const mutatedPKHash = flip(init.pkHash)
    const mutatedRoute = await encodeSuite02CapabilityKey(
      bytes(identity.readSecretB64),
      mutatedPKHash,
    )
    const mutatedKeyPreimage = await v2RegistrationProofPreimage(
      { ...init, pkHash: mutatedPKHash, shareId: mutatedRoute.shareIdRaw },
      challenge,
      endpoint.relayIdentity,
    )
    expect(await verifyEd25519(mutatedKeyPreimage, bytes(vector.signatureB64))).toBe(false)
    await expect(
      v2RegistrationProofPreimage(
        { ...init, shareId: flip(init.shareId) },
        challenge,
        endpoint.relayIdentity,
      ),
    ).rejects.toThrow()

    const stopAxes: readonly { readonly init: V2StopInit; readonly challenge: V2Challenge }[] = [
      { init: { ...stopInit, shareInstance: flip(stopInit.shareInstance) }, challenge: stopChallenge },
      { init: { ...stopInit, relayIdentity: flip(stopInit.relayIdentity) }, challenge: stopChallenge },
      { init: { ...stopInit, stopId: flip(stopInit.stopId) }, challenge: stopChallenge },
      { init: stopInit, challenge: { ...stopChallenge, id: flip(stopChallenge.id) } },
      { init: stopInit, challenge: { ...stopChallenge, nonce: flip(stopChallenge.nonce) } },
      { init: stopInit, challenge: { ...stopChallenge, expiresAtUnixSeconds: stopChallenge.expiresAtUnixSeconds + 1n } },
    ]
    for (const axis of stopAxes) {
      const candidate = await v2StopProofPreimage(axis.init, axis.challenge)
      expect(await verifyEd25519(candidate, bytes(vector.stopSignatureB64))).toBe(false)
    }
    const mutatedStopKeyPreimage = await v2StopProofPreimage(
      { ...stopInit, pkHash: mutatedPKHash, shareId: mutatedRoute.shareIdRaw },
      stopChallenge,
    )
    expect(await verifyEd25519(mutatedStopKeyPreimage, bytes(vector.stopSignatureB64))).toBe(false)
    await expect(
      v2StopProofPreimage({ ...stopInit, shareId: flip(stopInit.shareId) }, stopChallenge),
    ).rejects.toThrow()

    await expect(
      v2RegistrationProofPreimage(init, { ...challenge, purpose: V2_CHALLENGE_PURPOSE.resume }, endpoint.relayIdentity),
    ).rejects.toThrow()
    const replayedChallenge = { ...challenge, id: flip(challenge.id) }
    const replayPreimage = await v2RegistrationProofPreimage(
      init,
      replayedChallenge,
      endpoint.relayIdentity,
    )
    expect(await verifyEd25519(replayPreimage, bytes(vector.signatureB64))).toBe(false)
    await expect(v2StopProofPreimage(stopInit, challenge)).rejects.toThrow()
    expect(() => decodeV2OpaqueRoute(flip(bytes(vector.opaqueRouteB64), 5))).toThrow()
    expect(() => decodeV2OpaqueRoute(bytes(vector.sessionRetiredB64))).toThrow()
    expect(() => decodeV2SessionRetired(flip(bytes(vector.sessionRetiredB64), 5))).toThrow()
    expect(() => decodeV2SessionRetired(bytes(vector.sessionRetiredB64).subarray(0, 15))).toThrow()
    expect(() => decodeV2SessionRetired(new Uint8Array(16))).toThrow()
    expect(() => encodeV2SessionRetired({ relaySessionId: new Uint8Array(8) })).toThrow()
    expect(() => encodeV2OpaqueRoute({ ...opaque, ciphertext: new Uint8Array(65_537) })).toThrow()
    expect(() => encodeV2OpaqueRoute({ ...opaque, ciphertext: new Uint8Array(0) })).toThrow()
    expect(() => encodeV2OpaqueRoute({ ...opaque, relaySessionId: new Uint8Array(8) })).toThrow()
    expect(
      decodeV2OpaqueRoute(
        encodeV2OpaqueRoute({ ...opaque, ciphertext: new Uint8Array(65_536).fill(1) }),
      ).ciphertext,
    ).toHaveLength(65_536)
    expect(() => encodeV2RelayError({ code: V2_RELAY_ERROR.stopped, retryAfterMilliseconds: 1 })).toThrow()
  })

  it('binds lane accept/reject to every hello axis and consumes one response', async () => {
    const vector = named<LaneVector>('sender-granted-lane-attach')
    const fields = {
      shareInstance: bytes(vector.shareInstanceB64),
      protocolSessionId: bytes(vector.protocolSessionIdB64),
      laneId: vector.attachedLaneId,
      laneEpoch: vector.attachedLaneEpoch,
      grantOperationId: bytes(vector.grantOperationIdB64),
      attachNonce: bytes(vector.attachNonceB64),
    }
    const trafficKey = bytes(vector.receiverToSenderKeyB64)
    const hello = await encodeV2LaneHello(fields, trafficKey)
    expect(hello).toEqual(bytes(vector.laneHelloB64))
    expect(await decodeV2LaneHello(hello, trafficKey)).toEqual(fields)
    expect(await v2LaneAcceptBody(hello, bytes(vector.laneAckBodyB64).subarray(37))).toEqual(
      bytes(vector.laneAckBodyB64),
    )
    const senderPublicKey = bytes(identity.senderPublicKeyB64)
    expect(await verifyV2LaneAccept(bytes(vector.laneAckB64), hello, senderPublicKey)).toEqual(
      bytes(vector.laneAckBodyB64).subarray(37),
    )
    const rejection = {
      code: vector.laneRejectCode as typeof V2_LANE_REJECT.admissionLimited,
      retryAfterMilliseconds: vector.laneRejectRetryAfterMilliseconds,
    }
    expect(await v2LaneRejectBody(hello, rejection)).toEqual(bytes(vector.laneRejectBodyB64))
    expect(await verifyV2LaneReject(bytes(vector.laneRejectB64), hello, senderPublicKey)).toEqual(
      rejection,
    )

    const axes = [5, 21, 37, 41, 45, 61]
    for (const offset of axes) {
      const hostile = flip(hello, offset)
      await expect(decodeV2LaneHello(hostile, trafficKey)).rejects.toThrow()
      await expect(verifyV2LaneAccept(bytes(vector.laneAckB64), hostile, senderPublicKey)).rejects.toThrow()
      await expect(verifyV2LaneReject(bytes(vector.laneRejectB64), hostile, senderPublicKey)).rejects.toThrow()
    }
    await expect(decodeV2LaneHello(hello, flip(trafficKey))).rejects.toThrow()
    await expect(verifyV2LaneAccept(flip(bytes(vector.laneAckB64), 53), hello, senderPublicKey)).rejects.toThrow()
    await expect(verifyV2LaneReject(flip(bytes(vector.laneRejectB64), 44), hello, senderPublicKey)).rejects.toThrow()
    await expect(encodeV2LaneHello({ ...fields, laneId: 0 }, trafficKey)).rejects.toThrow()
    await expect(
      v2LaneRejectBody(hello, { code: V2_LANE_REJECT.stopping, retryAfterMilliseconds: 1 }),
    ).rejects.toThrow()

    const authority = new V2LaneResponseAuthority(hello, senderPublicKey)
    await expect(authority.accept(bytes(vector.laneAckB64))).resolves.toEqual(
      bytes(vector.laneAckBodyB64).subarray(37),
    )
    await expect(authority.accept(bytes(vector.laneAckB64))).rejects.toThrow(/consumed/u)
  })
})
