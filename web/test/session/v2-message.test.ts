import { describe, expect, it } from 'vitest'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'

import { decodePeerAnswer, decodePeerCandidate } from '../../src/connectivity/signaling-codec'

import {
	decodeOperationErrorControl,
	encodeMessage,
  encodeBody,
  decodeMessage,

  validateSenderControlBody,

  MESSAGE_KIND,
  PEER_OPERATION_CODE,
} from '../../src/session/message'
import { senderOperationControl } from './signed-control-fixture'

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

describe('v2 session wire registry', () => {
  it.each(Object.entries(MESSAGE_KIND).filter(([, kind]) => kind !== MESSAGE_KIND.blockFragment))(
    'decodes the registered %s control message', (_name, kind) => {
      const sessionControl = kind === MESSAGE_KIND.sessionTerminal || kind === MESSAGE_KIND.peerPathControl
      const encoded = encodeMessage(kind, sessionControl ? undefined : 1n, encodeBody(null))
      expect(decodeMessage(encoded.plaintext)).toEqual(encoded)
    },
  )

  it.each([0n, 20n, 21n, 255n, 0xffffffffffffffffn, BigInt(MESSAGE_KIND.blockFragment)])(
    'rejects unknown kinds and the data codec on the control path: %s', kind => {
      const plaintext = encodeCanonicalCbor(new Map<number, unknown>([[0, kind], [1, null], [2, null]]))
      expect(() => decodeMessage(plaintext)).toThrow('unknown or uses the wrong codec')
    },
  )
})

describe('v2 authenticated sender control schemas', () => {
  it('preserves a BOM-only operation failure message through the session envelope', () => {
    const body = encodeBody(new Map<number, unknown>([
      [0, 1], [1, 4], [2, 0x4001], [3, false], [4, null], [5, '\ufeff'],
    ]))
    const message = decodeMessage(encodeMessage(MESSAGE_KIND.operationError, 1n, body).plaintext)
    expect(message.body).toEqual(body)
    expect(() => validateSenderControlBody(message.kind, message.body)).not.toThrow()
    expect(decodeOperationErrorControl(message.body).message).toBe('\ufeff')
  })

  it('accepts a BOM-only session terminal message without treating it as empty', () => {
    const body = encodeBody(new Map<number, unknown>([[0, 1], [1, 0x1001], [2, '\ufeff']]))
    const message = decodeMessage(encodeMessage(MESSAGE_KIND.sessionTerminal, undefined, body).plaintext)
    expect(message.body).toEqual(body)
    expect(() => validateSenderControlBody(message.kind, message.body)).not.toThrow()
  })

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

  it('rejects invalid revision retry delays outside 1 through 30000 milliseconds', async () => {

    const operationId = 2n

    const openResult = async (retryAfterMilliseconds: number) => senderOperationControl({
      kind: MESSAGE_KIND.openResults,
      operationId,
      semanticBody: encodeBody(new Map<number, unknown>([
        [0, 1],
        [1, [[identity(3), 1, 0x3001, true, retryAfterMilliseconds]]],
      ])),
    })

    for (const accepted of [1, 30_000]) {
      const result = await openResult(accepted)
      await expect(validatedBody(result.message)).resolves.toBeDefined()
    }
    for (const rejected of [0, 30_001]) {
      const result = await openResult(rejected)
      await expect(validatedBody(result.message))
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

  it('preserves map and array semantics without changing their typed body', async () => {

    const operationId = 10n

    const answerBody = encodeBody([4, identity(8), identity(9), 1n, 0, 0, 'answer-sdp'])
    const candidateBody = encodeBody([
      4, identity(8), identity(9), 1n, 0, 0, 'candidate', 'data', 0, 'windshare',
    ])
    const catalogBody = encodeBody(new Map<number, unknown>([[0, 1], [1, identity(11)]]))
    const answer = senderOperationControl({
      kind: MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: answerBody,
    })
    const candidate = senderOperationControl({
      kind: MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: candidateBody,
    })
    const catalog = senderOperationControl({
      kind: MESSAGE_KIND.catalogResult,
      operationId,
      semanticBody: catalogBody,
    })

    const verifiedAnswer = await validatedBody(
      answer.message,
    )
    const verifiedCandidate = await validatedBody(
      candidate.message,
    )
    const verifiedCatalog = await validatedBody(
      catalog.message,
    )

    expect(verifiedAnswer).toEqual(answerBody)
    expect(decodePeerAnswer(verifiedAnswer).sdp).toBe('answer-sdp')
    expect(verifiedCandidate).toEqual(candidateBody)
    expect(decodePeerCandidate(verifiedCandidate).candidate).toBe('candidate')
    expect(verifiedCatalog).toEqual(catalogBody)
  })

})


async function validatedBody(message: ReturnType<typeof encodeMessage>) {
  validateSenderControlBody(message.kind, message.body)
  return message.body
}
