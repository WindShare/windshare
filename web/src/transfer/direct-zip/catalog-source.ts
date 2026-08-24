import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type { V2CatalogClient } from '../../catalog/v2-client'
import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import type { V2CatalogEntry, V2ShareDescriptor } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import { artifactDirectoryPath, artifactFilePath } from '../job/artifact-path'
import type { AuthenticatedDirectory, PendingFile } from '../job/contract'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../job/coordinate/direct-tree'
import type {
  DirectZipAuthenticatedRootV1,
  DirectZipIntent,
  DirectZipOrderedMemberV1,
  DirectZipOrderedSourceV1,
} from './model'

const TEXT_ENCODER = new TextEncoder()

interface CatalogFrontierEntry {
  readonly entry: V2CatalogEntry
  readonly parent: V2CommittedDirectory
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[] | undefined
  readonly ancestry: readonly string[]
  readonly iterator: AsyncIterator<V2CatalogEntry>
}

interface ResolvedCatalogCandidate {
  readonly member?: DirectZipOrderedMemberV1
  readonly child?: V2CommittedDirectory
}

export interface DirectZipCatalogSourceOptionsV1 {
  readonly catalog: Pick<V2CatalogClient, 'loadDirectory' | 'entries'>
  readonly descriptor: V2ShareDescriptor
  readonly selection: V2FrozenSelectionPolicy
  readonly intent: DirectZipIntent
  readonly maximumNodeClaims: number
}

/**
 * A heap of one lookahead entry per authenticated directory produces global ZIP
 * path order without retaining a wide generation or trusting worker completion order.
 */
export class DirectZipCatalogSourceV1 implements DirectZipOrderedSourceV1 {
  readonly #options: DirectZipCatalogSourceOptionsV1
  readonly #selectedRuleIds: Set<string>
  readonly #observedSelectedRuleIds = new Set<string>()
  readonly #claimedNodeIds = new Set<string>()
  #root: V2CommittedDirectory | undefined

