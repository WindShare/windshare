import { isCatalogName } from '../../catalog/path-policy'
import {
  MAX_SAFE_RENAMED_SAMPLE,
  safeRenamedSummary,
  type SafeRenamedEntry,
  type SafeRenamedSummary,
} from '../../transfer/state'
import { decideCollisionName, isPortableOutputName } from '../names/policy'
import type {
  ReceiveFileRecord,
  ReceiveTaskRecord,
  SafeRenamedDirectoryRecord,
} from '../store/types'

export { decideCollisionName } from '../names/policy'

const SAFE_NAME_DOMAIN = 'windshare/fsa-safe-name/v1'
const SAFE_NAME_MARKER = '.windshare-'
const SAFE_NAME_HASH_BYTES = 8
const MAX_READABLE_PREFIX_BYTES = 64
const FALLBACK_PREFIX = 'entry'
const RESERVED_STEM_PREFIX = 'entry-'
const MAX_COLLISION_INDEX = 0xffff_ffff

const TEXT_ENCODER = new TextEncoder()
const SAFE_CODE_POINT = /[\p{L}\p{N}]/u
const SAFE_NAME_SUFFIX_PATTERN = /\.windshare-[0-9a-f]{16}$/

export type SafeNameEntryKind = 'file' | 'directory'
export type SafeNameEntryScope = 'root' | 'child'
export type DirectoryClaimStatus = 'claimed' | 'owned' | 'conflict'

export function isSafeComponentName(name: string): boolean {
  return SAFE_NAME_SUFFIX_PATTERN.test(name)
}

export interface DeterministicSafeComponentInput {
  readonly taskId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: SafeNameEntryKind
  readonly collisionIndex?: number
}

/**
 * Produces a per-task deterministic, portable FSA component for a logical name
 * rejected by the browser or OS. Salting with the task's random ID isolates
 * each download from re-shared safe names and sender predictions while keeping
 * pause/resume and crash recovery deterministic within the same task.
 * Replacing any original extension avoids repeating blocked-suffix refusals.
 */
