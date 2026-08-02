import { PassThrough } from 'node:stream'

import { describe, expect, it } from 'vitest'

import {
  createTestEventDecoder,
  drainTestEventStream,
} from '../../scripts/browser-evidence/process/test-event-channel.mjs'

const IDENTITY = Object.freeze({
  runId: 'event-channel-run',
  operationId: 'event-channel-operation',
  scenario: 'event-channel/scenario',
})

function encodedEvent(overrides: Readonly<Record<string, unknown>> = {}): Buffer {
  return Buffer.from(`${JSON.stringify({
    schema_version: 'windshare.test-event/v1',
    run_id: IDENTITY.runId,
    operation_id: IDENTITY.operationId,
    scenario: IDENTITY.scenario,
    component: 'fixture_process',
    milestone: 'listener_ready',
    outcome: 'succeeded',
    payload: { address: '127.0.0.1:49152' },
    ...overrides,
  })}\n`, 'utf8')
}

describe('private test-event channel', () => {
  it('decodes fragmented canonical JSONL and deeply freezes admitted evidence', () => {
    const decoder = createTestEventDecoder({
      identity: IDENTITY,
      minimumEvents: 1,
      maximumEvents: 1,
    })
    const encoded = encodedEvent()

    decoder.push(encoded.subarray(0, 11))
    decoder.push(encoded.subarray(11))

    expect(decoder.finish()).toBe(1)
    const events = decoder.events.snapshot().events
    expect(events).toHaveLength(1)
    expect(events[0]).toMatchObject({
      ...IDENTITY,
      component: 'fixture_process',
      milestone: 'listener_ready',
      outcome: 'succeeded',
    })
    expect(Object.isFrozen(events[0])).toBe(true)
    expect(Object.isFrozen(events[0]?.payload)).toBe(true)
  })

  it('fails closed on identity mismatch before publishing the first event', () => {
    const decoder = createTestEventDecoder({
      identity: IDENTITY,
      minimumEvents: 1,
    })

    decoder.push(encodedEvent({ operation_id: 'different-operation' }))

    expect(() => decoder.finish()).toThrow('private test event is invalid')
    expect(decoder.events.snapshot().events).toHaveLength(0)
  })

  it.each([
    ['early EOF', Buffer.alloc(0), 'ended before its required event'],
    ['truncated event', encodedEvent().subarray(0, -1), 'truncated line'],
    ['empty trailing record', Buffer.concat([encodedEvent(), Buffer.from('\n')]), 'empty or oversized'],
  ])('rejects %s', (_label, bytes, expected) => {
    const decoder = createTestEventDecoder({
      identity: IDENTITY,
      minimumEvents: 1,
    })
    decoder.push(bytes)
    expect(() => decoder.finish()).toThrow(expected)
  })

  it('rejects a repeated event when the endpoint is one-shot', () => {
    const decoder = createTestEventDecoder({
      identity: IDENTITY,
      minimumEvents: 1,
      maximumEvents: 1,
    })

    decoder.push(Buffer.concat([encodedEvent(), encodedEvent()]))

    expect(() => decoder.finish()).toThrow('repeated or trailing events')
  })

  it('joins stream EOF only after every event byte is consumed', async () => {
    const stream = new PassThrough()
    const drained = drainTestEventStream(stream, {
      identity: IDENTITY,
      minimumEvents: 1,
      maximumEvents: 1,
    })
    const firstEvent = drained.events[Symbol.asyncIterator]().next()

    stream.write(encodedEvent().subarray(0, 7))
    stream.end(encodedEvent().subarray(7))

    await expect(drained.completion).resolves.toBe(1)
    await expect(firstEvent).resolves.toMatchObject({
      done: false,
      value: { milestone: 'listener_ready' },
    })
  })
})
