import { expect, it } from 'vitest'
import { encodeBlockRequest } from '../../src/content/flow'
import { decodeMessage, encodeBody, encodeMessage, MESSAGE_KIND } from '../../src/session/message'
import { OperationRouter } from '../../src/session/operation-router'
import { SessionRuntimeError } from '../../src/session/runtime-types'
import { fragmentRecord } from '../content/v2-fragment-fixture'

it('retains a final response when physical input ends before the consumer reads it', async () => {
  const router = new OperationRouter(() => undefined)
  const operation = router.create(MESSAGE_KIND.listChildren, encodeBody([]))
  const final = encodeMessage(MESSAGE_KIND.catalogResult, operation.id, encodeBody([]))
  await router.route(final)
  let drained = false
  const ending = operation.endInput(new SessionRuntimeError('lane', 'lane ended')).then(() => { drained = true })
  await Promise.resolve()
  expect(drained).toBe(false)
  await expect(operation.next()).resolves.toEqual(final)
  await ending
  expect(operation.settlement).toBe('remote_final')
  router.terminate(new Error('test closed'))
})

it('delivers queued fragments before reporting incomplete input and allows explicit cancellation during drain', async () => {
  const router = new OperationRouter(() => undefined)
  const request = encodeBlockRequest(new Uint8Array(16).fill(1), [0n])
  const failure = new SessionRuntimeError('lane', 'lane ended')
  for (const cancel of [false, true]) {
    const operation = router.create(MESSAGE_KIND.requestBlocks, request)
    const fragment = decodeMessage(fragmentRecord(operation.id, Uint8Array.of(7))[0]!)
    await router.route(fragment)
    const ending = operation.endInput(failure)
    if (cancel) operation.cancel(new Error('user stopped'))
    else await expect(operation.next()).resolves.toEqual(fragment)
    await ending
    await expect(operation.next()).rejects.toThrow(cancel ? 'user stopped' : 'lane ended')
  }
  router.terminate(new Error('test closed'))
})