export async function deterministicSafeComponent(
  input: DeterministicSafeComponentInput,
): Promise<string> {
  if (input.taskId.length === 0) {
    throw new TypeError('Deterministic safe component requires a non-empty taskId')
  }
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
    `${SAFE_NAME_DOMAIN}\0${input.taskId.length}:${input.taskId}\0${input.entryKind}\0${collisionIndex}\0${encodedPath}`,
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

export type SafeRenamedTaskSlice = Pick<ReceiveTaskRecord, 'artifact' | 'target' | 'safeRenamedDirectories'>
export type SafeRenamedFileSlice = Pick<ReceiveFileRecord, 'path' | 'fsaPath' | 'status'>

export interface CollectedSafeRenamedState {
  readonly summary: SafeRenamedSummary | undefined
  readonly directories: readonly SafeRenamedDirectoryRecord[] | undefined
}

export class SafeNameRegistry {
  readonly #assigned = new Map<string, string>()
  readonly #recorded = new Map<string, SafeRenamedEntry>()
  readonly #directories = new Map<string, SafeRenamedDirectoryRecord>()
  readonly #directoryOwners = new Map<string, string>()
  readonly #listeners = new Set<(summary: SafeRenamedSummary) => void>()

  constructor(onChange?: (summary: SafeRenamedSummary) => void) {
    if (onChange !== undefined) this.#listeners.add(onChange)
  }

  subscribe(listener: (summary: SafeRenamedSummary) => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  /**
   * Enforces 1:1 physical-to-logical ownership for directories within a task so
   * two distinct logical folders can never silently merge into one physical path.
   */
  claimDirectory(
    logicalPath: readonly string[],
    physicalPath: readonly string[],
  ): DirectoryClaimStatus {
    const physKey = pathKey(physicalPath)
    const logKey = pathKey(logicalPath)
    const owner = this.#directoryOwners.get(physKey)
    if (owner === undefined) {
      this.#directoryOwners.set(physKey, logKey)
      return 'claimed'
    }
    return owner === logKey ? 'owned' : 'conflict'
  }

  releaseDirectoryClaim(
    logicalPath: readonly string[],
    physicalPath: readonly string[],
  ): void {
    const physKey = pathKey(physicalPath)
    if (this.#directoryOwners.get(physKey) === pathKey(logicalPath)) {
      this.#directoryOwners.delete(physKey)
    }
  }

  physicalComponent(
    logicalPath: readonly string[],
    entryKind: SafeNameEntryKind,
    scope: SafeNameEntryScope = 'child',
  ): string | undefined {
    return this.#assigned.get(entryKey(logicalPath, entryKind, scope))
  }

  assign(
    logicalPath: readonly string[],
    entryKind: SafeNameEntryKind,
    physicalName: string,
    scope: SafeNameEntryScope = 'child',
  ): void {
    this.#assigned.set(entryKey(logicalPath, entryKind, scope), physicalName)
  }

  record(
    logicalPath: readonly string[],
    entryKind: SafeNameEntryKind,
    physicalName: string,
    scope: SafeNameEntryScope = 'child',
  ): void {
    if (this.#insertRecorded(logicalPath, entryKind, physicalName, scope)) {
      this.#notify()
    }
  }

  recordRoot(
    logicalPath: readonly string[],
    entryKind: SafeNameEntryKind,
    physicalName: string,
  ): void {
    this.record(logicalPath, entryKind, physicalName, 'root')
  }

  recordDirectory(
    logicalPath: readonly string[],
    physicalPath: readonly string[],
  ): void {
    if (logicalPath.length === 0 || logicalPath.length !== physicalPath.length) return
    const key = pathKey(logicalPath)
    const physKey = pathKey(physicalPath)
    if (!this.#directoryOwners.has(physKey)) {
      this.#directoryOwners.set(physKey, key)
    }
    if (!this.#directories.has(key)) {
      this.#directories.set(key, freezeDirectoryRecord(logicalPath, physicalPath))
    }
    if (this.#insertRecorded(logicalPath, 'directory', physicalPath.at(-1)!, 'child')) {
      this.#notify()
    }
  }

  renamedDirectories(): readonly SafeRenamedDirectoryRecord[] {
    return Object.freeze([...this.#directories.values()])
  }

  /**
   * Restores in-memory lookup and live summary counts across pause/resume, including
   * `receiving` files so resumed sessions keep resolving safe-renamed parents seamlessly.
   */
  seedFromPrior(
    task: SafeRenamedTaskSlice,
    priorFiles: readonly SafeRenamedFileSlice[],
    renamedDirectories: readonly SafeRenamedDirectoryRecord[] | undefined = task.safeRenamedDirectories,
  ): void {
    if (task.target.kind !== 'fsa') return
    const rootKind: SafeNameEntryKind = task.target.singleFile ? 'file' : 'directory'
    const expectedRoot = decideCollisionName(task.artifact.name, rootKind, task.target.collisionIndex)
    if (task.target.reservedName !== expectedRoot) {
      this.#insertRecorded([task.artifact.name], rootKind, task.target.reservedName, 'root')
    }
    if (task.target.singleFile) return
    this.#seedPriorFiles(priorFiles)
    this.#seedPriorDirectories(renamedDirectories)
  }

  summary(): SafeRenamedSummary | undefined {
    return summarizeEntries(this.#recorded)
  }

  #seedPriorFiles(priorFiles: readonly SafeRenamedFileSlice[]): void {
    for (const file of priorFiles) {
      if (file.status !== 'completed' && file.status !== 'receiving') continue
      if (file.fsaPath === undefined || file.fsaPath.length !== file.path.length) continue
      this.#seedDirectorySegments(file.path.slice(0, -1), file.fsaPath.slice(0, -1))
      const last = file.path.length - 1
      if (last >= 0 && file.fsaPath[last] !== file.path[last]) {
        this.#insertRecorded(file.path, 'file', file.fsaPath[last]!, 'child')
      }
    }
  }

  #seedPriorDirectories(renamedDirectories: readonly SafeRenamedDirectoryRecord[] | undefined): void {
    for (const dir of renamedDirectories ?? []) {
      if (dir.logicalPath.length === 0 || dir.logicalPath.length !== dir.physicalPath.length) continue
      this.#seedDirectorySegments(dir.logicalPath, dir.physicalPath)
    }
  }

  #seedDirectorySegments(logical: readonly string[], physical: readonly string[]): void {
    for (let depth = 1; depth <= logical.length; depth += 1) {
      const logicalPrefix = logical.slice(0, depth)
      const physicalPrefix = physical.slice(0, depth)
      const key = pathKey(logicalPrefix)
      const physKey = pathKey(physicalPrefix)
      if (!this.#directoryOwners.has(physKey)) {
        this.#directoryOwners.set(physKey, key)
      }
      if (physical[depth - 1] === logical[depth - 1]) continue
      if (!this.#directories.has(key)) {
        this.#directories.set(key, freezeDirectoryRecord(logicalPrefix, physicalPrefix))
      }
      this.#insertRecorded(logicalPrefix, 'directory', physicalPrefix[depth - 1]!, 'child')
    }
  }

  #insertRecorded(
    logicalPath: readonly string[],
    entryKind: SafeNameEntryKind,
    physicalName: string,
    scope: SafeNameEntryScope,
  ): boolean {
    const key = entryKey(logicalPath, entryKind, scope)
    this.#assigned.set(key, physicalName)
    if (this.#recorded.has(key)) return false
    this.#recorded.set(key, Object.freeze({
      logicalPath: Object.freeze([...logicalPath]),
      physicalName,
    }))
    return true
  }

  #notify(): void {
    const current = this.summary()
    if (current === undefined) return
    for (const listener of this.#listeners) {
      try { listener(current) } catch { /* Subscriber failure cannot break output writes. */ }
    }
  }
}

