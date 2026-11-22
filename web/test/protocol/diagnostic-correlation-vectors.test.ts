import { describe, expect, it } from 'vitest'

import {
  PROTOCOL_ERROR_SCOPES,
  PROTOCOL_MESSAGE_KINDS,
} from '../../src/session/diagnostics'
import {
  createDiagnosticIdentity,
  type DiagnosticBytesIdentity,
  type DiagnosticBytesIdentityKind,
  type DiagnosticIdentity,
  type ProtocolCorrelation,
} from '../../src/session/identities'
import {
  projectCorrelation,
  type CorrelationRecord,
} from '../../src/application/diagnostics/protocol-trace'
import { loadVectorFile } from '../vectors'

const vectors = loadVectorFile(
  new URL('../../../core/testvectors/diagnostic-correlation-v1.json', import.meta.url),
)
const DECIMAL_TEXT = /^(?:0|[1-9]\d*)$/u
const POSITIVE_DECIMAL_TEXT = /^[1-9]\d*$/u
const BASE64URL_IDENTITY = /^[A-Za-z0-9_-]{22}$/u
const MAX_UINT64 = (1n << 64n) - 1n

if (vectors.kind !== 'diagnostic-correlation-v1') {
  throw new Error(`unexpected diagnostic correlation vector kind: ${vectors.kind}`)
}

describe('Go↔TypeScript diagnostic correlation vectors', () => {
  for (const vector of vectors.cases) {
    it(`replays ${vector.name}`, () => {
      const input = requiredRecord(vector.input, 'vector input')
      const expected = requiredRecord(vector.expected, 'vector expected')
      const correlation = correlationFromVector(input)
      const projected = projectCorrelation(correlation)
      const expectedCorrelation = optionalCorrelation(expected.correlation)

      expect(projected).toEqual(expectedCorrelation)
      expect(JSON.stringify(projected)).toBe(JSON.stringify(expectedCorrelation))
      if (projected !== undefined) {
        expect(Object.isFrozen(projected)).toBe(true)
        assertCanonicalIdentities(projected)
      }

      const sequence = requiredDecimal(input.sequence, 'input sequence')
      const elapsedMilliseconds = requiredDecimal(input.elapsed_ms, 'input elapsed milliseconds')
      expect(requiredDecimal(expected.sequence, 'expected sequence')).toBe(sequence)
      expect(requiredDecimal(expected.elapsed_ms, 'expected elapsed milliseconds'))
        .toBe(elapsedMilliseconds)
      expect(BigInt(sequence)).toBeLessThanOrEqual(MAX_UINT64)
      expect(BigInt(elapsedMilliseconds)).toBeLessThanOrEqual(MAX_UINT64)

      expect(PROTOCOL_MESSAGE_KINDS).toContain(
        requiredString(expected.request_kind, 'request kind'),
      )
      expect(PROTOCOL_ERROR_SCOPES).toContain(
        requiredString(expected.wire_scope, 'wire scope'),
      )
      const wireCode = requiredNumber(expected.wire_code, 'wire code')
      expect(Number.isInteger(wireCode) && wireCode >= 0 && wireCode <= 0xffff).toBe(true)
    })
  }
})

describe('diagnostic correlation projection invariants', () => {
  it('rejects dependent identities without their authorities', () => {
    const operation = createDiagnosticIdentity('protocol_operation', 16n)
    const attempt = createDiagnosticIdentity('peer_attempt', identityBytes(0x20))

    expect(() => projectCorrelation({
      protocolOperationId: operation,
    })).toThrow(/requires a protocol session/u)
    expect(() => projectCorrelation({
      peerAttemptId: attempt,
    })).toThrow(/requires a peer path/u)
  })

  it('rejects malformed bytes and lane values at the projection boundary', () => {
    const malformed = fakeIdentity('protocol_session', new Uint8Array(15))
    expect(() => projectCorrelation({
      protocolSessionId: malformed,
    })).toThrow(/exactly 16 bytes/u)
    expect(() => projectCorrelation({
      lane: { id: -1, epoch: 0 },
    })).toThrow(/uint32 ID and epoch/u)
    expect(() => projectCorrelation({
      lane: { id: 1, epoch: 0x1_0000_0000 },
    })).toThrow(/uint32 ID and epoch/u)
  })

  it('omits all-zero identities and keeps a zero lane epoch', () => {
    const allZero = fakeIdentity('protocol_session', new Uint8Array(16))
    expect(projectCorrelation({ protocolSessionId: allZero })).toBeUndefined()

    const projected = projectCorrelation({ lane: { id: 0, epoch: 0 } })
    expect(projected).toEqual({ lane_id: 0, lane_epoch: 0 })
    expect(JSON.stringify(projected)).toBe('{"lane_id":0,"lane_epoch":0}')
  })
})

