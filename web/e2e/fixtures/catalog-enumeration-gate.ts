import { connect } from 'node:net'

import type { TestEvent } from '../../scripts/browser-evidence/process/test-event-channel.mjs'
import {
  parseTestIdentity,
  type TestIdentity,
} from '../../scripts/browser-evidence/process/test-identity.mjs'

const CONTROL_SCHEMA_VERSION = 'windshare.catalog-enumeration-gate-control/v1'
const RESPONSE_SCHEMA_VERSION = 'windshare.catalog-enumeration-gate-response/v1'
const SNAPSHOT_ACTION = 'snapshot'
const RELEASE_ACTION = 'release'
const OBSERVED_OUTCOME = 'observed'
const RELEASED_OUTCOME = 'released'
const MAXIMUM_CONTROL_BYTES = 1_024
const CONTROL_TIMEOUT_MILLISECONDS = 5_000
const LOOPBACK_ADDRESS_PATTERN = /^127\.0\.0\.1:([1-9]\d{0,4})$/u
const MAXIMUM_TCP_PORT = 65_535

type CatalogGateAction = typeof SNAPSHOT_ACTION | typeof RELEASE_ACTION

export interface CatalogGateControlBounds {
  readonly signal?: AbortSignal
  readonly timeoutMilliseconds?: number
}

export interface CatalogGateSnapshot {
  readonly blockedRequests: number
  readonly released: boolean
}

interface CapturedOutputProbe {
  capturedOutputContains(value: string): boolean
}

interface CatalogGateControlResponse {
  readonly schemaVersion: typeof RESPONSE_SCHEMA_VERSION
  readonly action: CatalogGateAction
  readonly outcome: typeof OBSERVED_OUTCOME | typeof RELEASED_OUTCOME
  readonly blockedRequests: number
  readonly released: boolean
}

/**
 * Retains the listener address as an owner-only capability. Callers can inspect
 * gate state and assert non-disclosure, but cannot copy the address into a page,
 * command line, environment, or artifact.
 */
export class CatalogEnumerationGateController {
  readonly #port: number
  readonly #address: string
  readonly #identity: TestIdentity
  #operationInFlight = false
  #releaseAttempted = false

  private constructor(address: string, port: number, identity: TestIdentity) {
    this.#address = address
    this.#port = port
    this.#identity = identity
  }

  static fromPrivateReadyEvent(event: TestEvent): CatalogEnumerationGateController {
    const address = privateListenerAddress(event.payload)
    const match = LOOPBACK_ADDRESS_PATTERN.exec(address)
    const port = match === null ? Number.NaN : Number(match[1])
    if (!Number.isSafeInteger(port) || port < 1 || port > MAXIMUM_TCP_PORT ||
        '127.0.0.1:' + String(port) !== address) {
      throw new Error('Catalog gate private readiness address is invalid')
    }
    const identity = parseTestIdentity({
      runId: event.runId,
      operationId: event.operationId,
      scenario: event.scenario,
    })
    return new CatalogEnumerationGateController(address, port, identity)
  }

  async snapshot(bounds: CatalogGateControlBounds = {}): Promise<CatalogGateSnapshot> {
    if (this.#releaseAttempted) {
      throw new Error('Catalog gate snapshot is unavailable after release admission')
    }
    const response = await this.#exchange(SNAPSHOT_ACTION, bounds)
    if (response.outcome !== OBSERVED_OUTCOME || response.released) {
      throw new Error('Catalog gate snapshot response is semantically invalid')
    }
    return Object.freeze({
      blockedRequests: response.blockedRequests,
      released: response.released,
    })
  }

  async assertBlocked(bounds: CatalogGateControlBounds = {}): Promise<CatalogGateSnapshot> {
    const snapshot = await this.snapshot(bounds)
    if (snapshot.blockedRequests < 1) {
      throw new Error('Catalog gate did not observe a blocked enumeration request')
    }
    return snapshot
  }

  async release(bounds: CatalogGateControlBounds = {}): Promise<CatalogGateSnapshot> {
    if (this.#releaseAttempted) {
      throw new Error('Catalog gate release is single-use')
    }
    // Retiring the local capability before I/O makes an ambiguous lost response
    // fail closed instead of authorizing a replay against a possibly released gate.
    this.#releaseAttempted = true
    const response = await this.#exchange(RELEASE_ACTION, bounds)
    if (response.outcome !== RELEASED_OUTCOME ||
        response.blockedRequests < 1 || !response.released) {
      throw new Error('Catalog gate release response is semantically invalid')
    }
    return Object.freeze({
      blockedRequests: response.blockedRequests,
      released: response.released,
    })
  }

