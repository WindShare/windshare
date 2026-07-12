import { describe, expect, it } from 'vitest'
import {
  BlockReassembler,
  ReassemblyViolation,
  type BlockFrame,
} from '../../src/session'

function block(sequence: number, last: boolean, payload: number[]): BlockFrame {
  return { type: 'block', index: 4n, sequence, last, payload: Uint8Array.from(payload) }
}

describe('block reassembly', () => {
  it('reassembles out-of-order frames and snapshots payload ownership', () => {
    const reassembly = new BlockReassembler(8)
    const tail = block(1, true, [3, 4])
    expect(reassembly.add(tail)).toBeUndefined()
    tail.payload.fill(99)
    expect(reassembly.add(block(0, false, [1, 2]))).toEqual(Uint8Array.of(1, 2, 3, 4))
    expect(reassembly.bufferedBytes).toBe(0)
    expect(reassembly.bufferedFrames).toBe(0)
  })

  it.each([
    ['duplicate-sequence', [block(0, false, [1]), block(0, true, [2])]],
    ['second-final', [block(2, true, [1]), block(1, true, [2])]],
    ['past-final', [block(2, false, [1]), block(1, true, [2])]],
  ] as const)('rejects %s', (kind, frames) => {
    const reassembly = new BlockReassembler(8)
    reassembly.add(frames[0])
    expect(() => reassembly.add(frames[1])).toThrowError(
      expect.objectContaining<Partial<ReassemblyViolation>>({ kind }),
    )
  })

  it('bounds incomplete ciphertext before retaining an attacker stream', () => {
    const reassembly = new BlockReassembler(2)
    reassembly.add(block(0, false, [1, 2]))
    expect(() => reassembly.add(block(1, false, [3]))).toThrowError(/2-byte/u)
    expect(reassembly.bufferedBytes).toBe(2)
  })

  it('rejects a sequence space that cannot fit the ciphertext byte budget', () => {
    const reassembly = new BlockReassembler(8)
    expect(() => reassembly.add(block(8, false, [1]))).toThrowError(
      expect.objectContaining<Partial<ReassemblyViolation>>({ kind: 'oversize' }),
    )
    expect(reassembly.bufferedBytes).toBe(0)
    expect(reassembly.bufferedFrames).toBe(0)
  })
})
