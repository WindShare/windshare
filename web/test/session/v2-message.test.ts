import { describe, expect, it } from 'vitest'

import { decodeV2PeerAnswer, decodeV2PeerCandidate } from '../../src/connectivity/v2-signaling-codec'
import { decodeCanonicalCbor, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
	decodeV2OperationErrorControl,
	encodeV2Message,
  encodeV2Body,
  type V2ControlBinding,
  validateV2SenderControlBody,
  verifyV2SenderControl,
  V2_MESSAGE_KIND,
  V2_PEER_OPERATION_CODE,
} from '../../src/session/v2-message'
import { senderControlKeyPair, signSenderOperationControl } from './signed-control-fixture'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 authenticated sender control schemas', () => {
  it('freezes operation retry delays to 1 through 30000 milliseconds', () => {
		const operationError = (retryAfterMilliseconds: number) => encodeV2Body(
			new Map<number, unknown>([
				[0, 1],
				[1, 4],
				[2, 0x4001],
				[3, true],
				[4, retryAfterMilliseconds],
				[5, 'retry later'],
			]),
		)
		expect(() => decodeV2OperationErrorControl(operationError(0))).toThrow(/limits/i)
    expect(decodeV2OperationErrorControl(operationError(30_000)).retryAfterMilliseconds)
      .toBe(30_000)
    expect(() => decodeV2OperationErrorControl(operationError(30_001))).toThrow(/limits/i)
    expect(() => decodeV2OperationErrorControl(encodeV2Body(new Map<number, unknown>([
      [0, 1], [1, 4], [2, 0x4001], [3, false], [4, null], [5, ''],
    ])))).toThrow(/empty/i)
	})

  it('rejects signed revision retry delays outside 1 through 30000 milliseconds', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(2)
    const binding = controlBinding(0n)
    const signedOpenResult = async (retryAfterMilliseconds: number) => signSenderOperationControl({
      kind: V2_MESSAGE_KIND.openResults,
      operationId,
      semanticBody: encodeV2Body(new Map<number, unknown>([
        [0, 1],
        [1, [[identity(3), 1, 0x3001, true, retryAfterMilliseconds]]],
      ])),
      binding,
      privateKey: keys.privateKey,
    })

    for (const accepted of [1, 30_000]) {
      const signed = await signedOpenResult(accepted)
      await expect(verifyV2SenderControl(signed.message, binding, keys.publicKey)).resolves.toBeDefined()
    }
    for (const rejected of [0, 30_001]) {
      const signed = await signedOpenResult(rejected)
      await expect(verifyV2SenderControl(signed.message, binding, keys.publicKey))
        .rejects.toThrow(/revision retry delay/i)
    }
  })

	it('validates result, completion, lease, and lane-grant bodies before routing', () => {
    const validCases = [
      [V2_MESSAGE_KIND.catalogResult, new Map<number, unknown>([[0, 1], [1, identity(1)]])],
      [V2_MESSAGE_KIND.openResults, new Map<number, unknown>([[
        0, 1,
      ], [
        1, [[identity(2), 1, 0x3001, false, null]],
      ]])],
      [V2_MESSAGE_KIND.operationComplete, new Map<number, unknown>([[0, 1], [1, 0]])],
      [V2_MESSAGE_KIND.leaseResult, new Map<number, unknown>([
        [0, 1], [1, identity(3)], [2, 120_000], [3, 60_000],
      ])],
      [V2_MESSAGE_KIND.laneAttach, new Map<number, unknown>([
        [0, 1], [1, 1], [2, 2], [3, 4], [4, identity(5)],
      ])],
    ] as const

    for (const [kind, body] of validCases) {
      expect(() => validateV2SenderControlBody(kind, encodeV2Body(body))).not.toThrow()
    }
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.leaseResult,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, identity(6)], [2, 119_999], [3, 60_000],
      ])),
    )).toThrow(/lease timing/i)
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.laneAttach,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, 1], [2, 0], [3, 4], [4, identity(7)],
      ])),
    )).toThrow(/lane grant ID/i)
  })

  it('freezes permanent peer failure scope and code bounds', () => {
    const peerError = (code: number, retryable = false) => encodeV2Body(
      new Map<number, unknown>([
        [0, 1], [1, 5], [2, code], [3, retryable], [4, retryable ? 1 : null],
        [5, 'Peer negotiation failed'],
      ]),
    )
    expect(decodeV2OperationErrorControl(
      peerError(V2_PEER_OPERATION_CODE.negotiation),
    ).scope).toBe('peer')
    expect(decodeV2OperationErrorControl(
      peerError(V2_PEER_OPERATION_CODE.admission),
    ).code).toBe(V2_PEER_OPERATION_CODE.admission)
    expect(() => decodeV2OperationErrorControl(peerError(0x5000))).toThrow(/inconsistent/i)
    expect(() => decodeV2OperationErrorControl(peerError(0x5005))).toThrow(/inconsistent/i)
    expect(() => decodeV2OperationErrorControl(
      peerError(V2_PEER_OPERATION_CODE.timeout, true),
    )).toThrow(/permanent/i)
  })

  it('validates authenticated peer answer and candidate bounds', () => {
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.peerAnswer,
      encodeV2Body([1, identity(8), identity(9), 'answer-sdp']),
    )).not.toThrow()
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.peerCandidate,
      encodeV2Body([1, identity(8), identity(9), 'candidate', null, 0, null]),
    )).not.toThrow()
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.peerAnswer,
      encodeV2Body([1, identity(8), identity(9), '']),
    )).toThrow(/empty/i)
    expect(() => validateV2SenderControlBody(
      V2_MESSAGE_KIND.peerCandidate,
      encodeV2Body([1, identity(8), identity(9), 'candidate', null, 65_536, null]),
    )).toThrow(/unsigned 16-bit/i)
  })

  it('unwraps signed map and array semantics without changing their typed body', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(10)
    const binding = controlBinding(0n)
    const answerBody = encodeV2Body([1, identity(8), identity(9), 'answer-sdp'])
    const candidateBody = encodeV2Body([
      1, identity(8), identity(9), 'candidate', 'data', 0, 'windshare',
    ])
    const catalogBody = encodeV2Body(new Map<number, unknown>([[0, 1], [1, identity(11)]]))
    const answer = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: answerBody,
      binding,
      privateKey: keys.privateKey,
    })
    const candidate = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: candidateBody,
      binding: { ...binding, sequence: 1n },
      privateKey: keys.privateKey,
    })
    const catalog = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.catalogResult,
      operationId,
      semanticBody: catalogBody,
      binding: { ...binding, sequence: 2n },
      privateKey: keys.privateKey,
    })

    const verifiedAnswer = await verifyV2SenderControl(
      answer.message,
      binding,
      keys.publicKey,
    )
    const verifiedCandidate = await verifyV2SenderControl(
      candidate.message,
      { ...binding, sequence: 1n },
      keys.publicKey,
    )
    const verifiedCatalog = await verifyV2SenderControl(
      catalog.message,
      { ...binding, sequence: 2n },
      keys.publicKey,
    )

    expect(verifiedAnswer).toEqual(answerBody)
    expect(decodeV2PeerAnswer(verifiedAnswer).sdp).toBe('answer-sdp')
    expect(verifiedCandidate).toEqual(candidateBody)
    expect(decodeV2PeerCandidate(verifiedCandidate).candidate).toBe('candidate')
    expect(verifiedCatalog).toEqual(catalogBody)
  })

  it('rejects signature, semantic type, delivery-axis, and replay substitution', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(12)
    const binding = controlBinding(7n)
    const answerBody = encodeV2Body([1, identity(8), identity(9), 'answer-sdp'])
    const signed = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: answerBody,
      binding,
      privateKey: keys.privateKey,
    })

    const signatureChanged = replaceSignedField(signed.message, 255, changedSignature(signed.signature))
    await expect(verifyV2SenderControl(signatureChanged, binding, keys.publicKey)).rejects
      .toThrow(/signature/i)
    const bodyChanged = replaceSignedField(
      signed.message,
      1,
      [1, identity(8), identity(9), 'other-answer'],
    )
    await expect(verifyV2SenderControl(bodyChanged, binding, keys.publicKey)).rejects
      .toThrow(/signature/i)

    const wrongSemanticType = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: encodeV2Body([1, identity(8), identity(9), 'candidate', null, 0, null]),
      binding,
      privateKey: keys.privateKey,
    })
    await expect(verifyV2SenderControl(wrongSemanticType.message, binding, keys.publicKey)).rejects
      .toThrow(/bounded array|field count/i)
    const changedKind = encodeV2Message(
      V2_MESSAGE_KIND.peerCandidate,
      operationId,
      signed.message.body,
    )
    await expect(verifyV2SenderControl(changedKind, binding, keys.publicKey)).rejects
      .toThrow(/signature/i)
    const changedOperation = encodeV2Message(
      V2_MESSAGE_KIND.peerAnswer,
      identity(13),
      signed.message.body,
    )
    await expect(verifyV2SenderControl(changedOperation, binding, keys.publicKey)).rejects
      .toThrow(/signature/i)

    const changedBindings: readonly V2ControlBinding[] = [
      { ...binding, shareInstance: identity(14) },
      { ...binding, protocolSessionId: identity(15) },
      { ...binding, laneId: binding.laneId + 1 },
      { ...binding, laneEpoch: binding.laneEpoch + 1 },
      { ...binding, sequence: binding.sequence + 1n },
    ]
    for (const candidate of changedBindings) {
      await expect(verifyV2SenderControl(signed.message, candidate, keys.publicKey)).rejects
        .toThrow(/signature/i)
    }
    await expect(verifyV2SenderControl(
      signed.message,
      { ...binding, direction: 0 } as unknown as V2ControlBinding,
      keys.publicKey,
    )).rejects.toThrow(/delivery identity/i)
  })
})

function controlBinding(sequence: bigint): V2ControlBinding {
  return Object.freeze({
    shareInstance: identity(20),
    protocolSessionId: identity(21),
    laneId: 22,
    laneEpoch: 23,
    direction: 1,
    sequence,
  })
}

function replaceSignedField(
  message: ReturnType<typeof encodeV2Message>,
  key: number,
  value: unknown,
): ReturnType<typeof encodeV2Message> {
  const decoded = decodeCanonicalCbor(message.body, 65_536, 'signed control test wrapper')
  if (!(decoded instanceof Map)) throw new Error('signed control test wrapper is not a map')
  decoded.set(key, value)
  return encodeV2Message(message.kind, message.operationId, encodeCanonicalCbor(decoded))
}

function changedSignature(signature: Uint8Array): Uint8Array<ArrayBuffer> {
  const changed = signature.slice()
  changed[0] = (changed[0] ?? 0) ^ 0x80
  return changed
}
