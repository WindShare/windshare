import {
  diffieHellman,
  hkdfSync,
  verify,
} from 'node:crypto'

import { decode, encode, rfc8949EncodeOptions } from 'cborg'
import { describe, expect, it } from 'vitest'

import { loadVectorFile, type VectorCase } from '../vectors'
import {
  bytes,
  canonicalV2RelayEndpoints,
  concat,
  derive,
  ed25519Public,
  expectCanonicalCBOR,
  expectSenderControl,
  expectSenderObject,
  fragmentDuplicateConflicts,
  hmacSha256,
  identity,
  namedCase,
  offlineMerkleRoot,
  senderObjects,
  sha256,
  u32,
  u64,
  utf8,
  validSingleFragment,
  x25519Private,
  x25519Public,
} from './r0-contract-support'

const AES_GCM_TAG_BYTES = 16
const OPERATION_HEADER_BYTES = 16
const OPERATION_NONCE_BYTES = 12
const FRAGMENT_HEADER_BYTES = 52

interface TranscriptVector extends VectorCase {
  readonly receiverPrivateB64: string
  readonly receiverPublicB64: string
  readonly senderPrivateB64: string
  readonly senderPublicB64: string
  readonly sessionAuthKeyB64: string
  readonly clientBodyB64: string
  readonly clientProofB64: string
  readonly clientHelloB64: string
  readonly serverBodyB64: string
  readonly serverSignatureB64: string
  readonly serverHelloB64: string
  readonly transcriptHashB64: string
  readonly protocolSessionIdB64: string
  readonly sharedSecretB64: string
  readonly handshakeSecretB64: string
  readonly receiverToSenderKeyB64: string
  readonly senderToReceiverKeyB64: string
  readonly initialLaneId: number
  readonly initialLaneEpoch: number
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
  readonly stoppedErrorB64: string
}

interface RelayEndpointCase {
  readonly name: string
  readonly relayBaseUrl: string
  readonly accepted: boolean
  readonly dialEndpoint?: string
  readonly relayIdentityEndpoint?: string
  readonly relayIdentityB64?: string
}

interface RelayEndpointVector extends VectorCase {
  readonly cases: readonly RelayEndpointCase[]
}

interface TerminalVector extends VectorCase {
  readonly shareInstanceB64: string
  readonly protocolSessionIdB64: string
  readonly laneId: number
  readonly laneEpoch: number
  readonly sequence: string
  readonly trafficKeyB64: string
  readonly semanticBodyCborB64: string
  readonly unsignedControlCborB64: string
  readonly signedControlCborB64: string
  readonly controlPreimageB64: string
  readonly controlSignatureB64: string
  readonly plaintextB64: string
  readonly nonceB64: string
  readonly aadB64: string
  readonly envelopeB64: string
}

interface OperationErrorVector extends TerminalVector {
  readonly operationIdB64: string
}

interface OpenResultsVector extends OperationErrorVector {
  readonly fileIdB64: string
  readonly revisionObjectB64: string
  readonly leaseIdB64: string
  readonly leaseTtlMilliseconds: string
  readonly renewAfterMilliseconds: string
}

interface LeaseResultVector extends OperationErrorVector {
  readonly leaseIdB64: string
  readonly leaseTtlMilliseconds: string
  readonly renewAfterMilliseconds: string
}

interface LaneAttachVector extends VectorCase {
  readonly shareInstanceB64: string
  readonly protocolSessionIdB64: string
  readonly grantLaneId: number
  readonly grantLaneEpoch: number
  readonly grantSequence: string
  readonly attachedLaneId: number
  readonly attachedLaneEpoch: number
  readonly grantOperationIdB64: string
  readonly attachNonceB64: string
  readonly receiverToSenderKeyB64: string
  readonly senderToReceiverKeyB64: string
  readonly grantSemanticBodyCborB64: string
  readonly unsignedGrantCborB64: string
  readonly grantSignedBodyB64: string
  readonly grantPreimageB64: string
  readonly grantSignatureB64: string
  readonly grantPlaintextB64: string
  readonly grantAadB64: string
  readonly grantEnvelopeB64: string
  readonly laneHelloBaseB64: string
  readonly laneHelloProofB64: string
  readonly laneHelloB64: string
  readonly laneAckBodyB64: string
  readonly laneAckSignatureB64: string
  readonly laneAckB64: string
  readonly laneRejectCode: number
  readonly laneRejectRetryAfterMilliseconds: number
  readonly laneRejectBodyB64: string
  readonly laneRejectSignatureB64: string
  readonly laneRejectB64: string
}

interface FragmentVector extends VectorCase {
  readonly operationIdB64: string
  readonly recordIdB64: string
  readonly fragmentIndex: number
  readonly fragmentCount: number
  readonly totalLength: string
  readonly payloadLength: string
  readonly fragmentPlaintextB64: string
  readonly maxFrameBytes: number
  readonly maxOperationPlaintextBytes: number
  readonly maxFragmentPayloadBytes: number
  readonly maxBlockRecordBytes: number
  readonly fragmentTimeoutSeconds: number
  readonly tombstoneSeconds: number
  readonly mutationsRejected: readonly string[]
}

interface MaximumFragmentVector extends VectorCase {
  readonly operationIdB64: string
  readonly recordPattern: string
  readonly recordLength: string
  readonly recordDigestB64: string
  readonly recordIdB64: string
  readonly fragmentCount: number
  readonly fragmentDigestsB64: readonly string[]
  readonly firstFragmentB64: string
  readonly lastFragmentB64: string
  readonly firstPayloadLength: string
  readonly lastPayloadLength: string
  readonly maxFrameBytes: number
  readonly maxOperationPlaintextBytes: number
  readonly maxFragmentPayloadBytes: number
  readonly maxBlockRecordBytes: number
}

