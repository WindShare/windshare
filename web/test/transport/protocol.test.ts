import { describe, expect, it, vi } from 'vitest'
import {
  RelayProtocolError,
  MAX_SIGNALING_JSON_DEPTH,
  createSessionId,
  decodeRelayEnvelope,
  decodeSignaling,
  encodeForwardEnvelope,
  encodeManifestEnvelope,
  encodeSignaling,
  encodeTerminalForwardEnvelope,
  formatSessionId,
  parseSessionId,
} from '../../src/transport/relay'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface RelayVector {
  readonly name: string
  readonly envelope: 'manifest' | 'forward' | 'terminal-forward'
  readonly sessionIdB64?: string
  readonly payloadB64: string
  readonly frameB64: string
}

interface RelaySignalingVector {
  readonly name: string
  readonly wire: string
  readonly accepted: boolean
  readonly canonical?: string
}

const vectors = loadVectorFile(new URL('../../../testvectors/relay-envelope.json', import.meta.url))
const signalingVectors = loadVectorFile(
  new URL('../../../testvectors/relay-signaling.json', import.meta.url),
)

describe('relay protocol', () => {
  for (const vector of vectors.cases as unknown as RelayVector[]) {
    it(`matches shared vector ${vector.name}`, () => {
      const payload = b64ToBytes(vector.payloadB64)
      let encoded: Uint8Array
      if (vector.envelope === 'manifest') {
        encoded = encodeManifestEnvelope(payload)
      } else {
        const sessionId = createSessionId(b64ToBytes(vector.sessionIdB64!))
        encoded = vector.envelope === 'forward'
          ? encodeForwardEnvelope(sessionId, payload)
          : encodeTerminalForwardEnvelope(sessionId, payload)
      }
      expect(encoded).toEqual(b64ToBytes(vector.frameB64))
      expect(decodeRelayEnvelope(encoded).type).toBe(vector.envelope)
    })
  }

  for (const vector of signalingVectors.cases as unknown as RelaySignalingVector[]) {
    it(`matches shared signaling contract ${vector.name}`, () => {
      if (!vector.accepted) {
        expect(() => decodeSignaling(vector.wire)).toThrow(RelayProtocolError)
        return
      }
      const decoded = decodeSignaling(vector.wire)
      const encoded = encodeSignaling(decoded)
      if (vector.canonical !== undefined) {
        expect(encoded).toBe(vector.canonical)
      } else {
        expect(() => decodeSignaling(encoded)).not.toThrow()
      }
    })
  }

  it('uses canonical eight-byte base64url session IDs', () => {
    const id = createSessionId(Uint8Array.of(0, 1, 2, 3, 4, 5, 6, 7))
    expect(formatSessionId(id)).toBe('AAECAwQFBgc')
    expect(parseSessionId('AAECAwQFBgc')).toEqual(id)
    expect(() => parseSessionId('AAECAwQFBgc=')).toThrow(RelayProtocolError)
    expect(() => parseSessionId('AAECAwQFBg')).toThrow(RelayProtocolError)

    const decode = vi.spyOn(globalThis, 'atob')
    expect(() => parseSessionId('A'.repeat(65_536))).toThrow(RelayProtocolError)
    expect(decode).not.toHaveBeenCalled()
  })

  it('strictly validates signaling size, fields, and types', () => {
    const join = encodeSignaling({ type: 'join', shareId: 'abc_123' })
    expect(decodeSignaling(join)).toEqual({ type: 'join', shareId: 'abc_123' })
    expect(() => decodeSignaling('{')).toThrowError(/malformed/u)
    expect(() => decodeSignaling(JSON.stringify({ type: 'unknown' }))).toThrowError(
      /unknown signaling/u,
    )
    expect(() => encodeSignaling({
      type: 'signal',
      sessionId: 'AAECAwQFBgc',
      kind: 'offer',
      payload: undefined,
    })).toThrowError(/missing payload/u)
    expect(() => decodeSignaling('{"type":"error","code":"bad","message":"\\ud800"}'))
      .toThrowError(/Unicode scalar/u)
  })

  it('bounds signaling structure before parser or serializer recursion limits', () => {
    const nestedArray = (depth: number): unknown => {
      let value: unknown = null
      for (let level = 0; level < depth; level += 1) {
        value = [value]
      }
      return value
    }
    const message = (payload: unknown) => ({
      type: 'signal' as const,
      sessionId: 'AAECAwQFBgc',
      kind: 'offer',
      payload,
    })

    expect(() => encodeSignaling(message(nestedArray(MAX_SIGNALING_JSON_DEPTH - 1))))
      .not.toThrow()
    expect(() => encodeSignaling(message(nestedArray(MAX_SIGNALING_JSON_DEPTH))))
      .toThrowError(/nesting depth/u)
  })
})
