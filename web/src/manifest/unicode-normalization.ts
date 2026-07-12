import {
  UNICODE_CANONICAL_COMPOSITION_DATA,
  UNICODE_CANONICAL_DECOMPOSITION_DATA,
  UNICODE_COMBINING_CLASS_DATA,
  UNICODE_NORMALIZATION_VERSION,
} from './unicode-normalization-data'

export const PINNED_NORMALIZATION_VERSION = UNICODE_NORMALIZATION_VERSION

const SCALAR_LIMIT = 0x11_0000
const HANGUL_SYLLABLE_BASE = 0xac00
const HANGUL_LEADING_BASE = 0x1100
const HANGUL_VOWEL_BASE = 0x1161
const HANGUL_TRAILING_BASE = 0x11a7
const HANGUL_LEADING_COUNT = 19
const HANGUL_VOWEL_COUNT = 21
const HANGUL_TRAILING_COUNT = 28
const HANGUL_BLOCK_COUNT = HANGUL_VOWEL_COUNT * HANGUL_TRAILING_COUNT
const HANGUL_SYLLABLE_COUNT = HANGUL_LEADING_COUNT * HANGUL_BLOCK_COUNT
const OUTPUT_CHUNK_CHARACTERS = 4_096

interface NormalizationTables {
  readonly decompositions: ReadonlyMap<number, readonly number[]>
  readonly combiningClasses: ReadonlyMap<number, number>
  readonly compositions: ReadonlyMap<number, number>
}

let normalizationTables: NormalizationTables | undefined

function parseScalar(value: string): number {
  return Number.parseInt(value, 16)
}

function compositionKey(starter: number, next: number): number {
  return starter * SCALAR_LIMIT + next
}

function parseNormalizationTables(): NormalizationTables {
  const decompositions = new Map<number, readonly number[]>()
  for (const record of UNICODE_CANONICAL_DECOMPOSITION_DATA.trim().split(/\s+/u)) {
    const separator = record.indexOf('=')
    decompositions.set(
      parseScalar(record.slice(0, separator)),
      Object.freeze(record.slice(separator + 1).split('.').map(parseScalar)),
    )
  }

  const combiningClasses = new Map<number, number>()
  for (const record of UNICODE_COMBINING_CLASS_DATA.trim().split(/\s+/u)) {
    const separator = record.indexOf('=')
    combiningClasses.set(
      parseScalar(record.slice(0, separator)),
      Number.parseInt(record.slice(separator + 1), 10),
    )
  }

  const compositions = new Map<number, number>()
  for (const record of UNICODE_CANONICAL_COMPOSITION_DATA.trim().split(/\s+/u)) {
    const separator = record.indexOf('=')
    const [starter, next] = record.slice(0, separator).split('.').map(parseScalar)
    if (starter === undefined || next === undefined) {
      throw new Error('Pinned Unicode composition data is malformed')
    }
    compositions.set(
      compositionKey(starter, next),
      parseScalar(record.slice(separator + 1)),
    )
  }
  return Object.freeze({ decompositions, combiningClasses, compositions })
}

function tables(): NormalizationTables {
  normalizationTables ??= parseNormalizationTables()
  return normalizationTables
}

function combiningClass(scalar: number): number {
  return tables().combiningClasses.get(scalar) ?? 0
}

function decomposeHangul(scalar: number): readonly number[] | undefined {
  const syllable = scalar - HANGUL_SYLLABLE_BASE
  if (syllable < 0 || syllable >= HANGUL_SYLLABLE_COUNT) {
    return undefined
  }
  const leading = HANGUL_LEADING_BASE + Math.floor(syllable / HANGUL_BLOCK_COUNT)
  const vowel = HANGUL_VOWEL_BASE +
    Math.floor((syllable % HANGUL_BLOCK_COUNT) / HANGUL_TRAILING_COUNT)
  const trailing = syllable % HANGUL_TRAILING_COUNT
  return trailing === 0
    ? [leading, vowel]
    : [leading, vowel, HANGUL_TRAILING_BASE + trailing]
}

