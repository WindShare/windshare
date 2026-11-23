import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import {
  decodePeerAnswer,
  decodePeerCandidate,
  decodePeerOffer,
  encodePeerAnswer,
  encodePeerCandidate,
  encodePeerOffer,
  samePeerBinding,
} from '../../src/connectivity/signaling-codec'
import { decodeCanonicalCbor, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
  encodeMessage,
  decodeMessage,
  validateSenderControlBody,
  MESSAGE_KIND,
} from '../../src/session/message'

interface SignalingVector {
  readonly schemaVersion: number
  readonly peerSlot: number
  readonly attemptSequence: string
  readonly messageKinds: { readonly offer: number; readonly answer: number; readonly candidate: number }
  readonly peerPathIdB64: string
  readonly attemptIdB64: string
  readonly controlBinding: {
    readonly operationId: string
  }
  readonly turnOffer: { readonly sdp: string; readonly bodyB64: string }
  readonly offer: { readonly sdp: string; readonly bodyB64: string }
  readonly answer: { readonly sdp: string; readonly bodyB64: string }
  readonly candidate: {
    readonly candidate: string
    readonly sdpMid: string
    readonly sdpMLineIndex: number
    readonly usernameFragment: string
    readonly bodyB64: string
  }
}

const vector = JSON.parse(readFileSync(
  new URL('../../../core/testvectors/v2-peer-signaling.json', import.meta.url),
  'utf8',
)) as SignalingVector

function bytes(value: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(Buffer.from(value, 'base64'))
}

describe('v2 E2E peer signaling codec', () => {
  it('cannot substitute optional admission purpose for an essential attempt', () => {
    const initial = decodePeerOffer(bytes(vector.offer.bodyB64))
    for (const purpose of ['recovery', 'throughput'] as const) {
      const alternate = decodePeerOffer(encodePeerOffer({ ...initial, purpose }))
      expect(alternate.purpose).toBe(purpose)
      expect(samePeerBinding(initial, alternate)).toBe(false)
    }
    const fields = [...decodeCanonicalCbor(bytes(vector.offer.bodyB64), 65_536, 'offer') as readonly unknown[]]
    fields[5] = 3
    expect(() => decodePeerOffer(encodeCanonicalCbor(fields))).toThrow()
  })

  it('matches the shared Go/TypeScript deterministic bodies and reserved kinds', () => {
    const binding = {
      peerPathId: bytes(vector.peerPathIdB64),
      attemptId: bytes(vector.attemptIdB64),
      purpose: 'initial' as const,
      attemptSequence: BigInt(vector.attemptSequence),
      peerSlot: vector.peerSlot,
    }
    expect(vector.schemaVersion).toBe(5)
    const turn = decodePeerOffer(bytes(vector.turnOffer.bodyB64))
    expect(turn.purpose).toBe('turn-relay')
    expect(turn.turnGrantId?.[0]).toBe(1)
    expect(encodePeerOffer(turn)).toEqual(bytes(vector.turnOffer.bodyB64))
    expect(MESSAGE_KIND.peerOffer).toBe(vector.messageKinds.offer)
    expect(MESSAGE_KIND.peerAnswer).toBe(vector.messageKinds.answer)
    expect(MESSAGE_KIND.peerCandidate).toBe(vector.messageKinds.candidate)

    const offer = { ...binding, sdp: vector.offer.sdp }
    const answer = { ...binding, sdp: vector.answer.sdp }
    const candidate = {
      ...binding,
      candidate: vector.candidate.candidate,
      sdpMid: vector.candidate.sdpMid,
      sdpMLineIndex: vector.candidate.sdpMLineIndex,
      usernameFragment: vector.candidate.usernameFragment,
    }
    expect(encodePeerOffer(offer)).toEqual(bytes(vector.offer.bodyB64))
    expect(decodePeerOffer(bytes(vector.offer.bodyB64))).toEqual(offer)
    expect(encodePeerAnswer(answer)).toEqual(bytes(vector.answer.bodyB64))
    expect(decodePeerAnswer(bytes(vector.answer.bodyB64))).toEqual(answer)
    expect(encodePeerCandidate(candidate)).toEqual(bytes(vector.candidate.bodyB64))
    expect(decodePeerCandidate(bytes(vector.candidate.bodyB64))).toEqual(candidate)
  })

  it('decodes Go answer and candidate bodies from session messages', () => {
    const operationId = BigInt(vector.controlBinding.operationId)
    const cases = [
      [MESSAGE_KIND.peerAnswer, vector.answer, decodePeerAnswer],
      [MESSAGE_KIND.peerCandidate, vector.candidate, decodePeerCandidate],
    ] as const

    for (const [kind, control, decodeBody] of cases) {
      const encoded = encodeMessage(kind, operationId, bytes(control.bodyB64))
      const message = decodeMessage(encoded.plaintext)
      validateSenderControlBody(message.kind, message.body)
      expect(message.operationId).toEqual(operationId)
      expect(decodeBody(message.body)).toMatchObject({
        peerPathId: bytes(vector.peerPathIdB64),
        attemptId: bytes(vector.attemptIdB64),
        purpose: 'initial' as const,
        attemptSequence: BigInt(vector.attemptSequence),
        peerSlot: vector.peerSlot,
      })
    }
  })

  it('rejects zero bindings, malformed or non-NFC text, and uint16 overflow', () => {
    const binding = {
      peerPathId: bytes(vector.peerPathIdB64),
      attemptId: bytes(vector.attemptIdB64),
      purpose: 'initial' as const,
      attemptSequence: BigInt(vector.attemptSequence),
      peerSlot: vector.peerSlot,
    }
    expect(() => encodePeerOffer({ ...binding, peerPathId: new Uint8Array(16), sdp: 'v=0' }))
      .toThrow()
    expect(() => encodePeerOffer({ ...binding, sdp: 'e\u0301' })).toThrow()
    expect(() => encodePeerOffer({ ...binding, sdp: '\ud800' })).toThrow()
    expect(() => encodePeerCandidate({
      ...binding,
      candidate: vector.candidate.candidate,
      sdpMid: null,
      sdpMLineIndex: 65_536,
      usernameFragment: null,
    })).toThrow()
  })
})
