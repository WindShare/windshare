import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
import { decodeOperationErrorControl, encodeBody, encodeMessage, MESSAGE_KIND, verifySenderControl } from '../../src/session/message'
import { OperationRouter, OPERATION_TOMBSTONE_MILLISECONDS } from '../../src/session/operation-router'

const vector = JSON.parse(readFileSync(new URL('../../../core/testvectors/v2-peer-signaling.json', import.meta.url), 'utf8')) as {
 senderPublicKeyB64: string; peerPathIdB64: string; attemptIdB64: string; peerSlot: number
 controlBinding: { shareInstanceB64: string; protocolSessionIdB64: string; operationIdB64: string; laneId: number; laneEpoch: number }
 peerErrors: readonly { code: number; bodyB64: string; signedBodyB64: string; sequence: string }[]
}
const bytes = (value: string) => Uint8Array.from(Buffer.from(value, 'base64'))
const path = bytes(vector.peerPathIdB64)
const attempt = bytes(vector.attemptIdB64)
const operationId = bytes(vector.controlBinding.operationIdB64)

async function authenticated(item: typeof vector.peerErrors[number]) {
 const wire = encodeMessage(MESSAGE_KIND.operationError, operationId, bytes(item.signedBodyB64))
 const body = await verifySenderControl(wire, {
  shareInstance: bytes(vector.controlBinding.shareInstanceB64),
  protocolSessionId: bytes(vector.controlBinding.protocolSessionIdB64),
  laneId: vector.controlBinding.laneId, laneEpoch: vector.controlBinding.laneEpoch,
  direction: 1, sequence: BigInt(item.sequence),
 }, createEd25519Verifier(bytes(vector.senderPublicKeyB64)))
 expect(body).toEqual(bytes(item.bodyB64))
 return encodeMessage(MESSAGE_KIND.operationError, operationId, body)
}

it('verifies Go signed peer errors and confines delayed terminal codes after tombstone GC', async () => {
 for (const item of vector.peerErrors) {
  let now = 0
  let terminal = false
  const router = new OperationRouter(() => { terminal = true }, () => now)
  router.create(MESSAGE_KIND.peerOffer, encodeBody([4, path, attempt, 1n, vector.peerSlot, 0, 'v=0'])).close()
  now = OPERATION_TOMBSTONE_MILLISECONDS + 1
  const currentId = new Uint8Array(16).fill(70)
  const current = router.create(MESSAGE_KIND.peerOffer, encodeBody([4, path, currentId, 2n, vector.peerSlot, 0, 'v=0']))
  const message = await authenticated(item)
  expect(decodeOperationErrorControl(message.body)).toMatchObject({
   code: item.code, peerAttempt: { peerPathId: path, attemptId: attempt, attemptSequence: 1n, peerSlot: vector.peerSlot },
  })
  await expect(router.route(message, 9, 1)).resolves.toBeUndefined()
  expect(terminal).toBe(false)
  expect(router.active()).toEqual([current])
  await expect(router.route(encodeMessage(MESSAGE_KIND.operationError, current.id, message.body)))
   .rejects.toMatchObject({ scope: 'session' })
 }
})

it('rejects missing, malformed, zero, overflowing, and cross-scope peer identities', () => {
 const valid = new Map<number, unknown>([[0,3],[1,5],[2,0x5002],[3,false],[4,null],[5,'expired'],[6,[path,attempt,1n,0]]])
 for (const binding of [null, [], [path, attempt], [path, attempt, 1n], [path, attempt, 0n, 0], [path, attempt, 1n << 64n, 0], [new Uint8Array(16), attempt, 1n, 0], [path, new Uint8Array(15), 1n, 0], [path, attempt, 1n, 4], [path, attempt, 1n, -1]]) {
  const fields = new Map(valid); fields.set(6,binding)
  expect(() => decodeOperationErrorControl(encodeBody(fields))).toThrow()
 }
 for (const mutate of [
  (fields: Map<number,unknown>) => { fields.delete(6) },
  (fields: Map<number,unknown>) => { fields.set(0,1) },
  (fields: Map<number,unknown>) => { fields.set(1,4);fields.set(2,0x4001) },
  (fields: Map<number,unknown>) => { fields.set(7,null) },
 ]) {
  const fields = new Map(valid); mutate(fields)
  expect(() => decodeOperationErrorControl(encodeBody(fields))).toThrow()
 }
})
