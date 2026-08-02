import { finished } from 'node:stream/promises'

import { createOwnedEventChannel } from './owned-process-channel.mjs'
import { parseTestIdentity } from './test-identity.mjs'

export const TEST_EVENT_SCHEMA_VERSION = 'windshare.test-event/v1'

const MAXIMUM_EVENT_BYTES = 1_048_576
const MAXIMUM_EVENT_FIELD_BYTES = 256
const DEFAULT_MAXIMUM_EVENTS = 1_024
const EVENT_OUTCOMES = Object.freeze(['started', 'succeeded', 'failed'])
const EVENT_FIELD_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$/u

export function createTestEventDecoder({
  identity,
  minimumEvents = 0,
  maximumEvents = DEFAULT_MAXIMUM_EVENTS,
}) {
  const parsedIdentity = parseTestIdentity(identity)
  requireEventCount(minimumEvents, 'minimum private test-event count', true)
  requireEventCount(maximumEvents, 'maximum private test-event count', false)
  if (minimumEvents > maximumEvents) {
    throw new Error('minimum private test-event count exceeds its maximum')
  }
  const state = new TestEventDecoder(parsedIdentity, minimumEvents, maximumEvents)
  return Object.freeze({
    events: state.events,
    push: (chunk) => state.push(chunk),
    fail: (cause) => state.fail(cause),
    finish: () => state.finish(),
  })
}

export function drainTestEventStream(stream, options) {
  if (stream === null || typeof stream?.on !== 'function') {
    throw new Error('private test-event stream is unavailable')
  }
  const decoder = createTestEventDecoder(options)
  const consume = (chunk) => decoder.push(Buffer.from(chunk))
  stream.on('data', consume)
  const completion = finished(stream, { cleanup: true }).then(
    () => decoder.finish(),
    (cause) => {
      decoder.fail(new Error('private test-event stream failed', { cause }))
      decoder.finish()
    },
  ).finally(() => stream.off('data', consume))
  return Object.freeze({ events: decoder.events, completion })
}

export function parseTestEvent(value, identity) {
  const parsedIdentity = parseTestIdentity(identity)
  requireRecord(value, 'private test event')
  rejectUnknownKeys(value, [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'component', 'milestone', 'outcome',
    'payload',
  ], 'private test event')
  for (const key of [
    'schema_version', 'run_id', 'operation_id', 'scenario', 'component', 'milestone', 'outcome',
  ]) {
    if (!Object.hasOwn(value, key)) throw new Error('private test event is missing a required field')
  }
  if (
    value.schema_version !== TEST_EVENT_SCHEMA_VERSION || value.run_id !== parsedIdentity.runId ||
    value.operation_id !== parsedIdentity.operationId || value.scenario !== parsedIdentity.scenario
  ) throw new Error('private test event identity differs from its child process')
  const component = requireEventField(value.component, 'event component')
  const milestone = requireEventField(value.milestone, 'event milestone')
  if (!EVENT_OUTCOMES.includes(value.outcome)) {
    throw new Error('private test event outcome is unsupported')
  }
  const event = {
    schemaVersion: TEST_EVENT_SCHEMA_VERSION,
    runId: parsedIdentity.runId,
    operationId: parsedIdentity.operationId,
    scenario: parsedIdentity.scenario,
    component,
    milestone,
    outcome: value.outcome,
    ...(Object.hasOwn(value, 'payload') ? { payload: deepFreeze(value.payload) } : {}),
  }
  return Object.freeze(event)
}

class TestEventDecoder {
  #buffer = Buffer.alloc(0)
  #eventCount = 0
  #failure
  #finished = false
  #events

  constructor(identity, minimumEvents, maximumEvents) {
    this.identity = identity
    this.minimumEvents = minimumEvents
    this.maximumEvents = maximumEvents
    this.#events = createOwnedEventChannel(maximumEvents, 'private test events')
    this.events = this.#events.view
  }

  push(chunk) {
    if (this.#finished) throw new Error('private test-event decoder is already finished')
    if (!(chunk instanceof Uint8Array)) throw new Error('private test-event chunk must be bytes')
    if (this.#failure !== undefined || chunk.byteLength === 0) return
    this.#buffer = Buffer.concat([this.#buffer, Buffer.from(chunk)])
    while (true) {
      const terminator = this.#buffer.indexOf(0x0a)
      if (terminator < 0) break
      const line = Buffer.from(this.#buffer.subarray(0, terminator))
      this.#buffer = Buffer.from(this.#buffer.subarray(terminator + 1))
      if (line.byteLength < 1 || line.byteLength > MAXIMUM_EVENT_BYTES) {
        this.fail(new Error('private test-event line is empty or oversized'))
        return
      }
      this.#eventCount += 1
      if (this.#eventCount > this.maximumEvents) {
        this.fail(new Error('private test-event stream contains repeated or trailing events'))
        return
      }
      try {
        this.#events.append(parseTestEvent(decodeCanonicalJson(line), this.identity))
      } catch (cause) {
        this.fail(new Error('private test event is invalid', { cause }))
        return
      }
    }
    if (this.#buffer.byteLength > MAXIMUM_EVENT_BYTES) {
      this.fail(new Error('private test-event line exceeds its byte limit'))
    }
  }

  fail(cause) {
    if (this.#failure !== undefined) return
    this.#failure = cause instanceof Error ? cause : new Error(String(cause))
    this.#buffer = Buffer.alloc(0)
    this.#events.fail(this.#failure)
  }

  finish() {
    if (this.#finished) throw new Error('private test-event decoder was finished more than once')
    this.#finished = true
    this.#events.finish()
    if (this.#failure !== undefined) throw this.#failure
    if (this.#buffer.byteLength !== 0) {
      const failure = new Error('private test-event stream ended with a truncated line')
      this.fail(failure)
      throw failure
    }
    if (this.#eventCount < this.minimumEvents) {
      const failure = new Error('private test-event stream ended before its required event')
      this.fail(failure)
      throw failure
    }
    return this.#eventCount
  }
}

function decodeCanonicalJson(bytes) {
  let encoded
  try {
    encoded = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error('private test event is not UTF-8', { cause })
  }
  let value
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('private test event is not JSON', { cause })
  }
  if (JSON.stringify(value) !== encoded) {
    throw new Error('private test event is not canonical JSON')
  }
  return value
}

function requireEventField(value, label) {
  if (
    typeof value !== 'string' || value.normalize('NFC') !== value ||
    Buffer.byteLength(value, 'utf8') < 1 || Buffer.byteLength(value, 'utf8') > MAXIMUM_EVENT_FIELD_BYTES ||
    !EVENT_FIELD_PATTERN.test(value)
  ) throw new Error(`private test ${label} is invalid`)
  return value
}

function deepFreeze(value, visited = new Set()) {
  if (value === null || typeof value !== 'object' || visited.has(value)) return value
  visited.add(value)
  for (const nested of Object.values(value)) deepFreeze(nested, visited)
  return Object.freeze(value)
}

function requireEventCount(value, label, allowZero) {
  if (!Number.isSafeInteger(value) || value < (allowZero ? 0 : 1)) {
    throw new Error(`${label} is invalid`)
  }
}

function requireRecord(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
}

function rejectUnknownKeys(value, keys, label) {
  const allowed = new Set(keys)
  if (Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error(`${label} contains unknown keys`)
  }
}
