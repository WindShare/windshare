import {
  UNICODE_CASE_FOLD_DATA,
  UNICODE_CASE_FOLD_VERSION,
} from './unicode-case-fold-data'
import { normalizeNFC15 } from './unicode-normalization'

export const PINNED_CASE_FOLD_VERSION = UNICODE_CASE_FOLD_VERSION

let caseFoldMappings: ReadonlyMap<number, string> | undefined

function foldMappings(): ReadonlyMap<number, string> {
  if (caseFoldMappings !== undefined) return caseFoldMappings

  const mappings = new Map<number, string>()
  for (const record of UNICODE_CASE_FOLD_DATA.trim().split(/\s+/u)) {
    const separator = record.indexOf('=')
    const source = Number.parseInt(record.slice(0, separator), 16)
    const replacement = record
      .slice(separator + 1)
      .split('.')
      .map((scalar) => String.fromCodePoint(Number.parseInt(scalar, 16)))
      .join('')
    mappings.set(source, replacement)
  }
  caseFoldMappings = mappings
  return mappings
}

export function fullCaseFold15(value: string): string {
  const mappings = foldMappings()
  let output = ''
  let unchangedStart = 0
  for (let offset = 0; offset < value.length; ) {
    const scalar = value.codePointAt(offset)
    if (scalar === undefined) break

    const width = scalar > 0xffff ? 2 : 1
    const replacement = mappings.get(scalar)
    if (replacement !== undefined) {
      output += value.slice(unchangedStart, offset)
      output += replacement
      unchangedStart = offset + width
    }
    offset += width
  }
  return output + value.slice(unchangedStart)
}

/** The collision identity is pinned so browser/OS locale cannot change share semantics. */
export function foldPortableName15(value: string): string {
  return normalizeNFC15(fullCaseFold15(value))
}
