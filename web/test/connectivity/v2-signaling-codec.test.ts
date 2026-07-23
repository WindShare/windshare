import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import {
  decodeV2PeerAnswer,
  decodeV2PeerCandidate,
  decodeV2PeerOffer,
  encodeV2PeerAnswer,
  encodeV2PeerCandidate,
  encodeV2PeerOffer,
} from '../../src/connectivity/v2-signaling-codec'
import { concatBytes } from '../../src/crypto/bytes'
import { decodeCanonicalCbor, encodeCanonicalCbor, requireBytes, requireNumericMap } from '../../src/protocol/cbor'
import {
  encodeV2Message,
  type V2ControlBinding,
  verifyV2SenderControl,
  V2_MESSAGE_KIND,
} from '../../src/session/v2-message'

interface SignedControlVector {
  readonly sequence: string
  readonly unsignedWrapperCborB64: string
  readonly controlPreimageB64: string
  readonly controlSignatureB64: string
  readonly signedBodyB64: string
}

interface SignalingVector {
  readonly schemaVersion: number
  readonly messageKinds: { readonly offer: number; readonly answer: number; readonly candidate: number }
  readonly peerPathIdB64: string
  readonly attemptIdB64: string
  readonly senderPublicKeyB64: string
  readonly controlBinding: {
    readonly shareInstanceB64: string
    readonly protocolSessionIdB64: string
    readonly laneId: number
    readonly laneEpoch: number
    readonly direction: number
    readonly operationIdB64: string
  }
  readonly offer: { readonly sdp: string; readonly bodyB64: string }
  readonly answer: SignedControlVector & { readonly sdp: string; readonly bodyB64: string }
  readonly candidate: {
    readonly candidate: string
    readonly sdpMid: string
    readonly sdpMLineIndex: number
    readonly usernameFragment: string
    readonly bodyB64: string
  } & SignedControlVector
}

const vector = JSON.parse(readFileSync(
  new URL('../../../core/testvectors/v2-peer-signaling.json', import.meta.url),
  'utf8',
)) as SignalingVector

function bytes(value: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(Buffer.from(value, 'base64'))
}

