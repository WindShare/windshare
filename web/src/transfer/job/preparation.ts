import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import type { V2CatalogEntry } from '../../catalog/v2-records'
import { encodeBase64Url } from '../../crypto/bytes'
import type { AuthenticatedGenerationReference } from '../../output/workspace/manifest'
import type {
  PreparationDirectoryEntry,
  PreparationDirectoryRole,
  PreparationFileEntry,
  PreparationManifestEntry,
} from '../../output/workspace/preparation'
import {
  snapshotCanonicalModifiedTime,
  snapshotMaterializationPath,
} from '../directory-admission'
import type { ReceiveIntent } from '../intent'
import type { ExactPreparationEvidence } from '../output-session'
import { directoryIsResultRoot } from './artifact-path'
import type {
  AuthenticatedDirectory,
  DirectoryCursor,
  PendingFile,
} from './contract'

const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const UTF8_ENCODER = new TextEncoder()

interface MutablePreparedDirectory {
  entry: PreparationDirectoryEntry
}

/** Collects bounded authenticated catalog facts without opening a file revision or content lane. */
export class ExactPreparationCollector {
  readonly #intent: ReceiveIntent
  readonly #generations = new Map<string, AuthenticatedGenerationReference>()
  readonly #directories = new Map<string, MutablePreparedDirectory>()
  readonly #files: PreparationFileEntry[] = []
  readonly #pendingFiles: PendingFile[] = []
  #selectedRawBytes = 0n

  constructor(intent: ReceiveIntent) {
    this.#intent = intent
  }

  observeGeneration(cursor: DirectoryCursor, committed: V2CommittedDirectory): void {
    const generation = encodeBase64Url(committed.generation)
    const prior = this.#generations.get(cursor.idText)
    if (prior !== undefined && prior.generation !== generation) {
      throw new TypeError('preparation observed conflicting authenticated generations')
    }
    this.#generations.set(cursor.idText, Object.freeze({
      directoryId: cursor.idText,
      generation,
    }))
  }

  materializeDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    artifactPath: readonly string[],
    requestedRole: 'selected' | 'ancestor',
  ): AuthenticatedDirectory {
    this.observeGeneration(cursor, committed)
    const generation = encodeBase64Url(committed.generation)
    const role = this.#role(cursor, requestedRole)
    const existing = this.#directories.get(cursor.idText)
    if (existing === undefined) {
      const entry: PreparationDirectoryEntry = Object.freeze({
        kind: 'directory',
        directoryId: cursor.idText,
        generation,
        sourcePath: snapshotMaterializationPath(cursor.path),
        artifactPath: snapshotMaterializationPath(artifactPath),
        role,
        ...(cursor.modifiedTime === undefined
          ? {}
          : { modifiedTime: snapshotCanonicalModifiedTime(cursor.modifiedTime) }),
      })
      this.#directories.set(cursor.idText, { entry })
    } else if (existing.entry.generation !== generation ||
        !samePath(existing.entry.sourcePath, cursor.path) ||
        !samePath(existing.entry.artifactPath, artifactPath)) {
      throw new TypeError('preparation directory identity was rebound')
    } else if (existing.entry.role === 'explicitly-selected-empty' && role === 'necessary-ancestor') {
      existing.entry = Object.freeze({ ...existing.entry, role })
    }
    const current = this.#directories.get(cursor.idText)?.entry
    if (current === undefined) throw new TypeError('preparation lost a directory entry')
    return Object.freeze({
      directoryId: cursor.idText,
      generation,
      sourcePath: current.sourcePath,
      artifactPath: current.artifactPath,
      ...(current.modifiedTime === undefined ? {} : { modifiedTime: current.modifiedTime }),
    })
  }

  referenceDirectory(
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
  ): AuthenticatedDirectory {
    this.observeGeneration(cursor, committed)
    return Object.freeze({
      directoryId: cursor.idText,
      generation: encodeBase64Url(committed.generation),
      sourcePath: snapshotMaterializationPath(cursor.path),
      artifactPath: Object.freeze([]),
      ...(cursor.modifiedTime === undefined
        ? {}
        : { modifiedTime: snapshotCanonicalModifiedTime(cursor.modifiedTime) }),
    })
  }

  addFile(
    entry: Extract<V2CatalogEntry, { kind: 'file' }>,
    sourcePath: readonly string[],
    artifactPath: readonly string[],
    parent: AuthenticatedDirectory,
  ): void {
    const exactSize = checkedU64(entry.expectedSize, 'prepared file size')
    this.#selectedRawBytes = checkedAdd(this.#selectedRawBytes, exactSize, 'prepared selected bytes')
    const prepared: PreparationFileEntry = Object.freeze({
      kind: 'file',
      fileId: entry.idText,
      containingDirectoryId: parent.directoryId,
      generation: parent.generation,
      sourcePath: snapshotMaterializationPath(sourcePath),
      artifactPath: snapshotMaterializationPath(artifactPath),
      exactSize,
      ...(entry.modifiedTime === undefined
        ? {}
        : { modifiedTime: snapshotCanonicalModifiedTime(entry.modifiedTime) }),
    })
    this.#files.push(prepared)
    this.#pendingFiles.push(Object.freeze({
      entry,
      sourcePath: prepared.sourcePath,
      artifactPath: prepared.artifactPath,
      parent,
      ready: Promise.resolve(),
      ...(entry.modifiedTime === undefined ? {} : { modifiedTime: entry.modifiedTime }),
    }))
  }

  pendingFiles(): readonly PendingFile[] {
    return Object.freeze([...this.#pendingFiles])
  }

  evidence(): ExactPreparationEvidence {
    const generations = [...this.#generations.values()].sort((left, right) =>
      compareUTF8(left.directoryId, right.directoryId) || compareUTF8(left.generation, right.generation))
    const entries: PreparationManifestEntry[] = [
      ...[...this.#directories.values()].map(value => value.entry),
      ...this.#files,
    ]
    entries.sort(comparePreparationEntries)
    return Object.freeze({
      generations: Object.freeze(generations),
      entries: Object.freeze(entries),
      entryCount: BigInt(entries.length),
      fileCount: BigInt(this.#files.length),
      directoryCount: BigInt(this.#directories.size),
      selectedRawBytes: this.#selectedRawBytes,
    })
  }

  #role(
    cursor: DirectoryCursor,
    requestedRole: 'selected' | 'ancestor',
  ): PreparationDirectoryRole {
    if (directoryIsResultRoot(this.#intent, cursor.path)) return 'result-root'
    return requestedRole === 'selected' ? 'explicitly-selected-empty' : 'necessary-ancestor'
  }
}

function comparePreparationEntries(left: PreparationManifestEntry, right: PreparationManifestEntry): number {
  const path = compareUTF8(left.artifactPath.join('/'), right.artifactPath.join('/'))
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function compareUTF8(left: string, right: string): number {
  const a = UTF8_ENCODER.encode(left)
  const b = UTF8_ENCODER.encode(right)
  const length = Math.min(a.byteLength, b.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = a[index]! - b[index]!
    if (difference !== 0) return difference
  }
  return a.byteLength - b.byteLength
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}

function checkedAdd(left: bigint, right: bigint, label: string): bigint {
  const sum = left + right
  if (sum > U64_MAXIMUM) throw new TypeError(`${label} overflowed u64`)
  return sum
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => right[index] === segment)
}
