import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { decodeV2PeerPathControl, encodeV2PeerPathControl } from '../../src/connectivity/v2-path-control-codec'
import { decodeV2OperationErrorControl, encodeV2Body, encodeV2Message, peerFailureScope, V2_MESSAGE_KIND, verifyV2SenderControl } from '../../src/session/v2-message'
import { V2OperationRouter } from '../../src/session/v2-operation-router'

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
  const failure = decodeV2OperationErrorControl(encodeV2Body(new Map<number, unknown>([[0,2],[1,5],[2,item.code],[3,false],[4,null],[5,'peer reason'],[6,[new Uint8Array(16).fill(1),new Uint8Array(16).fill(2),1n]]])))
  expect(failure.code).toBe(item.code)
 }
})

it('verifies all Go-authored session-bound signed path controls and canonical bodies', async () => {
 for (const item of vector.pathControls) {
  const body = bytes(item.bodyB64)
  expect(encodeV2PeerPathControl(decodeV2PeerPathControl(body))).toEqual(body)
  const message = encodeV2Message(V2_MESSAGE_KIND.peerPathControl, undefined, bytes(item.signedBodyB64))
  expect(await verifyV2SenderControl(message, {
   shareInstance: bytes(vector.controlBinding.shareInstanceB64), protocolSessionId: bytes(vector.controlBinding.protocolSessionIdB64),
   laneId: vector.controlBinding.laneId, laneEpoch: vector.controlBinding.laneEpoch, direction: 1, sequence: BigInt(item.sequence),
  }, bytes(vector.senderPublicKeyB64))).toEqual(body)
  expect(() => encodeV2Message(V2_MESSAGE_KIND.peerPathControl, new Uint8Array(16).fill(1), body)).toThrow()
 }
})

it('rejects overlong demand, unknown kind, zero sequence and non-demand holds', () => {
 const valid = decodeV2PeerPathControl(bytes(vector.pathControls[0]!.bodyB64))
 for (const update of [{validForMilliseconds:120_001}, {controlSequence:0n}, {kind:5}, {kind:3}, {validForMilliseconds:0}, {holdForMilliseconds:-1}, {providerProfile:'x'.repeat(65)}, {providerProfile:'bad profile'}]) {
  expect(() => encodeV2PeerPathControl({...valid,...update})).toThrow()
 }
 expect(() => decodeV2PeerPathControl(encodeV2Body([1,1,1,1,1,1,1]))).toThrow()
})

it('routes path controls without a negotiation and stops after session termination', async () => {
 const router = new V2OperationRouter(() => undefined)
 const received: Uint8Array[] = []
 const unsubscribe = router.subscribePeerPathControls(body => received.push(body))
 const body = bytes(vector.pathControls[0]!.bodyB64)
 await router.route(encodeV2Message(V2_MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toEqual([body]); expect(router.active()).toEqual([])
 unsubscribe()
 await router.route(encodeV2Message(V2_MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toHaveLength(1)
 router.terminate(new Error('done'))
 await router.route(encodeV2Message(V2_MESSAGE_KIND.peerPathControl, undefined, body))
 expect(received).toHaveLength(1)
})

it('seals known session-terminal reasons before a waiting peer consumer can retry', async () => {
 let terminated = false
 const router = new V2OperationRouter(() => { terminated = true })
 const id = new Uint8Array(16).fill(1)
 const operation = router.create(id, V2_MESSAGE_KIND.peerOffer, encodeV2Body([2,id,id,1n,'v=0']))
 const reading = operation.next().catch(error => error as unknown)
 const failure = encodeV2Message(V2_MESSAGE_KIND.operationError,id,encodeV2Body(new Map<number,unknown>([[0,2],[1,5],[2,0x500a],[3,false],[4,null],[5,'authentication'],[6,[id,id,1n]]])))
 await expect(router.route(failure)).rejects.toMatchObject({scope:'session'})
 expect(terminated).toBe(true)
 expect(await reading).toMatchObject({scope:'session'})
 expect(router.active()).toEqual([])
})