/**
 * Single source of truth for persisted safe-renamed state, shared by FSA settlement
 * and crash recovery (`pauseFromCheckpoints`). Only counts committed (`completed`) files,
 * the reserved root entry, and retained empty renamed directories.
 */
export function collectSafeRenamedFromRecords(
  task: SafeRenamedTaskSlice,
  files: readonly SafeRenamedFileSlice[],
  candidateDirectories: readonly SafeRenamedDirectoryRecord[] | undefined = task.safeRenamedDirectories,
): CollectedSafeRenamedState {
  if (task.target.kind !== 'fsa') {
    return Object.freeze({ summary: undefined, directories: undefined })
  }
  const entries = new Map<string, SafeRenamedEntry>()
  const rootKind: SafeNameEntryKind = task.target.singleFile ? 'file' : 'directory'
  const expectedRoot = decideCollisionName(task.artifact.name, rootKind, task.target.collisionIndex)
  const rootRenamed = task.target.reservedName !== expectedRoot

  if (task.target.singleFile) {
    if (rootRenamed && files.some(file => file.status === 'completed')) {
      recordEntryIfAbsent(entries, [task.artifact.name], 'file', 'root', task.target.reservedName)
    }
    return Object.freeze({ summary: summarizeEntries(entries), directories: undefined })
  }

  if (rootRenamed) {
    recordEntryIfAbsent(entries, [task.artifact.name], 'directory', 'root', task.target.reservedName)
  }

  const coveredDirectories = collectCompletedFileRenames(entries, files)
  const directories = collectCandidateDirectoryRenames(entries, coveredDirectories, candidateDirectories)
  return Object.freeze({ summary: summarizeEntries(entries), directories })
}

function recordEntryIfAbsent(
  entries: Map<string, SafeRenamedEntry>,
  logicalPath: readonly string[],
  entryKind: SafeNameEntryKind,
  scope: SafeNameEntryScope,
  physicalName: string,
): void {
  const key = entryKey(logicalPath, entryKind, scope)
  if (!entries.has(key)) {
    entries.set(key, Object.freeze({
      logicalPath: Object.freeze([...logicalPath]),
      physicalName,
    }))
  }
}

function collectCompletedFileRenames(
  entries: Map<string, SafeRenamedEntry>,
  files: readonly SafeRenamedFileSlice[],
): Set<string> {
  const coveredDirectories = new Set<string>()
  for (const file of files) {
    if (file.status !== 'completed') continue
    if (file.fsaPath === undefined || file.fsaPath.length !== file.path.length) continue
    collectCompletedFileParentRenames(entries, coveredDirectories, file.path, file.fsaPath)
    const last = file.path.length - 1
    if (last >= 0 && file.fsaPath[last] !== file.path[last]) {
      recordEntryIfAbsent(entries, file.path, 'file', 'child', file.fsaPath[last]!)
    }
  }
  return coveredDirectories
}

function collectCompletedFileParentRenames(
  entries: Map<string, SafeRenamedEntry>,
  coveredDirectories: Set<string>,
  logical: readonly string[],
  physical: readonly string[],
): void {
  for (let depth = 1; depth < logical.length; depth += 1) {
    const logicalPrefix = logical.slice(0, depth)
    coveredDirectories.add(pathKey(logicalPrefix))
    if (physical[depth - 1] !== logical[depth - 1]) {
      recordEntryIfAbsent(entries, logicalPrefix, 'directory', 'child', physical[depth - 1]!)
    }
  }
}