  constructor(options: DirectZipCatalogSourceOptionsV1) {
    if (!Number.isSafeInteger(options.maximumNodeClaims) || options.maximumNodeClaims <= 0) {
      throw new RangeError('direct ZIP catalog node budget must be a positive exact integer')
    }
    if (options.intent.plan.kind !== 'direct-resumable-zip' ||
        options.intent.artifact.kind !== 'zip-archive') {
      throw new TypeError('direct ZIP catalog source requires a frozen direct ZIP intent')
    }
    this.#options = options
    this.#selectedRuleIds = new Set(options.selection.canonicalRules
      .filter(rule => rule.selected)
      .map(rule => encodeBase64Url(rule.id)))
  }

  async root(signal: AbortSignal): Promise<DirectZipAuthenticatedRootV1> {
    const committed = await this.#loadRoot(signal)
    return Object.freeze({
      directoryId: committed.directoryIdText,
      generation: committed.generationText,
      discoveryEvidence: evidenceBytes('root', [
        committed.directoryIdText,
        committed.generationText,
        committed.pageCount.toString(),
      ]),
    })
  }

  async *members(signal: AbortSignal): AsyncGenerator<DirectZipOrderedMemberV1> {
    const root = await this.#loadRoot(signal)
    const frontier: CatalogFrontierEntry[] = []
    await this.#pushFirst(frontier, root, [], [], signal)
    let previous: DirectZipOrderedMemberV1 | undefined
    while (frontier.length > 0) {
      signal.throwIfAborted()
      const candidate = heapPop(frontier)!
      await this.#pushNext(frontier, candidate, signal)
      const selected = this.#options.selection.selected(candidate.entry, candidate.ancestry)
      if (this.#selectedRuleIds.has(candidate.entry.idText)) {
        this.#observedSelectedRuleIds.add(candidate.entry.idText)
      }
      const resolved = await this.#resolveCandidate(candidate, selected, signal)
      if (resolved.member !== undefined) {
        const member = resolved.member
        requireCanonicalSuccessor(previous, member)
        previous = member
        yield member
      }
      if (resolved.child !== undefined) {
        await this.#pushFirst(
          frontier,
          resolved.child,
          candidate.sourcePath,
          [...candidate.ancestry, candidate.entry.idText],
          signal,
        )
      }
    }
    const missing = [...this.#selectedRuleIds]
      .filter(identity => !this.#observedSelectedRuleIds.has(identity))
    if (missing.length > 0) {
      throw new Error('direct ZIP authenticated discovery did not resolve every selected identity')
    }
  }

  async #resolveCandidate(
    candidate: CatalogFrontierEntry,
    selected: boolean,
    signal: AbortSignal,
  ): Promise<ResolvedCatalogCandidate> {
    if (candidate.entry.kind === 'file') {
      if (!selected) return Object.freeze({})
      if (candidate.artifactPath === undefined) {
        throw new TypeError('selected file escapes the frozen ZIP result-root anchor')
      }
      return Object.freeze({ member: this.#fileMember(candidate) })
    }

    let child: V2CommittedDirectory | undefined
    if (this.#options.selection.shouldDiscover(candidate.entry.idText, candidate.ancestry)) {
      child = await this.#loadDirectory(candidate.entry, signal)
    }
    if (!selected) return child === undefined ? Object.freeze({}) : Object.freeze({ child })
    if (candidate.artifactPath === undefined) {
      throw new TypeError('selected directory escapes the frozen ZIP result-root anchor')
    }
    child ??= await this.#loadDirectory(candidate.entry, signal)
    const member = this.#directoryMember(candidate, child)
    return member === undefined ? Object.freeze({ child }) : Object.freeze({ child, member })
  }

  async #loadRoot(signal: AbortSignal): Promise<V2CommittedDirectory> {
    if (this.#root !== undefined) return this.#root
    const committed = await this.#options.catalog.loadDirectory(
      this.#options.descriptor.syntheticRoot,
      { signal },
    )
    requireCommittedDirectory(committed, this.#options.descriptor.syntheticRoot)
    this.#claim(committed.directoryIdText)
    if (this.#selectedRuleIds.has(committed.directoryIdText)) {
      this.#observedSelectedRuleIds.add(committed.directoryIdText)
    }
    this.#root = committed
    return committed
  }

  async #loadDirectory(
    entry: Extract<V2CatalogEntry, { readonly kind: 'directory' }>,
    signal: AbortSignal,
  ): Promise<V2CommittedDirectory> {
    this.#claim(entry.idText)
    const committed = await this.#options.catalog.loadDirectory(entry.id, { signal })
    requireCommittedDirectory(committed, entry.id)
    return committed
  }

  #claim(identity: string): void {
    if (this.#claimedNodeIds.has(identity)) {
      throw new Error('direct ZIP catalog traversal revisited a directory identity')
    }
    if (this.#claimedNodeIds.size >= this.#options.maximumNodeClaims) {
      throw new Error('direct ZIP catalog traversal exhausted its identity budget')
    }
    this.#claimedNodeIds.add(identity)
  }

  async #pushFirst(
    frontier: CatalogFrontierEntry[],
    parent: V2CommittedDirectory,
    parentPath: readonly string[],
    ancestry: readonly string[],
    signal: AbortSignal,
  ): Promise<void> {
    const iterator = this.#options.catalog.entries(parent, signal)[Symbol.asyncIterator]()
    const next = await iterator.next()
    if (next.done) return
    heapPush(frontier, this.#candidate(next.value, parent, parentPath, ancestry, iterator))
  }

  async #pushNext(
    frontier: CatalogFrontierEntry[],
    previous: CatalogFrontierEntry,
    signal: AbortSignal,
  ): Promise<void> {
    signal.throwIfAborted()
    const next = await previous.iterator.next()
    if (next.done) return
    const candidate = this.#candidate(
      next.value,
      previous.parent,
      previous.sourcePath.slice(0, -1),
      previous.ancestry,
      previous.iterator,
    )
    if (comparePath(candidate.sourcePath, candidate.entry.kind, previous.sourcePath, previous.entry.kind) <= 0) {
      throw new Error('direct ZIP committed directory entries are not in canonical order')
    }
    heapPush(frontier, candidate)
  }

  #candidate(
    entry: V2CatalogEntry,
    parent: V2CommittedDirectory,
    parentPath: readonly string[],
    ancestry: readonly string[],
    iterator: AsyncIterator<V2CatalogEntry>,
  ): CatalogFrontierEntry {
    const sourcePath = snapshotPortableCatalogPath([...parentPath, entry.name])
    const artifactPath = projectedArtifactPath(this.#options.intent, sourcePath, entry.kind)
    return Object.freeze({ entry, parent, sourcePath, artifactPath, ancestry, iterator })
  }

  #directoryMember(
    candidate: CatalogFrontierEntry,
    committed: V2CommittedDirectory,
  ): Extract<DirectZipOrderedMemberV1, { kind: 'directory' }> | undefined {
    if (candidate.entry.kind !== 'directory') {
      throw new TypeError('direct ZIP directory candidate changed kind')
    }
    const path = requireArtifactPath(candidate)
    const markerRoot = this.#options.intent.artifact.layout.name
    if (path.length === 0 || (path.length === 1 && path[0] === markerRoot)) return undefined
    return Object.freeze({
      kind: 'directory',
      directoryId: candidate.entry.idText,
      generation: committed.generationText,
      sourcePath: candidate.sourcePath,
      artifactPath: path,
      ...(candidate.entry.modifiedTime === undefined ? {} : { modifiedTime: candidate.entry.modifiedTime }),
      layoutEvidence: evidenceBytes('directory-layout', [path.join('/')]),
      discoveryEvidence: evidenceBytes('directory-discovery', [
        candidate.parent.directoryIdText,
        candidate.parent.generationText,
        candidate.entry.idText,
        committed.generationText,
        candidate.sourcePath.join('/'),
      ]),
    })
  }

  #fileMember(candidate: CatalogFrontierEntry): Extract<DirectZipOrderedMemberV1, { kind: 'file' }> {
    if (candidate.entry.kind !== 'file') throw new TypeError('direct ZIP file candidate changed kind')
    const artifactPath = requireArtifactPath(candidate)
    const parent: AuthenticatedDirectory = Object.freeze({
      kind: 'reference',
      directoryId: candidate.parent.directoryIdText,
      generation: candidate.parent.generationText,
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(candidate.sourcePath.slice(0, -1)),
      logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath.slice(0, -1)),
    })
    const pending: PendingFile = Object.freeze({
      entry: candidate.entry,
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(candidate.sourcePath),
      logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath),
      materializationRelativePath: snapshotMaterializationRootRelativePath(artifactPath),
      parent,
      ...(candidate.entry.modifiedTime === undefined ? {} : { modifiedTime: candidate.entry.modifiedTime }),
      ready: Promise.resolve(),
    })
    return Object.freeze({
      kind: 'file',
      fileId: candidate.entry.idText,
      expectedSize: candidate.entry.expectedSize,
      sourcePath: candidate.sourcePath,
      artifactPath,
      ...(candidate.entry.modifiedTime === undefined ? {} : { modifiedTime: candidate.entry.modifiedTime }),
      layoutEvidence: evidenceBytes('file-layout', [
        artifactPath.join('/'),
        candidate.entry.expectedSize.toString(),
      ]),
      discoveryEvidence: evidenceBytes('file-discovery', [
        candidate.parent.directoryIdText,
        candidate.parent.generationText,
        candidate.entry.idText,
        candidate.sourcePath.join('/'),
      ]),
      pending,
    })
  }
}

