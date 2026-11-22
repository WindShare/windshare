import { isCatalogName } from '../../catalog/path-policy'
import {
  MAX_SAFE_RENAMED_SAMPLE,
  safeRenamedSummary,
  type SafeRenamedEntry,
  type SafeRenamedSummary,
} from '../../transfer/state'
import { isPortableOutputName } from '../names/policy'

const SAFE_NAME_DOMAIN = 'windshare/fsa-safe-name/v1'
const SAFE_NAME_MARKER = '.windshare-'
const SAFE_NAME_HASH_BYTES = 4
const MAX_READABLE_PREFIX_BYTES = 64
const FALLBACK_PREFIX = 'entry'
const RESERVED_STEM_PREFIX = 'entry-'
const MAX_COLLISION_INDEX = 0xffff_ffff

const TEXT_ENCODER = new TextEncoder()
const SAFE_CODE_POINT = /[\p{L}\p{N}]/u
const SAFE_NAME_SUFFIX_PATTERN = /\.windshare-[0-9a-f]{8}$/

export type SafeNameEntryKind = 'file' | 'directory'

export function isSafeComponentName(name: string): boolean {
  return SAFE_NAME_SUFFIX_PATTERN.test(name)
}

export interface DeterministicSafeComponentInput {
  readonly logicalPath: readonly string[]
  readonly entryKind: SafeNameEntryKind
  readonly collisionIndex?: number
}

/**
 * Produces a deterministic, portable FSA component for a logical name rejected by the
 * browser or OS. Replacing any original extension avoids repeating blocked-suffix refusals.
 */
export async function deterministicSafeComponent(
  input: DeterministicSafeComponentInput,
): Promise<string> {
  if (input.logicalPath.length === 0) {
    throw new TypeError('Deterministic safe component requires a non-empty logical path')
  }
  for (const segment of input.logicalPath) {
    if (!isCatalogName(segment)) {
      throw new TypeError('Deterministic safe component requires canonical catalog path segments')
    }
  }
  const collisionIndex = input.collisionIndex ?? 0
  if (!Number.isSafeInteger(collisionIndex) || collisionIndex < 0 || collisionIndex > MAX_COLLISION_INDEX) {
    throw new RangeError('Safe component collision index must be an unsigned 32-bit integer')
  }

  const encodedPath = input.logicalPath.map(segment => `${segment.length}:${segment}`).join('/')
  const material = TEXT_ENCODER.encode(
    `${SAFE_NAME_DOMAIN}\0${input.entryKind}\0${collisionIndex}\0${encodedPath}`,
  )
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', material))
  const suffix = hexPrefix(digest, SAFE_NAME_HASH_BYTES)
  const prefix = readablePrefix(input.logicalPath.at(-1)!)
  const candidate = `${prefix}${SAFE_NAME_MARKER}${suffix}`
  const safeName = isPortableOutputName(candidate) ? candidate : `${RESERVED_STEM_PREFIX}${candidate}`
  if (!isPortableOutputName(safeName)) {
    throw new TypeError('Generated FSA safe component failed portable output validation')
  }
  return safeName
}

export function decideCollisionName(
  requestedName: string,
  kind: 'file' | 'directory',
  collisionIndex: number,
): string {
  if (collisionIndex <= 0) return requestedName
  if (kind === 'directory') return `${requestedName} (${collisionIndex})`
  const dot = requestedName.lastIndexOf('.')
  if (dot <= 0 || dot === requestedName.length - 1) return `${requestedName} (${collisionIndex})`
  return `${requestedName.slice(0, dot)} (${collisionIndex})${requestedName.slice(dot)}`
}

export class SafeNameRegistry {
  readonly #assigned = new Map<string, string>()
  readonly #recorded = new Map<string, SafeRenamedEntry>()

  physicalComponent(logicalPath: readonly string[], entryKind: SafeNameEntryKind): string | undefined {
    return this.#assigned.get(entryKey(logicalPath, entryKind))
  }

  assign(logicalPath: readonly string[], entryKind: SafeNameEntryKind, physicalName: string): void {
    this.#assigned.set(entryKey(logicalPath, entryKind), physicalName)
  }

  record(logicalPath: readonly string[], entryKind: SafeNameEntryKind, physicalName: string): void {
    const key = entryKey(logicalPath, entryKind)
    this.#assigned.set(key, physicalName)
    if (this.#recorded.has(key)) return
    this.#recorded.set(key, Object.freeze({
      logicalPath: Object.freeze([...logicalPath]),
      physicalName,
    }))
  }

  summary(): SafeRenamedSummary | undefined {
    if (this.#recorded.size === 0) return undefined
    const sample: SafeRenamedEntry[] = []
    for (const entry of this.#recorded.values()) {
      if (sample.length >= MAX_SAFE_RENAMED_SAMPLE) break
      sample.push(entry)
    }
    return safeRenamedSummary({ count: this.#recorded.size, sample })
  }
}

function readablePrefix(leaf: string): string {
  let result = ''
  let usedBytes = 0
  let pendingSeparator = false
  for (const character of leaf) {
    if (!SAFE_CODE_POINT.test(character)) {
      if (result.length > 0) pendingSeparator = true
      continue
    }
    const charBytes = TEXT_ENCODER.encode(character).byteLength
    const separatorBytes = pendingSeparator ? 1 : 0
    if (usedBytes + separatorBytes + charBytes > MAX_READABLE_PREFIX_BYTES) break
    if (pendingSeparator) {
      result += '-'
      usedBytes += 1
      pendingSeparator = false
    }
    result += character
    usedBytes += charBytes
  }
  return result.length > 0 ? result : FALLBACK_PREFIX
}

function hexPrefix(bytes: Uint8Array, length: number): string {
  let result = ''
  for (let index = 0; index < length; index += 1) {
    result += (bytes[index] ?? 0).toString(16).padStart(2, '0')
  }
  return result
}

function entryKey(logicalPath: readonly string[], entryKind: SafeNameEntryKind): string {
  return `${entryKind}\0${JSON.stringify(logicalPath)}`
}