function collectCandidateDirectoryRenames(
  entries: Map<string, SafeRenamedEntry>,
  coveredDirectories: ReadonlySet<string>,
  candidateDirectories: readonly SafeRenamedDirectoryRecord[] | undefined,
): readonly SafeRenamedDirectoryRecord[] | undefined {
  const retainedDirs = new Map<string, SafeRenamedDirectoryRecord>()
  for (const dir of candidateDirectories ?? []) {
    if (dir.logicalPath.length === 0 || dir.logicalPath.length !== dir.physicalPath.length) continue
    if (!recordRenamedDirectoryPrefixes(entries, dir)) continue
    const dirKey = pathKey(dir.logicalPath)
    if (!coveredDirectories.has(dirKey) && !retainedDirs.has(dirKey)) {
      retainedDirs.set(dirKey, freezeDirectoryRecord(dir.logicalPath, dir.physicalPath))
    }
  }
  return retainedDirs.size === 0 ? undefined : Object.freeze([...retainedDirs.values()])
}

function recordRenamedDirectoryPrefixes(
  entries: Map<string, SafeRenamedEntry>,
  dir: SafeRenamedDirectoryRecord,
): boolean {
  let hasRename = false
  for (let depth = 1; depth <= dir.logicalPath.length; depth += 1) {
    if (dir.physicalPath[depth - 1] === dir.logicalPath[depth - 1]) continue
    hasRename = true
    recordEntryIfAbsent(entries, dir.logicalPath.slice(0, depth), 'directory', 'child', dir.physicalPath[depth - 1]!)
  }
  return hasRename
}

export function mergeRenamedDirectories(
  prior: readonly SafeRenamedDirectoryRecord[] | undefined,
  current: readonly SafeRenamedDirectoryRecord[] | undefined,
): readonly SafeRenamedDirectoryRecord[] | undefined {
  const merged = new Map<string, SafeRenamedDirectoryRecord>()
  for (const dir of [...(prior ?? []), ...(current ?? [])]) {
    if (dir.logicalPath.length === 0 || dir.logicalPath.length !== dir.physicalPath.length) continue
    merged.set(pathKey(dir.logicalPath), freezeDirectoryRecord(dir.logicalPath, dir.physicalPath))
  }
  return merged.size === 0 ? undefined : Object.freeze([...merged.values()])
}

export function sameSafeRenamedState(
  left: CollectedSafeRenamedState,
  right: CollectedSafeRenamedState,
): boolean {
  if (left.summary?.count !== right.summary?.count) return false
  const leftSample = left.summary?.sample ?? []
  const rightSample = right.summary?.sample ?? []
  if (leftSample.length !== rightSample.length) return false
  for (let i = 0; i < leftSample.length; i += 1) {
    if (leftSample[i]!.physicalName !== rightSample[i]!.physicalName) return false
    if (pathKey(leftSample[i]!.logicalPath) !== pathKey(rightSample[i]!.logicalPath)) return false
  }
  const leftDirs = left.directories ?? []
  const rightDirs = right.directories ?? []
  if (leftDirs.length !== rightDirs.length) return false
  for (let i = 0; i < leftDirs.length; i += 1) {
    if (pathKey(leftDirs[i]!.logicalPath) !== pathKey(rightDirs[i]!.logicalPath)) return false
    if (pathKey(leftDirs[i]!.physicalPath) !== pathKey(rightDirs[i]!.physicalPath)) return false
  }
  return true
}

export function applySafeRenamedFields<T extends ReceiveTaskRecord>(
  record: T,
  safe: CollectedSafeRenamedState,
): T {
  const next = { ...record } as Record<string, unknown>
  if (safe.summary === undefined) delete next.safeRenamed
  else next.safeRenamed = safe.summary
  if (safe.directories === undefined) delete next.safeRenamedDirectories
  else next.safeRenamedDirectories = safe.directories
  return Object.freeze(next) as T
}

function summarizeEntries(entries: ReadonlyMap<string, SafeRenamedEntry>): SafeRenamedSummary | undefined {
  if (entries.size === 0) return undefined
  const sample: SafeRenamedEntry[] = []
  for (const entry of entries.values()) {
    if (sample.length >= MAX_SAFE_RENAMED_SAMPLE) break
    sample.push(entry)
  }
  return safeRenamedSummary({ count: entries.size, sample })
}

function freezeDirectoryRecord(
  logicalPath: readonly string[],
  physicalPath: readonly string[],
): SafeRenamedDirectoryRecord {
  return Object.freeze({
    logicalPath: Object.freeze([...logicalPath]),
    physicalPath: Object.freeze([...physicalPath]),
  })
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

function pathKey(path: readonly string[]): string {
  return JSON.stringify(path)
}

function entryKey(
  logicalPath: readonly string[],
  entryKind: SafeNameEntryKind,
  scope: SafeNameEntryScope,
): string {
  return `${scope}\0${entryKind}\0${JSON.stringify(logicalPath)}`
}