function requireCommittedDirectory(directory: V2CommittedDirectory, expectedId: Uint8Array): void {
  if (!equalBytes(directory.directoryId, expectedId) || directory.omittedCount !== 0n ||
      directory.generation.byteLength !== 16 || directory.generation.every(byte => byte === 0)) {
    throw new Error('direct ZIP catalog generation lost authenticated directory authority')
  }
}

function evidenceBytes(domain: string, fields: readonly string[]): Uint8Array<ArrayBuffer> {
  return TEXT_ENCODER.encode(JSON.stringify([`windshare/direct-zip/${domain}/v1`, ...fields]))
}

function requireCanonicalSuccessor(
  previous: DirectZipOrderedMemberV1 | undefined,
  next: DirectZipOrderedMemberV1,
): void {
  if (previous !== undefined && comparePath(
    previous.artifactPath,
    previous.kind,
    next.artifactPath,
    next.kind,
  ) >= 0) {
    throw new Error('direct ZIP ordered source produced a non-canonical successor')
  }
}

function compareFrontier(left: CatalogFrontierEntry, right: CatalogFrontierEntry): number {
  const order = comparePath(
    left.artifactPath ?? EMPTY_PATH,
    left.entry.kind,
    right.artifactPath ?? EMPTY_PATH,
    right.entry.kind,
  )
  return order !== 0 ? order : left.entry.idText.localeCompare(right.entry.idText)
}

