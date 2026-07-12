import { createHash } from 'node:crypto'

import { describe, expect, it } from 'vitest'

import {
  UNICODE_CANONICAL_COMPOSITION_DATA,
  UNICODE_CANONICAL_DECOMPOSITION_DATA,
  UNICODE_COMBINING_CLASS_DATA,
  UNICODE_NORMALIZATION_DATA_SHA256,
  UNICODE_NORMALIZATION_VERSION,
} from '../../src/manifest/unicode-normalization-data'
import {
  normalizeNFC15,
  PINNED_NORMALIZATION_VERSION,
} from '../../src/manifest/unicode-normalization'

const DECOMPOSITION_RECORDS = UNICODE_CANONICAL_DECOMPOSITION_DATA.trim().split(/\s+/u)
const COMBINING_CLASS_RECORDS = UNICODE_COMBINING_CLASS_DATA.trim().split(/\s+/u)
const COMPOSITION_RECORDS = UNICODE_CANONICAL_COMPOSITION_DATA.trim().split(/\s+/u)
const HANGUL_SYLLABLE_BASE = 0xac00
const HANGUL_SYLLABLE_COUNT = 11_172

function scalars(value: string): string {
  return value
    .split('.')
    .map((scalar) => String.fromCodePoint(Number.parseInt(scalar, 16)))
    .join('')
}

describe('pinned Unicode normalization', () => {
  it('pins the generated Unicode 15 data set and its complete digest', () => {
    const digest = createHash('sha256')
      .update(
        UNICODE_CANONICAL_DECOMPOSITION_DATA +
        UNICODE_COMBINING_CLASS_DATA +
        UNICODE_CANONICAL_COMPOSITION_DATA,
      )
      .digest('hex')
      .toUpperCase()

    expect(PINNED_NORMALIZATION_VERSION).toBe('15.0.0')
    expect(UNICODE_NORMALIZATION_VERSION).toBe(PINNED_NORMALIZATION_VERSION)
    expect(DECOMPOSITION_RECORDS).toHaveLength(2_061)
    expect(COMBINING_CLASS_RECORDS).toHaveLength(922)
    expect(COMPOSITION_RECORDS).toHaveLength(941)
    expect(digest).toBe(UNICODE_NORMALIZATION_DATA_SHA256)
  })

  it('reproduces every Unicode 15 canonical decomposition and composition', () => {
    for (const record of DECOMPOSITION_RECORDS) {
      const separator = record.indexOf('=')
      const source = String.fromCodePoint(Number.parseInt(record.slice(0, separator), 16))
      const decomposition = scalars(record.slice(separator + 1))

      // Unicode normalization stability guarantees that later runtimes retain the
      // Unicode 15 result for characters already assigned in this pinned table.
      expect(normalizeNFC15(source)).toBe(source.normalize('NFC'))
      expect(normalizeNFC15(decomposition)).toBe(decomposition.normalize('NFC'))
    }
  })

  it('implements canonical ordering, exclusions, and algorithmic Hangul', () => {
    expect(normalizeNFC15('q\u0315\u0300\u05ae\u0301')).toBe(
      'q\u05ae\u0300\u0301\u0315',
    )
    expect(normalizeNFC15('\u0344')).toBe('\u0308\u0301')
    expect(normalizeNFC15('\u1100\u1161\u11a8')).toBe('\uac01')
    expect(normalizeNFC15('\uac01')).toBe('\uac01')
  })

  it('matches stable Unicode 15 behavior across mixed deterministic sequences', () => {
    const pool = [
      ...DECOMPOSITION_RECORDS.map((record) =>
        String.fromCodePoint(Number.parseInt(record.slice(0, record.indexOf('=')), 16))),
      ...COMBINING_CLASS_RECORDS.map((record) =>
        String.fromCodePoint(Number.parseInt(record.slice(0, record.indexOf('=')), 16))),
      'A',
      '\u1100',
      '\u1161',
      '\u11a8',
    ]
    let state = 0x6d2b_79f5
    for (let sample = 0; sample < 2_048; sample += 1) {
      let value = ''
      const length = 1 + (state % 12)
      for (let index = 0; index < length; index += 1) {
        state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
        value += pool[state % pool.length] ?? ''
      }
      expect(normalizeNFC15(value)).toBe(value.normalize('NFC'))
    }
  })

  it('recomposes every algorithmic Hangul syllable', () => {
    for (let offset = 0; offset < HANGUL_SYLLABLE_COUNT; offset += 1) {
      const syllable = String.fromCodePoint(HANGUL_SYLLABLE_BASE + offset)
      expect(normalizeNFC15(syllable.normalize('NFD'))).toBe(syllable)
    }
  })

  it('does not import post-policy compositions from ambient browser ICU', () => {
    const postUnicode15Sequence = '\u{105d2}\u0307'
    const postUnicode15Composite = '\u{105c9}'

    expect(normalizeNFC15(postUnicode15Sequence)).toBe(postUnicode15Sequence)
    expect(normalizeNFC15(postUnicode15Composite)).toBe(postUnicode15Composite)
    expect(normalizeNFC15(postUnicode15Sequence)).not.toBe(postUnicode15Composite)
  })
})