interface SemanticsVector extends VectorCase {
  readonly values?: Readonly<Record<string, string>>
  readonly cases?: readonly SelectionCase[] | readonly CheckpointCut[]
  readonly cuts?: readonly CheckpointCut[]
  readonly order?: readonly string[]
  readonly publishOnlyAfter?: readonly string[]
  readonly preCommitCrashVisible?: boolean
  readonly states?: readonly string[]
  readonly explicitStopUsesCrashGrace?: boolean
}

interface SelectionCase {
  readonly files: string
  readonly bytes: string
  readonly terminal: boolean
  readonly failed: boolean
  readonly class: string
}

interface CheckpointCut {
  readonly cut: string
  readonly published: boolean
}

const sessionCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases
const fragmentCases = loadVectorFile(
  new URL('../../../core/testvectors/v2-fragment.json', import.meta.url),
).cases
const fragment = namedCase<FragmentVector>(fragmentCases, 'single-fragment-block-record')
const maximumFragment = namedCase<MaximumFragmentVector>(
  fragmentCases,
  'maximum-block-record-fragmentation',
)
const semantics = loadVectorFile(
  new URL('../../../core/testvectors/v2-semantics.json', import.meta.url),
).cases as SemanticsVector[]

function classifySelection(value: SelectionCase): string {
  if (BigInt(value.files) >= 30n || BigInt(value.bytes) >= 8n << 20n) {
    return 'large'
  }
  if (!value.terminal || value.failed) {
    return 'unknown'
  }
  return 'small'
}

describe('R0 Go to TypeScript protocol contract', () => {
  it('derives suite-02 identities and every key from the capability', () => {
    const readSecret = bytes(identity.readSecretB64)
    const senderPublic = bytes(identity.senderPublicKeyB64)
    const shareInstance = bytes(identity.shareInstanceB64)
    const pkHash = sha256(concat(utf8('windshare/v2 sender-key\0'), senderPublic)).subarray(0, 16)
    const shareIdRaw = sha256(
      concat(utf8('windshare/v2 share-id\0'), pkHash),
    ).subarray(0, 12)
    const fileObjectKey = derive(
      readSecret,
      'windshare/v2 file-object',
      concat(shareInstance, bytes(identity.fileIdB64)),
    )
    const revisionKey = derive(
      fileObjectKey,
      'windshare/v2 file-revision',
      bytes(identity.fileRevisionB64),
    )

    expect(identity.suite).toBe(2)
    expect(readSecret).toHaveLength(16)
    expect(pkHash).toEqual(bytes(identity.pkHashB64))
    expect(shareIdRaw).toEqual(bytes(identity.shareIdRawB64))
    expect(
      sha256(concat(utf8('windshare/v2 share-id\0'), pkHash)).subarray(0, 12),
    ).toEqual(shareIdRaw)
    expect(Buffer.from(shareIdRaw).toString('base64url')).toBe(identity.shareId)
    expect(
      Buffer.from(concat(Uint8Array.of(identity.suite), readSecret, pkHash)).toString('base64url'),
    ).toBe(identity.keyString)
    expect(identity.keyString).toHaveLength(44)
    expect(derive(readSecret, 'windshare/v2 descriptor', pkHash)).toEqual(
      bytes(identity.descriptorKeyB64),
    )
    expect(derive(readSecret, 'windshare/v2 catalog', shareInstance)).toEqual(
      bytes(identity.catalogKeyB64),
    )
    expect(fileObjectKey).toEqual(bytes(identity.fileObjectKeyB64))
    expect(revisionKey).toEqual(bytes(identity.revisionKeyB64))
    expect(derive(revisionKey, 'windshare/v2 file-segment', u64(BigInt(identity.segment)))).toEqual(
      bytes(identity.fileSegmentKeyB64),
    )
  })

  it('authenticates, opens, and canonically re-encodes every sender object', () => {
    const senderPublicKey = ed25519Public(bytes(identity.senderPublicKeyB64))
    expect(senderObjects.map((vector) => vector.domain)).toEqual([
      'windshare/v2 object/descriptor',
      'windshare/v2 object/catalog-page',
      'windshare/v2 object/directory-error',
      'windshare/v2 object/file-revision',
      'windshare/v2 object/block-record',
      'windshare/v2 object/offline-commit',
    ])
    for (const vector of senderObjects) {
      expectSenderObject(vector, senderPublicKey)
    }
    const canonicalDescriptor = bytes(senderObjects[0]!.canonicalCborB64)
    expect((decode(canonicalDescriptor, { useMaps: true }) as Map<number, unknown>).get(6)).toBe(7)
    const nonMinimalFirstKey = concat(
      canonicalDescriptor.subarray(0, 1),
      Uint8Array.of(0x18, 0x00),
      canonicalDescriptor.subarray(2),
    )
    expect(() => expectCanonicalCBOR(nonMinimalFirstKey)).toThrow()

    const inventoryObjects = senderObjects.slice(0, -1)
    const offlineCommit = decode(bytes(senderObjects.at(-1)!.canonicalCborB64), {
      useMaps: true,
    }) as Map<number, unknown>
    const inventoryDigests = inventoryObjects.map((value) => sha256(bytes(value.objectB64)))
    expect(offlineCommit.get(2)).toEqual(offlineMerkleRoot(inventoryDigests))
    expect(offlineCommit.get(5)).toBe(inventoryObjects.length)
    expect(offlineCommit.get(6)).toBe(
      inventoryObjects.reduce((total, value) => total + bytes(value.objectB64).byteLength, 0),
    )
  })

})