function correlationFromVector(input: Record<string, unknown>): ProtocolCorrelation | undefined {
  const candidate: {
    protocolSessionId?: DiagnosticIdentity<'protocol_session'>
    protocolOperationId?: DiagnosticIdentity<'protocol_operation'>
    peerPathId?: DiagnosticIdentity<'peer_path'>
    peerAttemptId?: DiagnosticIdentity<'peer_attempt'>
    lane?: Readonly<{ id: number; epoch: number }>
  } = {}

  const session = optionalIdentityBytes(input.protocol_session_id_bytes, 'protocol session')
  if (session !== undefined) {
    candidate.protocolSessionId = createDiagnosticIdentity('protocol_session', session)
  }
  if (input.protocol_operation_id !== undefined) {
    candidate.protocolOperationId = createDiagnosticIdentity(
      'protocol_operation',
      BigInt(requiredDecimal(input.protocol_operation_id, 'protocol operation')),
    )
  }
  const path = optionalIdentityBytes(input.peer_path_id_bytes, 'peer path')
  if (path !== undefined) candidate.peerPathId = createDiagnosticIdentity('peer_path', path)
  const attempt = optionalIdentityBytes(input.peer_attempt_id_bytes, 'peer attempt')
  if (attempt !== undefined) {
    candidate.peerAttemptId = createDiagnosticIdentity('peer_attempt', attempt)
  }

  if (input.lane_id !== undefined || input.lane_epoch !== undefined) {
    candidate.lane = {
      id: requiredNumber(input.lane_id, 'lane ID'),
      epoch: requiredNumber(input.lane_epoch, 'lane epoch'),
    }
  }
  return Object.keys(candidate).length === 0
    ? undefined
    : Object.freeze(candidate)
}

function optionalCorrelation(value: unknown): CorrelationRecord | undefined {
  if (value === undefined) return undefined
  return requiredRecord(value, 'expected correlation') as CorrelationRecord
}

function optionalIdentityBytes(value: unknown, label: string): Uint8Array | undefined {
  if (value === undefined) return undefined
  if (!Array.isArray(value) || value.length !== 16) {
    throw new Error(`${label} identity must contain 16 byte numbers`)
  }
  const numbers = value.map((entry) => requiredNumber(entry, `${label} identity byte`))
  if (numbers.some((entry) => !Number.isInteger(entry) || entry < 0 || entry > 0xff)) {
    throw new Error(`${label} identity contains a non-byte value`)
  }
  return Uint8Array.from(numbers)
}

function requiredRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(value: unknown, label: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${label} must be a non-empty string`)
  }
  return value
}

function requiredDecimal(value: unknown, label: string): string {
  const text = requiredString(value, label)
  if (!DECIMAL_TEXT.test(text)) throw new Error(`${label} must be canonical decimal text`)
  return text
}

function requiredNumber(value: unknown, label: string): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`${label} must be a finite number`)
  }
  return value
}

function assertCanonicalIdentities(correlation: CorrelationRecord): void {
  for (const identity of [
    correlation.protocol_session_id,
    correlation.peer_path_id,
    correlation.peer_attempt_id,
  ]) {
    if (identity === undefined) continue
    expect(identity).toMatch(BASE64URL_IDENTITY)
    expect(identity).not.toContain('=')
  }
  if (correlation.protocol_operation_id !== undefined) {
    expect(correlation.protocol_operation_id).toMatch(POSITIVE_DECIMAL_TEXT)
    expect(BigInt(correlation.protocol_operation_id)).toBeLessThanOrEqual(MAX_UINT64)
  }
}

function identityBytes(start: number): Uint8Array {
  return Uint8Array.from({ length: 16 }, (_, index) => (start + index) & 0xff)
}

function fakeIdentity<Kind extends DiagnosticBytesIdentityKind>(
  kind: Kind,
  bytes: Uint8Array,
): DiagnosticBytesIdentity<Kind> {
  return {
    kind,
    byteLength: 16,
    copyBytes: () => bytes.slice(),
  }
}
