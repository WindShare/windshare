import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { decodePeerPathControl, encodePeerPathControl } from '../../src/protocol/peer-path-control'
import { decodeMessage, decodeOperationErrorControl, encodeBody, encodeMessage, peerFailureScope, MESSAGE_KIND, verifySenderControl } from '../../src/session/message'
import { OperationRouter } from '../../src/session/operation-router'

const vector = JSON.parse(readFileSync(new URL('../../../core/testvectors/v2-peer-signaling.json', import.meta.url), 'utf8')) as {
 senderPublicKeyB64: string
 controlBinding: { shareInstanceB64: string; protocolSessionIdB64: string; laneId: number; laneEpoch: number }
 failureScopes: readonly { code: number; scope: string }[]
 pathControls: readonly { kind: number; bodyB64: string; signedBodyB64: string; sequence: string }[]
}
const bytes = (value: string) => Uint8Array.from(Buffer.from(value, 'base64'))

it('uses the shared closed reason scopes, including unknown path-terminal namespace values', () => {
 for (const item of vector.failureScopes) {
  expect(peerFailureScope(item.code)).toBe(item.scope)
  const failure = decodeOperationErrorControl(encodeBody(new Map<number, unknown>([[0,3],[1,5],[2,item.code],[3,false],[4,null],[5,'peer reason'],[6,[new Uint8Array(16).fill(1),new Uint8Array(16).fill(2),1n,0]]])))
  expect(failure.code).toBe(item.code)
 }
})

it('verifies all Go-authored session-bound signed path controls and canonical bodies', async () => {
 for (const item of vector.pathControls) {
  const body = bytes(item.bodyB64)
  expect(encodePeerPathControl(decodePeerPathControl(body))).toEqual(body)
  const encoded = encodeMessage(MESSAGE_KIND.peerPathControl, undefined, bytes(item.signedBodyB64))
  // Transport delivery decodes the outer registry before verifying the signed body.
  const message = decodeMessage(encoded.plaintext)
  expect(await verifySenderControl(message, {
   shareInstance: bytes(vector.controlBinding.shareInstanceB64), protocolSessionId: bytes(vector.controlBinding.protocolSessionIdB64),
   laneId: vector.controlBinding.laneId, laneEpoch: vector.controlBinding.laneEpoch, direction: 1, sequence: BigInt(item.sequence),
  }, createEd25519Verifier(bytes(vector.senderPublicKeyB64)))).toEqual(body)
  expect(() => encodeMessage(MESSAGE_KIND.peerPathControl, new Uint8Array(16).fill(1), body)).toThrow()
 }
})

it('rejects overlong demand, unknown kind, zero sequence and non-demand holds', () => {
 const valid = decodePeerPathControl(bytes(vector.pathControls[0]!.bodyB64))
 for (const update of [{validForMilliseconds:120_001}, {controlSequence:0n}, {kind:5}, {kind:3}, {validForMilliseconds:0}, {holdForMilliseconds:-1}, {providerProfile:'x'.repeat(65)}, {providerProfile:'bad profile'}]) {
  expect(() => encodePeerPathControl({...valid,...update})).toThrow()
 }
 expect(() => decodePeerPathControl(encodeBody([1,1,1,1,1,1,1]))).toThrow()
})

it('routes path controls without a negotiation and stops after session termination', async () => {
 const router = new OperationRouter(() => undefined)
 const received: Uint8Array[] = []
 const unsubscribe = router.subscribePeerPathControls(body => received.push(body))
 const body = bytes(vector.pathControls[0]!.bodyB64)
 await router.route(encodeMessage(MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toEqual([body]); expect(router.active()).toEqual([])
 unsubscribe()
 await router.route(encodeMessage(MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toHaveLength(1)
 router.terminate(new Error('done'))
 await router.route(encodeMessage(MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toHaveLength(1)
})

it('seals known session-terminal reasons before a waiting peer consumer can retry', async () => {
 let terminated = false
 const router = new OperationRouter(() => { terminated = true })
 const id = new Uint8Array(16).fill(1)
 const operation = router.create(MESSAGE_KIND.peerOffer, encodeBody([4, id, id, 1n, 0, 0, 'v=0']))
 const reading = operation.next().catch(error => error as unknown)
 const failure = encodeMessage(MESSAGE_KIND.operationError,operation.id,encodeBody(new Map<number,unknown>([[0,3],[1,5],[2,0x500a],[3,false],[4,null],[5,'authentication'],[6,[id,id,1n,0]]])))
 await expect(router.route(failure)).rejects.toMatchObject({scope:'session'})
 expect(terminated).toBe(true)
 expect(await reading).toMatchObject({scope:'session'})
 expect(router.active()).toEqual([])
})
