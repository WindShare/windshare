import { describe, expect, it } from 'vitest'
import {
  BLOCK_HEADER_BYTES,
  FrameCodecError,
  MAX_BLOCK_PAYLOAD_BYTES,
  decodeFrame,
  encodeBlock,
  encodeError,
  encodeRequest,
  splitBlockCiphertext,
} from '../../src/session'
import { b64ToBytes, loadVectorFile } from '../vectors'

interface FrameVector {
  readonly name: string
  readonly frameB64: string
  readonly request?: { readonly indices: readonly string[] }
  readonly block?: {
    readonly index: string
    readonly seq: number
    readonly last: boolean
    readonly payloadB64: string
  }
  readonly error?: { readonly code: number; readonly msg: string }
}

const vectors = loadVectorFile(new URL('../../../testvectors/frame-codec.json', import.meta.url))

describe('session frame codec', () => {
  for (const vector of vectors.cases as unknown as FrameVector[]) {
    it(`matches shared vector ${vector.name}`, () => {
      const want = b64ToBytes(vector.frameB64)
      let encoded: Uint8Array
      if (vector.request !== undefined) {
        encoded = encodeRequest(vector.request.indices.map(BigInt))
      } else if (vector.block !== undefined) {
        encoded = encodeBlock({
          index: BigInt(vector.block.index),
          sequence: vector.block.seq,
          last: vector.block.last,
          payload: b64ToBytes(vector.block.payloadB64),
        })
      } else if (vector.error !== undefined) {
        encoded = encodeError(vector.error.code, vector.error.msg)
      } else {
        throw new Error(`unsupported frame vector ${vector.name}`)
      }
      expect(encoded).toEqual(want)
      expect(decodeFrame(want)).toBeDefined()
    })
  }

  it('rejects noncanonical lengths, flags, UTF-8, and bounds', () => {
    expect(() => decodeFrame(new Uint8Array())).toThrow(FrameCodecError)
    expect(() => encodeRequest([])).toThrowError(/contain an index/u)
    expect(() => encodeBlock({
      index: 0n,
      sequence: 0,
      last: true,
      payload: new Uint8Array(),
    })).toThrowError(/must not be empty/u)

    const malformed = encodeBlock({
      index: 0n,
      sequence: 0,
      last: true,
      payload: Uint8Array.of(1),
    })
    malformed[13] = 0x80
    expect(() => decodeFrame(malformed)).toThrowError(/undefined flags/u)
    expect(() => decodeFrame(Uint8Array.of(3, 1, 0, 1, 0, 0xff))).toThrowError(
      /valid UTF-8/u,
    )
    expect(() => encodeError(1, '\ud800')).toThrowError(
      expect.objectContaining<Partial<FrameCodecError>>({ kind: 'invalid-utf8' }),
    )
  })

  it('splits ciphertext into contiguous owned frames', () => {
    const ciphertext = Uint8Array.from({ length: 11 }, (_, index) => index)
    const frames = splitBlockCiphertext(9n, ciphertext, 4)
    expect(frames).toHaveLength(3)
    expect(frames.map((wire) => decodeFrame(wire))).toMatchObject([
      { type: 'block', sequence: 0, last: false },
      { type: 'block', sequence: 1, last: false },
      { type: 'block', sequence: 2, last: true },
    ])
    ciphertext.fill(99)
    expect((decodeFrame(frames[0]!) as { payload: Uint8Array }).payload).toEqual(
      Uint8Array.of(0, 1, 2, 3),
    )
    expect(MAX_BLOCK_PAYLOAD_BYTES + BLOCK_HEADER_BYTES).toBe(65_536)
  })
})