function decomposition(scalar: number): readonly number[] {
  return decomposeHangul(scalar) ?? tables().decompositions.get(scalar) ?? [scalar]
}

function composeHangul(starter: number, next: number): number | undefined {
  const leading = starter - HANGUL_LEADING_BASE
  const vowel = next - HANGUL_VOWEL_BASE
  if (
    leading >= 0 && leading < HANGUL_LEADING_COUNT &&
    vowel >= 0 && vowel < HANGUL_VOWEL_COUNT
  ) {
    return HANGUL_SYLLABLE_BASE +
      (leading * HANGUL_VOWEL_COUNT + vowel) * HANGUL_TRAILING_COUNT
  }

  const syllable = starter - HANGUL_SYLLABLE_BASE
  const trailing = next - HANGUL_TRAILING_BASE
  if (
    syllable >= 0 && syllable < HANGUL_SYLLABLE_COUNT &&
    syllable % HANGUL_TRAILING_COUNT === 0 &&
    trailing > 0 && trailing < HANGUL_TRAILING_COUNT
  ) {
    return starter + trailing
  }
  return undefined
}

function compose(starter: number, next: number): number | undefined {
  return composeHangul(starter, next) ??
    tables().compositions.get(compositionKey(starter, next))
}

function composeSegment(segment: number[]): number[] {
  // ECMAScript requires a stable sort. Ordering by the bounded canonical class
  // avoids quadratic insertion behavior on an attacker-controlled run of marks.
  segment.sort((left, right) => combiningClass(left) - combiningClass(right))
  const result: number[] = []
  let starterPosition = -1
  let starter = 0
  let lastClass = 0
  for (const scalar of segment) {
    const scalarClass = combiningClass(scalar)
    const composite = starterPosition >= 0 && (lastClass < scalarClass || lastClass === 0)
      ? compose(starter, scalar)
      : undefined
    if (composite !== undefined) {
      result[starterPosition] = composite
      starter = composite
      continue
    }
    result.push(scalar)
    if (scalarClass === 0) {
      starterPosition = result.length - 1
      starter = scalar
    }
    lastClass = scalarClass
  }
  return result
}

/**
 * Applies NFC with WindShare's Unicode 15.0.0 tables.
 *
 * Browser ICU data advances with the runtime. Using it directly would let two
 * peers assign different canonical identities to post-policy characters.
 */
export function normalizeNFC15(value: string): string {
  const output: string[] = []
  let bufferedOutput = ''
  let segment: number[] = []
  const emit = (scalars: readonly number[]) => {
    for (const scalar of scalars) {
      bufferedOutput += String.fromCodePoint(scalar)
      if (bufferedOutput.length >= OUTPUT_CHUNK_CHARACTERS) {
        output.push(bufferedOutput)
        bufferedOutput = ''
      }
    }
  }
  const accept = (scalar: number) => {
    const scalarClass = combiningClass(scalar)
    if (scalarClass !== 0) {
      segment.push(scalar)
      return
    }
    if (segment.length === 0) {
      segment.push(scalar)
      return
    }
    const directStarter = segment.length === 1 ? segment[0] : undefined
    if (directStarter !== undefined && combiningClass(directStarter) === 0) {
      const directComposite = compose(directStarter, scalar)
      if (directComposite !== undefined) {
        segment[0] = directComposite
      } else {
        emit(segment)
        segment[0] = scalar
      }
      return
    }
    const normalized = composeSegment(segment)
    const previous = normalized.length === 1 ? normalized[0] : undefined
    const composite = previous !== undefined && combiningClass(previous) === 0
      ? compose(previous, scalar)
      : undefined
    if (composite !== undefined) {
      segment = [composite]
      return
    }
    emit(normalized)
    segment = [scalar]
  }

  for (const character of value) {
    const scalar = character.codePointAt(0)
    if (scalar === undefined) {
      continue
    }
    for (const decomposed of decomposition(scalar)) {
      accept(decomposed)
    }
  }
  emit(composeSegment(segment))
  if (bufferedOutput !== '') {
    output.push(bufferedOutput)
  }
  return output.join('')
}
