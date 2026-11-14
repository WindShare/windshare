import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { text } from '../../src/network/strict-json'

// Go derives this list from unicode.IsSpace plus U+FEFF, so both verifiers are
// held to one generated set instead of a hand-copied one.
const { invalid_edge_code_points: edges } = JSON.parse(readFileSync(new URL('../../../testdata/network/text-edges.json', import.meta.url), 'utf8')) as {
  invalid_edge_code_points: number[]
}
const REJECTED_ANYWHERE = new Set([0x0, 0x9, 0xa, 0xd])
const MAXIMUM_CODE_POINT = 0x10ffff
const isSurrogate = (point: number) => point >= 0xd800 && point <= 0xdfff
const label = (point: number) => `U+${point.toString(16).toUpperCase().padStart(4, '0')}`
function accepts(value: string): boolean {
  try { text(value); return true } catch { return false }
}

describe('signed directory text edges', () => {
  it('pins the Go-generated list', () => {
    expect(edges).toHaveLength(26)
    expect(edges).toContain(0xfeff)
    expect(edges).toContain(0x85)
  })
  it.each(edges.map(point => [label(point), point] as const))('rejects %s at either edge', (_, point) => {
    const char = String.fromCodePoint(point)
    expect(accepts(char + 'a')).toBe(false)
    expect(accepts('a' + char)).toBe(false)
    expect(accepts('a' + char + 'b')).toBe(!REJECTED_ANYWHERE.has(point))
  })
  it('checks only the true edges, not line boundaries inside the text', () => {
    for (const inner of ['a \u2028b', 'a\u2029 b', 'a \u0085b', 'a\u2028\u2029b']) expect(accepts(inner)).toBe(true)
  })
  it('caps UTF-8 bytes, not UTF-16 code units', () => {
    expect(accepts('\u00e9'.repeat(64))).toBe(true)
    expect(accepts('\u00e9'.repeat(63) + '\u65e5')).toBe(false)
  })
  it('accepts every other code point at either edge', () => {
    const invalid = new Set(edges), mismatches: string[] = []
    for (let point = 0; point <= MAXIMUM_CODE_POINT; point++) {
      if (isSurrogate(point)) continue
      const char = String.fromCodePoint(point), expected = !invalid.has(point) && !REJECTED_ANYWHERE.has(point)
      if (accepts(char + 'a') !== expected || accepts('a' + char) !== expected) mismatches.push(label(point))
    }
    expect(mismatches).toEqual([])
  }, 30_000)
})