const EMPTY_PATH: readonly string[] = Object.freeze([])

function projectedArtifactPath(
  intent: DirectZipIntent,
  sourcePath: readonly string[],
  kind: V2CatalogEntry['kind'],
): readonly string[] | undefined {
  const anchor = intent.artifact.layout.anchor
  if (anchor.kind === 'directory') {
    const anchorPath = anchor.sourcePath.split('/')
    if (!startsWith(sourcePath, anchorPath) && !startsWith(anchorPath, sourcePath)) return undefined
  }
  return kind === 'directory'
    ? artifactDirectoryPath(intent, sourcePath)
    : artifactFilePath(intent, sourcePath)
}

function requireArtifactPath(candidate: CatalogFrontierEntry): readonly string[] {
  if (candidate.artifactPath === undefined) {
    throw new TypeError('selected catalog entry escapes the frozen ZIP result-root anchor')
  }
  return candidate.artifactPath
}

function startsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return path.length >= prefix.length && prefix.every((segment, index) => path[index] === segment)
}

function comparePath(
  leftPath: readonly string[],
  leftKind: 'directory' | 'file',
  rightPath: readonly string[],
  rightKind: 'directory' | 'file',
): number {
  const left = TEXT_ENCODER.encode(leftPath.join('/'))
  const right = TEXT_ENCODER.encode(rightPath.join('/'))
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = left[index]! - right[index]!
    if (difference !== 0) return difference
  }
  if (left.byteLength !== right.byteLength) return left.byteLength - right.byteLength
  if (leftKind === rightKind) return 0
  return leftKind === 'directory' ? -1 : 1
}

function heapPush(heap: CatalogFrontierEntry[], value: CatalogFrontierEntry): void {
  heap.push(value)
  let index = heap.length - 1
  while (index > 0) {
    const parent = Math.floor((index - 1) / 2)
    if (compareFrontier(heap[parent]!, value) <= 0) break
    heap[index] = heap[parent]!
    index = parent
  }
  heap[index] = value
}

function heapPop(heap: CatalogFrontierEntry[]): CatalogFrontierEntry | undefined {
  const root = heap[0]
  const tail = heap.pop()
  if (root === undefined || tail === undefined || heap.length === 0) return root
  let index = 0
  while (true) {
    const left = index * 2 + 1
    if (left >= heap.length) break
    const right = left + 1
    const child = right < heap.length && compareFrontier(heap[right]!, heap[left]!) < 0 ? right : left
    if (compareFrontier(heap[child]!, tail) >= 0) break
    heap[index] = heap[child]!
    index = child
  }
  heap[index] = tail
  return root
}
