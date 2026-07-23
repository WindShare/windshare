import { describe, expect, it } from 'vitest'

import {
  foldPortableName15,
  fullCaseFold15,
  PINNED_CASE_FOLD_VERSION,
} from '../../src/unicode/case-fold'

describe('pinned Unicode case folding', () => {
  it('uses the frozen Unicode 15 table rather than locale-sensitive lowercase', () => {
    expect(PINNED_CASE_FOLD_VERSION).toBe('15.0.0')
    expect(fullCaseFold15('Straße')).toBe('strasse')
    expect(fullCaseFold15('ẞ-ﬃ-ſ')).toBe('ss-ffi-s')
    expect(fullCaseFold15('İ-I-ı')).toBe('i̇-i-ı')
  })

  it('normalizes the folded collision identity with the pinned NFC table', () => {
    expect(foldPortableName15('Ångström')).toBe(foldPortableName15('A\u030angstro\u0308m'))
  })
})
