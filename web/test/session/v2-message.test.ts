import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { describe, expect, it } from 'vitest'

import { decodePeerAnswer, decodePeerCandidate } from '../../src/connectivity/signaling-codec'
import { decodeCanonicalCbor, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
	decodeOperationErrorControl,
	encodeMessage,
  encodeBody,
  decodeMessage,
  type ControlBinding,
  validateSenderControlBody,
  verifySenderControl,
  MESSAGE_KIND,
  PEER_OPERATION_CODE,
} from '../../src/session/message'
import { senderControlKeyPair, signSenderOperationControl } from './signed-control-fixture'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 session wire registry', () => {
  it.each(Object.entries(MESSAGE_KIND).filter(([, kind]) => kind !== MESSAGE_KIND.blockFragment))(
    'decodes the registered %s control message', (_name, kind) => {
      const sessionControl = kind === MESSAGE_KIND.sessionTerminal || kind === MESSAGE_KIND.peerPathControl || kind === MESSAGE_KIND.turnControl
      const encoded = encodeMessage(kind, sessionControl ? undefined : identity(1), encodeBody(null))
      expect(decodeMessage(encoded.plaintext)).toEqual(encoded)
    },
  )

  it.each([0n, 21n, 255n, 0xffffffffffffffffn, BigInt(MESSAGE_KIND.blockFragment)])(
    'rejects unknown kinds and the data codec on the control path: %s', kind => {
      const plaintext = encodeCanonicalCbor(new Map<number, unknown>([[0, kind], [1, null], [2, null]]))
      expect(() => decodeMessage(plaintext)).toThrow('unknown or uses the wrong codec')
    },
  )
})

