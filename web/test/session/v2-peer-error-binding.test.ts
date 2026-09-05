import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { decodeV2OperationErrorControl, encodeV2Body, encodeV2Message, V2_MESSAGE_KIND, verifyV2SenderControl } from '../../src/session/v2-message'
import { V2OperationRouter, V2_OPERATION_TOMBSTONE_MILLISECONDS } from '../../src/session/v2-operation-router'

const vector = JSON.parse(readFileSync(new URL('../../../core/testvectors/v2-peer-signaling.json', import.meta.url), 'utf8')) as {
 senderPublicKeyB64: string; peerPathIdB64: string; attemptIdB64: string
 controlBinding: { shareInstanceB64: string; protocolSessionIdB64: string; operationIdB64: string; laneId: number; laneEpoch: number }
 peerErrors: readonly { code: number; bodyB64: string; signedBodyB64: string; sequence: string }[]
}
const bytes = (value: string) => Uint8Array.from(Buffer.from(value, 'base64'))
const path = bytes(vector.peerPathIdB64)
const attempt = bytes(vector.attemptIdB64)
const operationId = bytes(vector.controlBinding.operationIdB64)

async function authenticated(item: typeof vector.peerErrors[number]) {
 const wire = encodeV2Message(V2_MESSAGE_KIND.operationError, operationId, bytes(item.signedBodyB64))
 const body = await verifyV2SenderControl(wire, {
  shareInstance: bytes(vector.controlBinding.shareInstanceB64),
  protocolSessionId: bytes(vector.controlBinding.protocolSessionIdB64),
  laneId: vector.controlBinding.laneId, laneEpoch: vector.controlBinding.laneEpoch,
  direction: 1, sequence: BigInt(item.sequence),
 }, bytes(vector.senderPublicKeyB64))
 expect(body).toEqual(bytes(item.bodyB64))
 return encodeV2Message(V2_MESSAGE_KIND.operationError, operationId, body)
}

it('verifies Go signed peer errors and confines delayed terminal codes after tombstone GC', async () => {
 for (const item of vector.peerErrors) {
  let now = 0
  let terminal = false
  const router = new V2OperationRouter(() => { terminal = true }, () => now)
  router.create(operationId, V2_MESSAGE_KIND.peerOffer, encodeV2Body([2, path, attempt, 1n, 'v=0'])).close()
  now = V2_OPERATION_TOMBSTONE_MILLISECONDS + 1
  const currentId = new Uint8Array(16).fill(70)
  const current = router.create(currentId, V2_MESSAGE_KIND.peerOffer, encodeV2Body([2, path, currentId, 2n, 'v=0']))
  const message = await authenticated(item)
  expect(decodeV2OperationErrorControl(message.body)).toMatchObject({
   code: item.code, peerAttempt: { peerPathId: path, attemptId: attempt, attemptSequence: 1n },
  })
  await expect(router.route(message, 9, 1)).resolves.toBeUndefined()
  expect(terminal).toBe(false)
  expect(router.active()).toEqual([current])
  await expect(router.route(encodeV2Message(V2_MESSAGE_KIND.operationError, currentId, message.body)))
   .rejects.toMatchObject({ scope: 'session' })
 }
})

it('rejects missing, malformed, zero, overflowing, and cross-scope peer identities', () => {
 const valid = new Map<number, unknown>([[0,2],[1,5],[2,0x5002],[3,false],[4,null],[5,'expired'],[6,[path,attempt,1n]]])
 for (const binding of [null, [], [path, attempt], [path, attempt, 0n], [path, attempt, 1n << 64n], [new Uint8Array(16), attempt, 1n], [path, new Uint8Array(15), 1n]]) {
  const fields = new Map(valid); fields.set(6,binding)
  expect(() => decodeV2OperationErrorControl(encodeV2Body(fields))).toThrow()
 }
 for (const mutate of [
  (fields: Map<number,unknown>) => { fields.delete(6) },
  (fields: Map<number,unknown>) => { fields.set(0,1) },
  (fields: Map<number,unknown>) => { fields.set(1,4);fields.set(2,0x4001) },
  (fields: Map<number,unknown>) => { fields.set(7,null) },
 ]) {
  const fields = new Map(valid); mutate(fields)
  expect(() => decodeV2OperationErrorControl(encodeV2Body(fields))).toThrow()
 }
})