  assertNotCapturedBy(probe: CapturedOutputProbe): void {
    if (probe.capturedOutputContains(this.#address)) {
      throw new Error('Catalog gate capability entered child process output')
    }
  }

  assertNotDisclosed(label: string, material: string | Uint8Array): void {
    const encoded = typeof material === 'string'
      ? material
      : Buffer.from(material).toString('utf8')
    if (encoded.includes(this.#address)) {
      throw new Error('Catalog gate capability entered ' + label)
    }
  }

  async #exchange(
    action: CatalogGateAction,
    bounds: CatalogGateControlBounds,
  ): Promise<CatalogGateControlResponse> {
    if (this.#operationInFlight) {
      throw new Error('Catalog gate control operations must be serialized')
    }
    this.#operationInFlight = true
    try {
      const request = Buffer.from(JSON.stringify({
        schema_version: CONTROL_SCHEMA_VERSION,
        run_id: this.#identity.runId,
        operation_id: this.#identity.operationId,
        scenario: this.#identity.scenario,
        action,
      }) + '\n', 'utf8')
      if (request.byteLength > MAXIMUM_CONTROL_BYTES) {
        throw new Error('Catalog gate control request exceeds its byte limit')
      }
      const timeoutMilliseconds = boundedControlTimeout(bounds.timeoutMilliseconds)
      const response = await exchangeHalfClosed(
        this.#port,
        request,
        timeoutMilliseconds,
        bounds.signal,
      )
      return parseControlResponse(response, action)
    } finally {
      this.#operationInFlight = false
    }
  }
}

function privateListenerAddress(payload: unknown): string {
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload) ||
      Object.keys(payload).length !== 1 || !Object.hasOwn(payload, 'address')) {
    throw new Error('Catalog gate private readiness payload is invalid')
  }
  const address = (payload as { readonly address?: unknown }).address
  if (typeof address !== 'string' || address.length === 0) {
    throw new Error('Catalog gate private readiness payload has no address')
  }
  return address
}

function boundedControlTimeout(value: number | undefined): number {
  const timeout = value ?? CONTROL_TIMEOUT_MILLISECONDS
  if (!Number.isSafeInteger(timeout) || timeout < 1) {
    throw new RangeError('Catalog gate control timeout must be a positive integer')
  }
  return Math.min(timeout, CONTROL_TIMEOUT_MILLISECONDS)
}

function exchangeHalfClosed(
  port: number,
  request: Uint8Array,
  timeoutMilliseconds: number,
  signal?: AbortSignal,
): Promise<Uint8Array> {
  if (signal?.aborted === true) {
    return Promise.reject(new Error('Catalog gate control was aborted'))
  }
  return new Promise<Uint8Array>((resolveResponse, rejectResponse) => {
    const socket = connect({ host: '127.0.0.1', port })
    const chunks: Buffer[] = []
    let receivedBytes = 0
    let settled = false

    const cleanup = () => {
      clearTimeout(timeout)
      signal?.removeEventListener('abort', aborted)
    }
    const settle = (operation: () => void) => {
      if (settled) return
      settled = true
      cleanup()
      operation()
    }
    const fail = (message: string) => {
      settle(() => {
        socket.destroy()
        rejectResponse(new Error(message))
      })
    }
    const aborted = () => fail('Catalog gate control was aborted')
    const timeout = setTimeout(
      () => fail('Catalog gate control reached its deadline'),
      timeoutMilliseconds,
    )

    socket.once('connect', () => {
      // EOF, rather than a timing heuristic, is the request commit boundary.
      socket.end(request)
    })
    socket.on('data', (chunk: Buffer) => {
      if (settled) return
      receivedBytes += chunk.byteLength
      if (receivedBytes > MAXIMUM_CONTROL_BYTES) {
        fail('Catalog gate control response exceeds its byte limit')
        return
      }
      chunks.push(chunk)
    })
    socket.once('end', () => {
      if (receivedBytes === 0) {
        fail('Catalog gate control returned no response')
        return
      }
      settle(() => {
        socket.destroy()
        resolveResponse(Buffer.concat(chunks, receivedBytes))
      })
    })
    socket.once('error', () => fail('Catalog gate control connection failed'))
    socket.once('close', () => {
      if (!settled) fail('Catalog gate control connection closed without EOF')
    })
    signal?.addEventListener('abort', aborted, { once: true })
    if (signal?.aborted === true) aborted()
  })
}

function parseControlResponse(
  encoded: Uint8Array,
  expectedAction: CatalogGateAction,
): CatalogGateControlResponse {
  const text = Buffer.from(encoded).toString('utf8')
  if (text.length < 2 || !text.endsWith('\n') || text.slice(0, -1).includes('\n')) {
    throw new Error('Catalog gate control response framing is invalid')
  }
  let value: unknown
  try {
    value = JSON.parse(text.slice(0, -1)) as unknown
  } catch {
    throw new Error('Catalog gate control response is invalid JSON')
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('Catalog gate control response must be an object')
  }
  const fields = value as Record<string, unknown>
  const keys = Object.keys(fields)
  const expectedKeys = [
    'schema_version',
    'action',
    'outcome',
    'blocked_requests',
    'released',
  ] as const
  if (keys.length !== expectedKeys.length ||
      !keys.every((key, index) => key === expectedKeys[index])) {
    throw new Error('Catalog gate control response fields are invalid')
  }
  if (fields.schema_version !== RESPONSE_SCHEMA_VERSION ||
      fields.action !== expectedAction ||
      (fields.outcome !== OBSERVED_OUTCOME && fields.outcome !== RELEASED_OUTCOME) ||
      !Number.isSafeInteger(fields.blocked_requests) ||
      (fields.blocked_requests as number) < 0 ||
      typeof fields.released !== 'boolean') {
    throw new Error('Catalog gate control response values are invalid')
  }
  const canonical = JSON.stringify({
    schema_version: fields.schema_version,
    action: fields.action,
    outcome: fields.outcome,
    blocked_requests: fields.blocked_requests,
    released: fields.released,
  })
  if (canonical !== text.slice(0, -1)) {
    throw new Error('Catalog gate control response is not canonical JSON')
  }
  return Object.freeze({
    schemaVersion: RESPONSE_SCHEMA_VERSION,
    action: expectedAction,
    outcome: fields.outcome,
    blockedRequests: fields.blocked_requests as number,
    released: fields.released,
  })
}