describe('v2 E2E peer signaling codec', () => {
  it('matches the shared Go/TypeScript deterministic bodies and reserved kinds', () => {
    const binding = {
      peerPathId: bytes(vector.peerPathIdB64),
      attemptId: bytes(vector.attemptIdB64),
    }
    expect(vector.schemaVersion).toBe(1)
    expect(V2_MESSAGE_KIND.peerOffer).toBe(vector.messageKinds.offer)
    expect(V2_MESSAGE_KIND.peerAnswer).toBe(vector.messageKinds.answer)
    expect(V2_MESSAGE_KIND.peerCandidate).toBe(vector.messageKinds.candidate)

    const offer = { ...binding, sdp: vector.offer.sdp }
    const answer = { ...binding, sdp: vector.answer.sdp }
    const candidate = {
      ...binding,
      candidate: vector.candidate.candidate,
      sdpMid: vector.candidate.sdpMid,
      sdpMLineIndex: vector.candidate.sdpMLineIndex,
      usernameFragment: vector.candidate.usernameFragment,
    }
    expect(encodeV2PeerOffer(offer)).toEqual(bytes(vector.offer.bodyB64))
    expect(decodeV2PeerOffer(bytes(vector.offer.bodyB64))).toEqual(offer)
    expect(encodeV2PeerAnswer(answer)).toEqual(bytes(vector.answer.bodyB64))
    expect(decodeV2PeerAnswer(bytes(vector.answer.bodyB64))).toEqual(answer)
    expect(encodeV2PeerCandidate(candidate)).toEqual(bytes(vector.candidate.bodyB64))
    expect(decodeV2PeerCandidate(bytes(vector.candidate.bodyB64))).toEqual(candidate)
  })

  it('opens the Go-authored signed answer and candidate wrappers as their typed bodies', async () => {
    expect(vector.controlBinding.direction).toBe(1)
    const operationId = bytes(vector.controlBinding.operationIdB64)
    const baseBinding = {
      shareInstance: bytes(vector.controlBinding.shareInstanceB64),
      protocolSessionId: bytes(vector.controlBinding.protocolSessionIdB64),
      laneId: vector.controlBinding.laneId,
      laneEpoch: vector.controlBinding.laneEpoch,
      direction: 1 as const,
    }
    const cases = [
      [V2_MESSAGE_KIND.peerAnswer, vector.answer, decodeV2PeerAnswer],
      [V2_MESSAGE_KIND.peerCandidate, vector.candidate, decodeV2PeerCandidate],
    ] as const

    for (const [kind, control, decodeBody] of cases) {
      const semanticBody = bytes(control.bodyB64)
      const binding: V2ControlBinding = {
        ...baseBinding,
        sequence: BigInt(control.sequence),
      }
      const expectedWrapper = encodeCanonicalCbor(new Map<number, unknown>([
        [0, 1],
        [1, decodeCanonicalCbor(semanticBody, 65_536, 'peer signaling vector body')],
      ]))
      expect(expectedWrapper).toEqual(bytes(control.unsignedWrapperCborB64))
      expect(peerControlPreimage(kind, operationId, binding, expectedWrapper)).toEqual(
        bytes(control.controlPreimageB64),
      )
      const signedBody = bytes(control.signedBodyB64)
      const signedFields = requireNumericMap(
        decodeCanonicalCbor(signedBody, 65_536, 'signed peer control vector'),
        [0, 1, 255],
        'signed peer control vector',
      )
      expect(requireBytes(signedFields.get(255), 64, 'peer control signature')).toEqual(
        bytes(control.controlSignatureB64),
      )
      const verified = await verifyV2SenderControl(
        encodeV2Message(kind, operationId, signedBody),
        binding,
        bytes(vector.senderPublicKeyB64),
      )
      expect(verified).toEqual(semanticBody)
      expect(decodeBody(verified)).toMatchObject({
        peerPathId: bytes(vector.peerPathIdB64),
        attemptId: bytes(vector.attemptIdB64),
      })
    }
  })

  it('rejects zero bindings, malformed or non-NFC text, and uint16 overflow', () => {
    const binding = {
      peerPathId: bytes(vector.peerPathIdB64),
      attemptId: bytes(vector.attemptIdB64),
    }
    expect(() => encodeV2PeerOffer({ ...binding, peerPathId: new Uint8Array(16), sdp: 'v=0' }))
      .toThrow()
    expect(() => encodeV2PeerOffer({ ...binding, sdp: 'e\u0301' })).toThrow()
    expect(() => encodeV2PeerOffer({ ...binding, sdp: '\ud800' })).toThrow()
    expect(() => encodeV2PeerCandidate({
      ...binding,
      candidate: vector.candidate.candidate,
      sdpMid: null,
      sdpMLineIndex: 65_536,
      usernameFragment: null,
    })).toThrow()
  })
})

function peerControlPreimage(
  kind: number,
  operationId: Uint8Array,
  binding: V2ControlBinding,
  unsignedWrapper: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const lane = new Uint8Array(8)
  const laneView = new DataView(lane.buffer)
  laneView.setUint32(0, binding.laneId, false)
  laneView.setUint32(4, binding.laneEpoch, false)
  const sequence = new Uint8Array(8)
  new DataView(sequence.buffer).setBigUint64(0, binding.sequence, false)
  return concatBytes([
    new TextEncoder().encode('windshare/v2 control/operation\0'),
    binding.shareInstance,
    binding.protocolSessionId,
    lane,
    Uint8Array.of(binding.direction),
    sequence,
    Uint8Array.of(kind),
    operationId,
    createHash('sha256').update(unsignedWrapper).digest(),
  ])
}