describe('v2 authenticated sender control schemas', () => {
  it('freezes operation retry delays to 1 through 30000 milliseconds', () => {
		const operationError = (retryAfterMilliseconds: number) => encodeBody(
			new Map<number, unknown>([
				[0, 1],
				[1, 4],
				[2, 0x4001],
				[3, true],
				[4, retryAfterMilliseconds],
				[5, 'retry later'],
			]),
		)
		expect(() => decodeOperationErrorControl(operationError(0))).toThrow(/limits/i)
    expect(decodeOperationErrorControl(operationError(30_000)).retryAfterMilliseconds)
      .toBe(30_000)
    expect(() => decodeOperationErrorControl(operationError(30_001))).toThrow(/limits/i)
    expect(() => decodeOperationErrorControl(encodeBody(new Map<number, unknown>([
      [0, 1], [1, 4], [2, 0x4001], [3, false], [4, null], [5, ''],
    ])))).toThrow(/empty/i)
	})

  it('rejects signed revision retry delays outside 1 through 30000 milliseconds', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(2)
    const binding = controlBinding(0n)
    const signedOpenResult = async (retryAfterMilliseconds: number) => signSenderOperationControl({
      kind: MESSAGE_KIND.openResults,
      operationId,
      semanticBody: encodeBody(new Map<number, unknown>([
        [0, 1],
        [1, [[identity(3), 1, 0x3001, true, retryAfterMilliseconds]]],
      ])),
      binding,
      privateKey: keys.privateKey,
    })

    for (const accepted of [1, 30_000]) {
      const signed = await signedOpenResult(accepted)
      await expect(verifySenderControl(signed.message, binding, createEd25519Verifier(keys.publicKey))).resolves.toBeDefined()
    }
    for (const rejected of [0, 30_001]) {
      const signed = await signedOpenResult(rejected)
      await expect(verifySenderControl(signed.message, binding, createEd25519Verifier(keys.publicKey)))
        .rejects.toThrow(/revision retry delay/i)
    }
  })

	it('validates result, completion, lease, and lane-grant bodies before routing', () => {
    const validCases = [
      [MESSAGE_KIND.catalogResult, new Map<number, unknown>([[0, 1], [1, identity(1)]])],
      [MESSAGE_KIND.openResults, new Map<number, unknown>([[
        0, 1,
      ], [
        1, [[identity(2), 1, 0x3001, false, null]],
      ]])],
      [MESSAGE_KIND.operationComplete, new Map<number, unknown>([[0, 1], [1, 0]])],
      [MESSAGE_KIND.leaseResult, new Map<number, unknown>([
        [0, 1], [1, identity(3)], [2, 120_000], [3, 60_000],
      ])],
      [MESSAGE_KIND.laneAttach, new Map<number, unknown>([
        [0, 1], [1, 1], [2, 2], [3, 4], [4, identity(5)],
      ])],
    ] as const

    for (const [kind, body] of validCases) {
      expect(() => validateSenderControlBody(kind, encodeBody(body))).not.toThrow()
    }
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.leaseResult,
      encodeBody(new Map<number, unknown>([
        [0, 1], [1, identity(6)], [2, 119_999], [3, 60_000],
      ])),
    )).toThrow(/lease timing/i)
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.laneAttach,
      encodeBody(new Map<number, unknown>([
        [0, 1], [1, 1], [2, 0], [3, 4], [4, identity(7)],
      ])),
    )).toThrow(/lane grant ID/i)
  })

  it('freezes permanent peer failure scope and code bounds', () => {
    const peerError = (code: number, retryable = false) => encodeBody(
      new Map<number, unknown>([
        [0, 3], [1, 5], [2, code], [3, retryable], [4, retryable ? 1 : null],
        [5, 'Peer negotiation failed'], [6, [identity(8), identity(9), 1n, 0]],
      ]),
    )
    expect(decodeOperationErrorControl(
      peerError(PEER_OPERATION_CODE.negotiation),
    ).scope).toBe('peer')
    expect(decodeOperationErrorControl(
      peerError(PEER_OPERATION_CODE.admission),
    ).code).toBe(PEER_OPERATION_CODE.admission)
    expect(() => decodeOperationErrorControl(peerError(0x4fff))).toThrow(/inconsistent/i)
    expect(() => decodeOperationErrorControl(peerError(0x6000))).toThrow(/inconsistent/i)
    expect(() => decodeOperationErrorControl(
      peerError(PEER_OPERATION_CODE.timeout, true),
    )).toThrow(/permanent/i)
  })

  it('validates authenticated peer answer and candidate bounds', () => {
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.peerAnswer,
      encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'answer-sdp']),
    )).not.toThrow()
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.peerCandidate,
      encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'candidate', null, 0, null]),
    )).not.toThrow()
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.peerAnswer,
      encodeBody([4, identity(8), identity(9), 1n, 0, 0, '']),
    )).toThrow(/empty/i)
    expect(() => validateSenderControlBody(
      MESSAGE_KIND.peerCandidate,
      encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'candidate', null, 65_536, null]),
    )).toThrow(/unsigned 16-bit/i)
  })

  it('unwraps signed map and array semantics without changing their typed body', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(10)
    const binding = controlBinding(0n)
    const answerBody = encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'answer-sdp'])
    const candidateBody = encodeBody([
      4, identity(8), identity(9), 1n, 0, 0, 'candidate', 'data', 0, 'windshare',
    ])
    const catalogBody = encodeBody(new Map<number, unknown>([[0, 1], [1, identity(11)]]))
    const answer = await signSenderOperationControl({
      kind: MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: answerBody,
      binding,
      privateKey: keys.privateKey,
    })
    const candidate = await signSenderOperationControl({
      kind: MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: candidateBody,
      binding: { ...binding, sequence: 1n },
      privateKey: keys.privateKey,
    })
    const catalog = await signSenderOperationControl({
      kind: MESSAGE_KIND.catalogResult,
      operationId,
      semanticBody: catalogBody,
      binding: { ...binding, sequence: 2n },
      privateKey: keys.privateKey,
    })

    const verifiedAnswer = await verifySenderControl(
      answer.message,
      binding,
      createEd25519Verifier(keys.publicKey),
    )
    const verifiedCandidate = await verifySenderControl(
      candidate.message,
      { ...binding, sequence: 1n },
      createEd25519Verifier(keys.publicKey),
    )
    const verifiedCatalog = await verifySenderControl(
      catalog.message,
      { ...binding, sequence: 2n },
      createEd25519Verifier(keys.publicKey),
    )

    expect(verifiedAnswer).toEqual(answerBody)
    expect(decodePeerAnswer(verifiedAnswer).sdp).toBe('answer-sdp')
    expect(verifiedCandidate).toEqual(candidateBody)
    expect(decodePeerCandidate(verifiedCandidate).candidate).toBe('candidate')
    expect(verifiedCatalog).toEqual(catalogBody)
  })

  it('rejects signature, semantic type, delivery-axis, and replay substitution', async () => {
    const keys = await senderControlKeyPair()
    const operationId = identity(12)
    const binding = controlBinding(7n)
    const answerBody = encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'answer-sdp'])
    const signed = await signSenderOperationControl({
      kind: MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: answerBody,
      binding,
      privateKey: keys.privateKey,
    })

    const signatureChanged = replaceSignedField(signed.message, 255, changedSignature(signed.signature))
    await expect(verifySenderControl(signatureChanged, binding, createEd25519Verifier(keys.publicKey))).rejects
      .toThrow(/signature/i)
    const bodyChanged = replaceSignedField(
      signed.message,
      1,
      [4, identity(8), identity(9), 1n, 0, 0, 'other-answer'],
    )
    await expect(verifySenderControl(bodyChanged, binding, createEd25519Verifier(keys.publicKey))).rejects
      .toThrow(/signature/i)

    const wrongSemanticType = await signSenderOperationControl({
      kind: MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'candidate', null, 0, null]),
      binding,
      privateKey: keys.privateKey,
    })
    await expect(verifySenderControl(wrongSemanticType.message, binding, createEd25519Verifier(keys.publicKey))).rejects
      .toThrow(/bounded array|field count/i)
    const changedKind = encodeMessage(
      MESSAGE_KIND.peerCandidate,
      operationId,
      signed.message.body,
    )
    await expect(verifySenderControl(changedKind, binding, createEd25519Verifier(keys.publicKey))).rejects
      .toThrow(/signature/i)
    const changedOperation = encodeMessage(
      MESSAGE_KIND.peerAnswer,
      identity(13),
      signed.message.body,
    )
    await expect(verifySenderControl(changedOperation, binding, createEd25519Verifier(keys.publicKey))).rejects
      .toThrow(/signature/i)

    const changedBindings: readonly ControlBinding[] = [
      { ...binding, shareInstance: identity(14) },
      { ...binding, protocolSessionId: identity(15) },
      { ...binding, laneId: binding.laneId + 1 },
      { ...binding, laneEpoch: binding.laneEpoch + 1 },
      { ...binding, sequence: binding.sequence + 1n },
    ]
    for (const candidate of changedBindings) {
      await expect(verifySenderControl(signed.message, candidate, createEd25519Verifier(keys.publicKey))).rejects
        .toThrow(/signature/i)
    }
    await expect(verifySenderControl(
      signed.message,
      { ...binding, direction: 0 } as unknown as ControlBinding,
      createEd25519Verifier(keys.publicKey),
    )).rejects.toThrow(/delivery identity/i)
  })
})

function controlBinding(sequence: bigint): ControlBinding {
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
  message: ReturnType<typeof encodeMessage>,
  key: number,
  value: unknown,
): ReturnType<typeof encodeMessage> {
  const decoded = decodeCanonicalCbor(message.body, 65_536, 'signed control test wrapper')
  if (!(decoded instanceof Map)) throw new Error('signed control test wrapper is not a map')
  decoded.set(key, value)
  return encodeMessage(message.kind, message.operationId, encodeCanonicalCbor(decoded))
}

function changedSignature(signature: Uint8Array): Uint8Array<ArrayBuffer> {
  const changed = signature.slice()
  changed[0] = (changed[0] ?? 0) ^ 0x80
  return changed
}