describe('R0 relay and ProtocolSession contract', () => {
  it('binds relay registration, X25519 traffic keys, and terminal envelopes', () => {
    const relayEndpoints = namedCase<RelayEndpointVector>(
      sessionCases,
      'v2-relay-endpoint-normalization',
    )
    const transcript = namedCase<TranscriptVector>(
      sessionCases,
      'sender-authenticated-x25519-transcript',
    )
    const registration = namedCase<RegistrationVector>(
      sessionCases,
      'fresh-relay-registration-proof',
    )
    const operationError = namedCase<OperationErrorVector>(
      sessionCases,
      'sender-signed-operation-error',
    )
    const laneAttach = namedCase<LaneAttachVector>(sessionCases, 'sender-granted-lane-attach')
    const openResults = namedCase<OpenResultsVector>(
      sessionCases,
      'relative-lease-open-results',
    )
    const leaseResult = namedCase<LeaseResultVector>(sessionCases, 'renew-lease-result')
    const terminal = namedCase<TerminalVector>(sessionCases, 'sender-signed-session-terminal')
    const senderPublicKey = ed25519Public(bytes(identity.senderPublicKeyB64))
    const clientProof = hmacSha256(
      bytes(transcript.sessionAuthKeyB64),
      concat(utf8('windshare/v2 client-hello\0'), sha256(bytes(transcript.clientBodyB64))),
    )
    const serverPreimage = concat(
      utf8('windshare/v2 server-hello\0'),
      sha256(bytes(transcript.serverBodyB64)),
    )
    const transcriptHash = sha256(
      concat(bytes(transcript.clientHelloB64), bytes(transcript.serverHelloB64)),
    )
    const protocolSessionId = sha256(
      concat(utf8('windshare/v2 protocol-session\0'), transcriptHash),
    ).subarray(0, 16)
    const receiverPrivate = x25519Private(bytes(transcript.receiverPrivateB64))
    const shared = Uint8Array.from(
      diffieHellman({
        privateKey: receiverPrivate,
        publicKey: x25519Public(bytes(transcript.senderPublicB64)),
      }),
    )
    const handshakeInfo = concat(
      utf8('windshare/v2 handshake'),
      Uint8Array.of(0),
      transcriptHash,
    )
    const handshake = Uint8Array.from(
      Buffer.from(
        hkdfSync('sha256', shared, bytes(transcript.sessionAuthKeyB64), handshakeInfo, 32),
      ),
    )

    expect(clientProof).toEqual(bytes(transcript.clientProofB64))
    expect(concat(bytes(transcript.clientBodyB64), clientProof)).toEqual(
      bytes(transcript.clientHelloB64),
    )
    expect(
      verify(null, serverPreimage, senderPublicKey, bytes(transcript.serverSignatureB64)),
    ).toBe(true)
    expect(transcriptHash).toEqual(bytes(transcript.transcriptHashB64))
    expect(protocolSessionId).toEqual(bytes(transcript.protocolSessionIdB64))
    expect(shared).toEqual(bytes(transcript.sharedSecretB64))
    expect(handshake).toEqual(bytes(transcript.handshakeSecretB64))
    expect(
      derive(handshake, 'windshare/v2 traffic/receiver-to-sender', transcriptHash),
    ).toEqual(bytes(transcript.receiverToSenderKeyB64))
    expect(derive(handshake, 'windshare/v2 traffic/sender-to-receiver', transcriptHash)).toEqual(
      bytes(transcript.senderToReceiverKeyB64),
    )

    for (const endpointCase of relayEndpoints.cases) {
      if (!endpointCase.accepted) {
        expect(() => canonicalV2RelayEndpoints(endpointCase.relayBaseUrl)).toThrow(TypeError)
        continue
      }
      const normalized = canonicalV2RelayEndpoints(endpointCase.relayBaseUrl)
      expect(normalized.dialEndpoint).toBe(endpointCase.dialEndpoint)
      expect(normalized.relayIdentityEndpoint).toBe(endpointCase.relayIdentityEndpoint)
      expect(
        sha256(
          concat(
            utf8('windshare/v2 relay-identity\0'),
            utf8(normalized.relayIdentityEndpoint),
          ),
        ),
      ).toEqual(bytes(endpointCase.relayIdentityB64!))
    }

    const normalizedRegistrationEndpoint = canonicalV2RelayEndpoints(registration.relayBaseUrl)
    expect(normalizedRegistrationEndpoint).toEqual({
      dialEndpoint: registration.dialEndpoint,
      relayIdentityEndpoint: registration.relayIdentityEndpoint,
    })
    const descriptorDigest = sha256(bytes(senderObjects[0]!.objectB64))
    const relayIdentity = sha256(
      concat(
        utf8('windshare/v2 relay-identity\0'),
        utf8(normalizedRegistrationEndpoint.relayIdentityEndpoint),
      ),
    )
    const registerPreimage = concat(
      utf8('windshare/v2 relay-register\0'),
      bytes(identity.shareIdRawB64),
      bytes(identity.shareInstanceB64),
      bytes(identity.pkHashB64),
      descriptorDigest,
      sha256(bytes(registration.resumeTokenB64)),
      relayIdentity,
      bytes(registration.challengeIdB64),
      bytes(registration.challengeNonceB64),
      u64(BigInt(registration.expiresAt)),
    )
    expect(descriptorDigest).toEqual(bytes(registration.descriptorDigestB64))
    expect(relayIdentity).toEqual(bytes(registration.relayIdentityB64))
    expect(sha256(bytes(registration.resumeTokenB64))).toEqual(
      bytes(registration.resumeTokenHashB64),
    )
    expect(registerPreimage).toEqual(bytes(registration.preimageB64))
    expect(verify(null, registerPreimage, senderPublicKey, bytes(registration.signatureB64))).toBe(
      true,
    )
    expect(
      concat(
        utf8('WS2R'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(identity.shareIdRawB64),
        bytes(identity.shareInstanceB64),
        bytes(identity.pkHashB64),
        descriptorDigest,
        bytes(registration.resumeTokenHashB64),
      ),
    ).toEqual(bytes(registration.registerInitB64))
    expect(
      concat(
        utf8('WS2Q'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(registration.challengeIdB64),
        bytes(registration.challengeNonceB64),
        u64(BigInt(registration.expiresAt)),
      ),
    ).toEqual(bytes(registration.registerChallengeB64))
    expect(
      concat(
        utf8('WS2P'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(identity.senderPublicKeyB64),
        bytes(registration.signatureB64),
      ),
    ).toEqual(bytes(registration.registerProofB64))
    const descriptorObject = bytes(senderObjects[0]!.objectB64)
    expect(
      concat(
        utf8('WS2U'),
        Uint8Array.of(2, 0, 0, 0),
        u32(descriptorObject.byteLength),
        descriptorObject,
      ),
    ).toEqual(bytes(registration.descriptorUploadB64))
    expect(
      concat(
        utf8('WS2K'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(identity.shareIdRawB64),
        bytes(identity.shareInstanceB64),
        descriptorDigest,
      ),
    ).toEqual(bytes(registration.registeredB64))
    expect(
      concat(utf8('WS2J'), Uint8Array.of(2, 0, 0, 0), bytes(identity.shareIdRawB64)),
    ).toEqual(bytes(registration.joinB64))
    expect(
      concat(
        utf8('WS2D'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(registration.relaySessionIdB64),
        u32(descriptorObject.byteLength),
        descriptorObject,
      ),
    ).toEqual(bytes(registration.descriptorDeliveryB64))

    const resumePreimage = concat(
      utf8('windshare/v2 relay-resume\0'),
      bytes(identity.shareIdRawB64),
      bytes(identity.shareInstanceB64),
      bytes(identity.pkHashB64),
      descriptorDigest,
      bytes(registration.resumeTokenHashB64),
      relayIdentity,
      bytes(registration.resumeChallengeIdB64),
      bytes(registration.resumeChallengeNonceB64),
      u64(BigInt(registration.resumeExpiresAt)),
    )
    expect(resumePreimage).toEqual(bytes(registration.resumePreimageB64))
    expect(
      verify(null, resumePreimage, senderPublicKey, bytes(registration.resumeSignatureB64)),
    ).toBe(true)
    expect(
      concat(
        utf8('WS2T'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(registration.resumeTokenB64),
      ),
    ).toEqual(bytes(registration.resumeCredentialB64))
    const stopPreimage = concat(
      utf8('windshare/v2 relay-stop\0'),
      bytes(identity.shareIdRawB64),
      bytes(identity.shareInstanceB64),
      bytes(identity.pkHashB64),
      relayIdentity,
      bytes(registration.stopIdB64),
      bytes(registration.stopChallengeIdB64),
      bytes(registration.stopChallengeNonceB64),
      u64(BigInt(registration.stopExpiresAt)),
    )
    expect(stopPreimage).toEqual(bytes(registration.stopPreimageB64))
    expect(verify(null, stopPreimage, senderPublicKey, bytes(registration.stopSignatureB64))).toBe(
      true,
    )
    expect(
      concat(
        utf8('WS2X'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(identity.shareIdRawB64),
        bytes(identity.shareInstanceB64),
        bytes(identity.pkHashB64),
        relayIdentity,
        bytes(registration.stopIdB64),
      ),
    ).toEqual(bytes(registration.stopInitB64))
    expect(
      concat(
        utf8('WS2Q'),
        Uint8Array.of(2, 2, 0, 0),
        bytes(registration.stopChallengeIdB64),
        bytes(registration.stopChallengeNonceB64),
        u64(BigInt(registration.stopExpiresAt)),
      ),
    ).toEqual(bytes(registration.stopChallengeB64))
    expect(
      concat(
        utf8('WS2V'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(identity.senderPublicKeyB64),
        bytes(registration.stopSignatureB64),
      ),
    ).toEqual(bytes(registration.stopProofB64))
    expect(
      concat(
        utf8('WS2Y'),
        Uint8Array.of(2, 0, 0, 0),
        bytes(registration.stopIdB64),
      ),
    ).toEqual(bytes(registration.stoppedB64))

    const openedOperationError = expectSenderControl(operationError, senderPublicKey)
    expect(openedOperationError.messageKind).toBe(10)
    expect(openedOperationError.operationId).toEqual(bytes(operationError.operationIdB64))

    const openedGrant = expectSenderControl({
      shareInstanceB64: laneAttach.shareInstanceB64,
      protocolSessionIdB64: laneAttach.protocolSessionIdB64,
      laneId: laneAttach.grantLaneId,
      laneEpoch: laneAttach.grantLaneEpoch,
      sequence: laneAttach.grantSequence,
      trafficKeyB64: laneAttach.senderToReceiverKeyB64,
      semanticBodyCborB64: laneAttach.grantSemanticBodyCborB64,
      unsignedControlCborB64: laneAttach.unsignedGrantCborB64,
      signedControlCborB64: laneAttach.grantSignedBodyB64,
      controlPreimageB64: laneAttach.grantPreimageB64,
      controlSignatureB64: laneAttach.grantSignatureB64,
      plaintextB64: laneAttach.grantPlaintextB64,
      aadB64: laneAttach.grantAadB64,
      envelopeB64: laneAttach.grantEnvelopeB64,
    }, senderPublicKey)
    expect(openedGrant.messageKind).toBe(12)
    expect(openedGrant.operationId).toEqual(bytes(laneAttach.grantOperationIdB64))
    expect(laneAttach.attachedLaneEpoch).toBe(laneAttach.grantLaneEpoch + 1)
    const laneHelloBase = bytes(laneAttach.laneHelloBaseB64)
    const laneHelloProof = hmacSha256(
      bytes(laneAttach.receiverToSenderKeyB64),
      concat(utf8('windshare/v2 lane-hello\0'), laneHelloBase),
    )
    expect(laneHelloProof).toEqual(bytes(laneAttach.laneHelloProofB64))
    expect(concat(laneHelloBase, laneHelloProof)).toEqual(bytes(laneAttach.laneHelloB64))
    const laneAckBody = bytes(laneAttach.laneAckBodyB64)
    expect(
      verify(
        null,
        concat(utf8('windshare/v2 lane-accept\0'), sha256(laneAckBody)),
        senderPublicKey,
        bytes(laneAttach.laneAckSignatureB64),
      ),
    ).toBe(true)
    expect(concat(laneAckBody, bytes(laneAttach.laneAckSignatureB64))).toEqual(
      bytes(laneAttach.laneAckB64),
    )

    const expectedOpenResultsBody = new Map<number, unknown>([
          [0, 1],
          [
            1,
            [
              [
                bytes(openResults.fileIdB64),
                0,
                bytes(openResults.revisionObjectB64),
                bytes(openResults.leaseIdB64),
                Number(openResults.leaseTtlMilliseconds),
                Number(openResults.renewAfterMilliseconds),
              ],
            ],
          ],
        ])
    const expectedOpenResults = Uint8Array.from(
      encode(
        new Map<number, unknown>([[0, 1], [1, expectedOpenResultsBody]]),
        rfc8949EncodeOptions,
      ),
    )
    expect(expectedOpenResults).toEqual(bytes(openResults.unsignedControlCborB64))
    expect(Number(openResults.leaseTtlMilliseconds)).toBe(120_000)
    expect(Number(openResults.renewAfterMilliseconds)).toBe(60_000)
    const openedOpenResults = expectSenderControl(openResults, senderPublicKey)
    expect(openedOpenResults.messageKind).toBe(4)
    expect(openedOpenResults.operationId).toEqual(bytes(openResults.operationIdB64))

    const expectedLeaseResultBody = new Map<number, unknown>([
      [0, 1],
      [1, bytes(leaseResult.leaseIdB64)],
      [2, Number(leaseResult.leaseTtlMilliseconds)],
      [3, Number(leaseResult.renewAfterMilliseconds)],
    ])
    const expectedLeaseResult = Uint8Array.from(
      encode(
        new Map<number, unknown>([[0, 1], [1, expectedLeaseResultBody]]),
        rfc8949EncodeOptions,
      ),
    )
    expect(expectedLeaseResult).toEqual(bytes(leaseResult.unsignedControlCborB64))
    const openedLeaseResult = expectSenderControl(leaseResult, senderPublicKey)
    expect(openedLeaseResult.messageKind).toBe(15)
    expect(openedLeaseResult.operationId).toEqual(bytes(leaseResult.operationIdB64))

    const openedTerminal = expectSenderControl(terminal, senderPublicKey)
    expect(openedTerminal.messageKind).toBe(11)
    expect(openedTerminal.operationId).toBeNull()
    expect(openedTerminal.body.get(1)).toBe(0x1008)
    expect(openedTerminal.body.get(2)).toBe('Sender stopped')
  })

})

describe('R0 resource and state-machine contract', () => {
  it('enforces fragment geometry and state-machine boundary semantics', () => {
    const encoded = bytes(fragment.fragmentPlaintextB64)
    const header = Buffer.from(encoded.subarray(0, FRAGMENT_HEADER_BYTES))
    const payload = encoded.subarray(FRAGMENT_HEADER_BYTES)
    expect(encoded.subarray(0, 4)).toEqual(Uint8Array.of(1, 8, 1, 0))
    expect(encoded.subarray(4, 20)).toEqual(bytes(fragment.operationIdB64))
    expect(encoded.subarray(20, 36)).toEqual(bytes(fragment.recordIdB64))
    expect(header.readUInt32BE(36)).toBe(fragment.fragmentIndex)
    expect(header.readUInt32BE(40)).toBe(fragment.fragmentCount)
    expect(header.readUInt32BE(44)).toBe(Number(fragment.totalLength))
    expect(header.readUInt32BE(48)).toBe(Number(fragment.payloadLength))
    expect(payload.byteLength).toBe(Number(fragment.totalLength))
    expect(sha256(payload).subarray(0, 16)).toEqual(bytes(fragment.recordIdB64))
    expect(fragment.maxOperationPlaintextBytes + OPERATION_HEADER_BYTES + OPERATION_NONCE_BYTES + AES_GCM_TAG_BYTES).toBe(
      fragment.maxFrameBytes,
    )
    expect(fragment.maxFragmentPayloadBytes + FRAGMENT_HEADER_BYTES).toBe(
      fragment.maxOperationPlaintextBytes,
    )
    expect(fragment.maxBlockRecordBytes).toBe(4 * 1024 * 1024 + 512)
    expect(fragment.fragmentTimeoutSeconds).toBe(15)
    expect(fragment.tombstoneSeconds).toBe(30)
    expect(
      validSingleFragment(
        encoded,
        bytes(fragment.operationIdB64),
        bytes(fragment.recordIdB64),
        fragment.maxFragmentPayloadBytes,
        fragment.maxBlockRecordBytes,
      ),
    ).toBe(true)
    const fragmentMutations = new Map<string, (value: Uint8Array) => void>([
      ['operation-id', (value) => { value[4] = value[4]! ^ 1 }],
      ['record-id', (value) => { value[20] = value[20]! ^ 1 }],
      ['index', (value) => {
        Buffer.from(value.buffer, value.byteOffset, value.byteLength).writeUInt32BE(1, 36)
      }],
      ['count', (value) => {
        Buffer.from(value.buffer, value.byteOffset, value.byteLength).writeUInt32BE(2, 40)
      }],
      ['total-length', (value) => {
        Buffer.from(value.buffer, value.byteOffset, value.byteLength).writeUInt32BE(
          Number(fragment.totalLength) + 1,
          44,
        )
      }],
      ['payload-length', (value) => {
        Buffer.from(value.buffer, value.byteOffset, value.byteLength).writeUInt32BE(
          Number(fragment.payloadLength) + 1,
          48,
        )
      }],
      ['last-flag', (value) => { value[2] = 0 }],
    ])
    expect([...fragmentMutations.keys(), 'conflicting-duplicate']).toEqual(
      fragment.mutationsRejected,
    )
    for (const [name, mutate] of fragmentMutations) {
      const candidate = encoded.slice()
      mutate(candidate)
      expect(
        validSingleFragment(
          candidate,
          bytes(fragment.operationIdB64),
          bytes(fragment.recordIdB64),
          fragment.maxFragmentPayloadBytes,
          fragment.maxBlockRecordBytes,
        ),
        name,
      ).toBe(false)
    }
    const conflictingDuplicate = encoded.slice()
    conflictingDuplicate[conflictingDuplicate.byteLength - 1] =
      conflictingDuplicate[conflictingDuplicate.byteLength - 1]! ^ 1
    expect(fragmentDuplicateConflicts(encoded, conflictingDuplicate)).toBe(true)

    expect(maximumFragment.recordPattern).toBe('byte((index*31+7)&255)')
    const maximumRecord = new Uint8Array(Number(maximumFragment.recordLength))
    for (let index = 0; index < maximumRecord.byteLength; index++) {
      maximumRecord[index] = (index * 31 + 7) & 0xff
    }
    expect(sha256(maximumRecord)).toEqual(bytes(maximumFragment.recordDigestB64))
    const maximumRecordId = sha256(maximumRecord).subarray(0, 16)
    expect(maximumRecordId).toEqual(bytes(maximumFragment.recordIdB64))
    const maximumFragments: Uint8Array[] = []
    for (let index = 0; index < maximumFragment.fragmentCount; index++) {
      const start = index * maximumFragment.maxFragmentPayloadBytes
      const end = Math.min(start + maximumFragment.maxFragmentPayloadBytes, maximumRecord.length)
      const fragmentHeader = Buffer.alloc(FRAGMENT_HEADER_BYTES)
      fragmentHeader[0] = 1
      fragmentHeader[1] = 8
      fragmentHeader[2] = index === maximumFragment.fragmentCount - 1 ? 1 : 0
      fragmentHeader.set(bytes(maximumFragment.operationIdB64), 4)
      fragmentHeader.set(maximumRecordId, 20)
      fragmentHeader.writeUInt32BE(index, 36)
      fragmentHeader.writeUInt32BE(maximumFragment.fragmentCount, 40)
      fragmentHeader.writeUInt32BE(maximumRecord.length, 44)
      fragmentHeader.writeUInt32BE(end - start, 48)
      maximumFragments.push(concat(fragmentHeader, maximumRecord.subarray(start, end)))
    }
    expect(
      maximumFragments.map((value) => Buffer.from(sha256(value)).toString('base64')),
    ).toEqual(maximumFragment.fragmentDigestsB64)
    expect(maximumFragments[0]).toEqual(bytes(maximumFragment.firstFragmentB64))
    expect(maximumFragments.at(-1)).toEqual(bytes(maximumFragment.lastFragmentB64))
    expect(maximumFragments[0]!.byteLength).toBe(maximumFragment.maxOperationPlaintextBytes)
    expect(maximumFragments.at(-1)!.byteLength - FRAGMENT_HEADER_BYTES).toBe(
      Number(maximumFragment.lastPayloadLength),
    )

    const limits = semantics.find((value) => value.name === 'frozen-limits')?.values
    expect(limits).toEqual({
      activeLanesProcess: '1024',
      activeLanesSession: '16',
      activeLanesShare: '256',
      activeLeasesProcess: '1024',
      activeLeasesSession: '32',
      activeLeasesShare: '256',
      applicationRelaySeconds: '8',
      catalogMemoryProcess: '536870912',
      catalogMemorySession: '16777216',
      catalogMemoryShare: '67108864',
      catalogSpillProcess: '17179869184',
      catalogSpillShare: '2147483648',
      clientHelloReplaySeconds: '300',
      committedEntriesProcess: '16777216',
      committedEntriesShare: '4194304',
      controlQueueBytes: '4194304',
      controlQueueFrames: '256',
      dataQueueBytes: '67108864',
      dataQueueFrames: '1024',
      joinStartingSeconds: '5',
      leaseMaximumSeconds: '7200',
      leaseRenewWindowSeconds: '60',
      leaseTTLSeconds: '120',
      logicalLanesSession: '16',
      maxBlockRequestIndices: '256',
      maxCatalogPageBytes: '61440',
      maxCatalogPageEntries: '256',
      maxChunkBytes: '4194304',
      maxDataFairnessBurst: '8',
      maxDescriptorBytes: '16384',
      maxDirectoryEntries: '1048576',
      maxFileBytes: '9007199254740991',
      maxFrameBytes: '65536',
      maxInitialRangesPerFile: '256',
      maxInitialRangesPerOpen: '1024',
      maxOpaqueCiphertextBytes: '65536',
      maxOpenBatch: '64',
      maxSelectedRootNameBytes: '1048576',
      maxSelectedRoots: '4096',
      minChunkBytes: '1024',
      opfsMinimumReserveBytes: '536870912',
      opfsStagingJobBytes: '8589934592',
      opfsStagingProcessBytes: '17179869184',
      operationTombstoneSeconds: '30',
      outputOpenTransactions: '32',
      reassemblyOperation: '4194816',
      reassemblyProcess: '1073741824',
      reassemblyRecordsProcess: '256',
      reassemblyRecordsSession: '16',
      reassemblyRecordsShare: '64',
      reassemblySession: '67108864',
      reassemblyShare: '268435456',
      receiverCacheProcess: '1073741824',
      receiverCacheSession: '134217728',
      relayChallengeSeconds: '30',
      relaySessionTombstoneSeconds: '60',
      revisionGraceSeconds: '120',
      scanConcurrencyProcess: '64',
      scanConcurrencySession: '4',
      scanConcurrencyShare: '16',
      scanWorkProcess: '16777216',
      scanWorkSession: '1048576',
      scanWorkShare: '4194304',
      sealedCacheProcess: '2147483648',
      sealedCacheShare: '268435456',
      segmentBytes: '17179869184',
      senderCrashGraceSeconds: '60',
      stableHandlesProcess: '1024',
      stableHandlesSession: '32',
      stableHandlesShare: '256',
    })

    const errorDomains = semantics.find((value) => value.name === 'error-domains') as
      | (SemanticsVector & {
          readonly session: Readonly<Record<string, number>>
          readonly directory: Readonly<Record<string, number>>
          readonly revision: Readonly<Record<string, number>>
          readonly block: Readonly<Record<string, number>>
          readonly peer: Readonly<Record<string, number>>
        })
      | undefined
    expect(errorDomains).toEqual({
      name: 'error-domains',
      session: {
        auth: 0x1001,
        'replay-sequence': 0x1002,
        malformed: 0x1003,
        version: 0x1004,
        budget: 0x1005,
        'sender-signature': 0x1006,
        'illegal-terminal': 0x1007,
        'sender-stopped': 0x1008,
      },
      directory: {
        stale: 0x2001,
        permission: 0x2002,
        collision: 0x2003,
        'too-wide': 0x2004,
        budget: 0x2005,
        'permanent-io': 0x2006,
        'transient-io': 0x2007,
        cancelled: 0x2008,
      },
      revision: {
        stale: 0x3001,
        'not-found': 0x3002,
        unreadable: 0x3003,
        'unsupported-stability': 0x3004,
        quota: 0x3005,
        'lease-expired': 0x3006,
        drift: 0x3007,
        'invalid-lease': 0x3008,
      },
      block: {
        'invalid-ref': 0x4001,
        'out-of-range': 0x4002,
        'object-auth': 0x4003,
        'fragment-conflict': 0x4004,
        timeout: 0x4005,
        cancelled: 0x4006,
      },
      peer: {
        negotiation: 0x5001,
        timeout: 0x5002,
        candidates: 0x5003,
        admission: 0x5004,
      },
    })
    expect(semantics.find((value) => value.name === 'relay-registration-errors')).toEqual({
      name: 'relay-registration-errors',
      codes: {
        malformed: 1,
        'unsupported-mode': 2,
        'share-id-collision': 3,
        'already-registered': 4,
        'challenge-expired': 5,
        'invalid-proof': 6,
        'descriptor-invalid': 7,
        'not-found': 8,
        starting: 9,
        admission: 10,
        stopped: 11,
      },
    })
  })
})

describe('R0 scheduling, recovery, and lifecycle contract', () => {
  it('enforces connection, output, source, and lifecycle state tables', () => {
    const selections = semantics.find((value) => value.name === 'selection-classification')
      ?.cases as readonly SelectionCase[]
    for (const selection of selections) {
      expect(classifySelection(selection)).toBe(selection.class)
    }

    expect(semantics.find((value) => value.name === 'operation-final-matrix')).toEqual({
      name: 'operation-final-matrix',
      operations: [
        { request: 'renew-lease', legalFinals: ['lease-result', 'operation-error'] },
        { request: 'release-lease', legalFinals: ['operation-complete', 'operation-error'] },
        { request: 'request-blocks', legalFinals: ['operation-complete', 'operation-error'] },
      ],
    })
    expect(semantics.find((value) => value.name === 'connection-timing')).toEqual({
      name: 'connection-timing',
      triggers: [
        {
          trigger: 'browse',
          startsP2P: false,
          p2pStartSeconds: null,
          applicationRelayDeadlineSeconds: null,
          outputPicker: 'none',
        },
        {
          trigger: 'preview-click',
          startsP2P: true,
          p2pStartSeconds: '0',
          applicationRelayDeadlineSeconds: '8',
          outputPicker: 'none',
        },
        {
          trigger: 'download-click',
          startsP2P: true,
          p2pStartSeconds: '0',
          applicationRelayDeadlineSeconds: '8',
          outputPicker: 'synchronous',
        },
      ],
      independentTimers: true,
      discoveryCannotDelay: true,
      unknownUsesNonSmallTiming: true,
      turnInsertionOnly: true,
    })

    const strictSequence = semantics.find((value) => value.name === 'strict-sequence') as
      | (SemanticsVector & {
          readonly cases: readonly {
            readonly epoch: number
            readonly expected: string
            readonly candidate: string
            readonly accepted: boolean
          }[]
        })
      | undefined
    for (const value of strictSequence?.cases ?? []) {
      expect(value.accepted).toBe(
        value.expected !== 'closed' && BigInt(value.candidate) === BigInt(value.expected),
      )
    }

    const laneEpochs = semantics.find((value) => value.name === 'lane-epoch-acceptance') as
      | (SemanticsVector & {
          readonly globallyAllocated: readonly number[]
          readonly cases: readonly {
            readonly lastAccepted: number | null
            readonly candidate: number
            readonly accepted: boolean
          }[]
        })
      | undefined
    expect(new Set(laneEpochs?.globallyAllocated).size).toBe(
      laneEpochs?.globallyAllocated.length,
    )
    for (const value of laneEpochs?.cases ?? []) {
      expect(value.accepted).toBe(
        value.lastAccepted === null || value.candidate > value.lastAccepted,
      )
    }

    const checkpoints = semantics.find(
      (value) => value.name === 'output-checkpoint-crash-cuts',
    )
    expect(checkpoints?.order).toEqual([
      'data-write',
      'data-flush',
      'journal-write',
      'journal-flush',
      'atomic-install',
      'reopen-verify',
    ])
    const cuts = checkpoints?.cuts as readonly CheckpointCut[]
    expect(cuts.filter((cut) => cut.published).map((cut) => cut.cut)).toEqual([
      'after-reopen-verify',
    ])

    expect(semantics.find((value) => value.name === 'output-backend-capabilities')).toEqual({
      name: 'output-backend-capabilities',
      backends: [
        {
          backend: 'fsa',
          durability: 'none-until-reauthorization-and-reopen-proof',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: false,
          powerLoss: false,
        },
        {
          backend: 'opfs-staging',
          durability: 'process-restart',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: false,
          powerLoss: false,
        },
        {
          backend: 'single-file-stream',
          durability: 'none',
          randomWrite: false,
          fileFailureIsolation: false,
          mtime: false,
          failureAfterFirstByte: 'abort-job',
        },
        {
          backend: 'zip-stream',
          durability: 'none',
          randomWrite: false,
          fileFailureIsolation: false,
          mtime: false,
          memberStart: 'first-local-file-header-byte',
        },
        {
          backend: 'cli-osfs',
          durability:
            'power-loss-when-file-and-directory-sync-proved-else-process-restart',
          randomWrite: true,
          fileFailureIsolation: true,
          mtime: true,
        },
      ],
    })
    expect(semantics.find((value) => value.name === 'zip-member-failure')).toEqual({
      name: 'zip-member-failure',
      cases: [
        {
          memberStarted: false,
          action: 'skip-and-report',
          jobOutcome: 'completed-with-errors',
        },
        { memberStarted: true, action: 'abort-job', jobOutcome: 'aborted' },
      ],
    })

    const catalog = semantics.find((value) => value.name === 'catalog-transaction')
    expect(catalog?.preCommitCrashVisible).toBe(false)
    expect(catalog?.publishOnlyAfter?.at(-1)).toBe('atomic-commit')
    expect(semantics.find((value) => value.name === 'stable-source-platforms')).toEqual({
      name: 'stable-source-platforms',
      platforms: [
        {
          platform: 'windows-local-ntfs-refs',
          mechanism: 'deny-share-write-handle+volume-file-id',
          supported: true,
        },
        {
          platform: 'linux-local-regular',
          mechanism: 'device+inode+size+mtime-ns+ctime-ns',
          supported: true,
        },
        {
          platform: 'darwin-local-regular',
          mechanism: 'device+inode+size+mtime-ns+ctime-ns',
          supported: true,
        },
        {
          platform: 'other-network-pseudo',
          mechanism: 'unsupported-stability',
          supported: false,
        },
      ],
    })
    const lifecycle = semantics.find((value) => value.name === 'offline-lifecycle') as
      | (SemanticsVector & {
          readonly transitions: readonly {
            readonly from: string
            readonly event: string
            readonly to: string
          }[]
          readonly explicitStopEffects: readonly string[]
          readonly unexpectedDisconnectStates: readonly string[]
        })
      | undefined
    expect(lifecycle?.states?.at(-1)).toBe('stopped')
    expect(lifecycle?.explicitStopUsesCrashGrace).toBe(false)
    expect(lifecycle?.transitions).toEqual([
      { from: 'preparing', event: 'registered', to: 'live-only' },
      { from: 'preparing', event: 'stop', to: 'stopping' },
      { from: 'live-only', event: 'begin-offline', to: 'offline-uploading' },
      { from: 'live-only', event: 'stop', to: 'stopping' },
      { from: 'offline-uploading', event: 'commit-ack', to: 'offline-committed' },
      { from: 'offline-uploading', event: 'stop', to: 'stopping' },
      { from: 'stopping', event: 'cleanup-complete', to: 'stopped' },
      { from: 'offline-committed', event: 'sender-exit', to: 'offline-committed' },
    ])
    expect(lifecycle?.explicitStopEffects).toContain('challenged-signed-stop')
    expect(lifecycle?.unexpectedDisconnectStates).toEqual([
      'live-only',
      'offline-uploading',
    ])
  })
})
