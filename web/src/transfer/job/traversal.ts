import { catalogFailure } from '../../catalog/failure-evidence'
import { catalogNameCollisionKey, snapshotCatalogPath } from '../../catalog/path-policy'
import type { CommittedDirectory } from '../../catalog/page-store'
import { MAXIMUM_DIRECTORY_PAGES } from '../../catalog/client'
import {
  CATALOG_COMMITMENT_BYTES,
  CATALOG_DIRECTORY_ENTRIES,
  CATALOG_IDENTITY_BYTES,
  CATALOG_PAGE_ENTRIES,
  type CatalogEntry,
  type CatalogPage,
} from '../../catalog/records'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  CatalogTraversalError,
  DirectoryTraversalError,
  DirectoryAncestry,
  type DirectoryCursor,
} from './contract'

/** Owns identity and ancestry invariants for one authenticated catalog walk. */
export class CatalogTraversalGuard {
  readonly #shareInstance: Uint8Array<ArrayBuffer>
  readonly #maximumNodeClaims: number
  readonly #ancestry = new DirectoryAncestry()
  readonly #claimedNodes = new Set<string>()

  constructor(shareInstance: Uint8Array<ArrayBuffer>, maximumNodeClaims: number) {
    this.#shareInstance = shareInstance
    this.#maximumNodeClaims = maximumNodeClaims
  }

  enterDirectory(directoryIdText: string): () => void {
    return this.#ancestry.enter(directoryIdText)
  }

  entryPath(cursor: DirectoryCursor, entry: CatalogEntry): readonly string[] {
    try {
      return snapshotCatalogPath([...cursor.path, entry.name])
    } catch (cause) {
      throw new DirectoryTraversalError('Catalog entry exceeded the protocol path policy', { cause })
    }
  }

  claimNode(nodeId: Uint8Array): void {
    const text = encodeBase64Url(nodeId)
    if (this.#claimedNodes.size >= this.#maximumNodeClaims) {
      throw catalogFailure(new CatalogTraversalError('Catalog node identity budget was exhausted'), {
        invariant: 'node-budget', expected: this.#maximumNodeClaims, actual: this.#claimedNodes.size, nodeId: text,
      })
    }
    if (!nodeId.some((value) => value !== 0)) {
      throw catalogFailure(new CatalogTraversalError('Catalog node identity is zero'), { invariant: 'nonzero-node', nodeId: text })
    }
    if (this.#claimedNodes.has(text)) {
      throw catalogFailure(new CatalogTraversalError('Catalog traversal reused a node identity'), { invariant: 'unique-node', nodeId: text })
    }
    this.#claimedNodes.add(text)
  }

  pageCursor(cursor: DirectoryCursor, committed: CommittedDirectory): CatalogPageCursor {
    return new CatalogPageCursor(this.#shareInstance, cursor, committed)
  }
}

/** Revalidates the committed page cursor at the injected catalog consumer boundary. */
export class CatalogPageCursor {
  readonly #shareInstance: Uint8Array<ArrayBuffer>
  readonly #cursor: DirectoryCursor
  readonly #committed: CommittedDirectory
  #nextPageIndex = 0
  #entryCount = 0
  #previousCommitment = new Uint8Array(CATALOG_COMMITMENT_BYTES)
  #lastName = new Uint8Array()
  readonly #seenNames = new Set<string>()
  readonly #seenNodes = new Set<string>()

  constructor(
    shareInstance: Uint8Array<ArrayBuffer>,
    cursor: DirectoryCursor,
    committed: CommittedDirectory,
  ) {
    if (!Number.isSafeInteger(committed.pageCount) || committed.pageCount <= 0 ||
        committed.pageCount > MAXIMUM_DIRECTORY_PAGES ||
        !Number.isSafeInteger(committed.entryCount) || committed.entryCount < 0 ||
        committed.entryCount > CATALOG_DIRECTORY_ENTRIES ||
        typeof committed.omittedCount !== 'bigint' || committed.omittedCount < 0n ||
        committed.omittedCount > BigInt(CATALOG_DIRECTORY_ENTRIES - committed.entryCount) ||
        committed.terminalCommitment.byteLength !== CATALOG_COMMITMENT_BYTES ||
        committed.terminalCommitment.every((byte) => byte === 0)) {
      throw catalogFailure(new CatalogTraversalError('Committed catalog page cursor is malformed'), {
        invariant: 'committed-cursor', directoryId: cursor.idText, pageCount: committed.pageCount, entryCount: committed.entryCount,
        omittedCount: String(committed.omittedCount), terminalCommitmentBytes: committed.terminalCommitment.byteLength,
      })
    }
    this.#shareInstance = shareInstance
    this.#cursor = cursor
    this.#committed = committed
  }

  accept(page: CatalogPage): void {
    const terminal = this.#nextPageIndex === this.#committed.pageCount - 1
    if (page.pageIndex !== this.#nextPageIndex || page.terminal !== terminal ||
        !equalBytes(page.directoryId, this.#cursor.id) ||
        !equalBytes(page.generation, this.#committed.generation) ||
        !equalBytes(page.shareInstance, this.#shareInstance) ||
        !equalBytes(page.previousCommitment, this.#previousCommitment) ||
        page.objectCommitment.byteLength !== CATALOG_COMMITMENT_BYTES ||
        page.objectCommitment.every((byte) => byte === 0) ||
        !Array.isArray(page.entries) || page.entries.length > CATALOG_PAGE_ENTRIES ||
        (page.entries.length === 0 && !(page.pageIndex === 0 && page.terminal)) ||
        typeof page.omittedCount !== 'bigint' || page.omittedCount < 0n ||
        (!terminal && page.omittedCount !== 0n) ||
        (terminal && page.omittedCount !== this.#committed.omittedCount)) {
      this.#rejectPageAuthority(page, terminal)
    }
    const pendingNames = new Set<string>()
    const pendingNodes = new Set<string>()
    let lastName = this.#lastName
    for (const entry of page.entries) {
      const authority = catalogEntryAuthority(entry)
      if ((lastName.byteLength > 0 && compareBytes(lastName, authority.nameBytes) >= 0) ||
          this.#seenNames.has(authority.nameKey) || pendingNames.has(authority.nameKey) ||
          this.#seenNodes.has(authority.nodeKey) || pendingNodes.has(authority.nodeKey)) {
        throw catalogFailure(new CatalogTraversalError(
          'Catalog entries changed canonical order or repeated a portable sibling identity',
        ), { invariant: 'canonical-unique-sibling', directoryId: this.#cursor.idText, pageIndex: page.pageIndex,
          name: entry.name, nodeId: entry.idText, previousName: new TextDecoder().decode(lastName),
          duplicateName: this.#seenNames.has(authority.nameKey) || pendingNames.has(authority.nameKey),
          duplicateNode: this.#seenNodes.has(authority.nodeKey) || pendingNodes.has(authority.nodeKey),
        })
      }
      pendingNames.add(authority.nameKey)
      pendingNodes.add(authority.nodeKey)
      lastName = authority.nameBytes
    }
    const nextEntryCount = this.#entryCount + page.entries.length
    if (!Number.isSafeInteger(nextEntryCount) || nextEntryCount > this.#committed.entryCount ||
        (terminal && (!equalBytes(page.objectCommitment, this.#committed.terminalCommitment) ||
          nextEntryCount !== this.#committed.entryCount))) {
      throw catalogFailure(new CatalogTraversalError('Catalog page count or terminal commitment changed'), {
        invariant: 'terminal-commitment', directoryId: this.#cursor.idText, expectedEntries: this.#committed.entryCount, actualEntries: nextEntryCount,
        expectedCommitment: encodeBase64Url(this.#committed.terminalCommitment), actualCommitment: encodeBase64Url(page.objectCommitment),
      })
    }
    for (const name of pendingNames) this.#seenNames.add(name)
    for (const node of pendingNodes) this.#seenNodes.add(node)
    this.#lastName = lastName
    this.#entryCount = nextEntryCount
    this.#previousCommitment = page.objectCommitment.slice()
    this.#nextPageIndex += 1
  }

  #rejectPageAuthority(page: CatalogPage, terminal: boolean): never {
      throw catalogFailure(new CatalogTraversalError('Catalog page changed its committed cursor authority'), {
        directoryId: this.#cursor.idText, expectedPage: this.#nextPageIndex, actualPage: page.pageIndex,
        expectedTerminal: terminal, actualTerminal: page.terminal,
        directoryMatches: equalBytes(page.directoryId, this.#cursor.id), generationMatches: equalBytes(page.generation, this.#committed.generation),
        shareMatches: equalBytes(page.shareInstance, this.#shareInstance), previousCommitmentMatches: equalBytes(page.previousCommitment, this.#previousCommitment),
        commitmentBytes: page.objectCommitment.byteLength, zeroCommitment: page.objectCommitment.every(byte => byte === 0),
        entries: Array.isArray(page.entries) ? page.entries.length : null, maximumEntries: CATALOG_PAGE_ENTRIES,
        expectedOmitted: String(terminal ? this.#committed.omittedCount : 0n), actualOmitted: String(page.omittedCount),
      })
  }

  finish(): void {
    if (this.#nextPageIndex !== this.#committed.pageCount ||
        this.#entryCount !== this.#committed.entryCount) {
      throw catalogFailure(new CatalogTraversalError('Catalog page cursor ended before its committed terminal page'), {
        invariant: 'complete-cursor', directoryId: this.#cursor.idText, expectedPages: this.#committed.pageCount, actualPages: this.#nextPageIndex,
        expectedEntries: this.#committed.entryCount, actualEntries: this.#entryCount,
      })
    }
  }
}

const TEXT_ENCODER = new TextEncoder()

function catalogEntryAuthority(entry: CatalogEntry): {
  readonly nameBytes: Uint8Array<ArrayBuffer>
  readonly nameKey: string
  readonly nodeKey: string
} {
  if (!(entry.id instanceof Uint8Array) || entry.id.byteLength !== CATALOG_IDENTITY_BYTES ||
      entry.id.every((byte) => byte === 0) || entry.idText !== encodeBase64Url(entry.id) ||
      typeof entry.name !== 'string' ||
      (entry.kind !== 'directory' &&
        (entry.kind !== 'file' || typeof entry.expectedSize !== 'bigint' || entry.expectedSize < 0n))) {
    throw catalogFailure(new CatalogTraversalError('Catalog page contains a malformed entry authority'), {
      invariant: 'entry-authority', nodeId: entry.idText, name: entry.name, kind: entry.kind,
      identityBytes: entry.id instanceof Uint8Array ? entry.id.byteLength : null, expectedIdentityBytes: CATALOG_IDENTITY_BYTES,
    })
  }
  let nameKey: string
  try {
    nameKey = catalogNameCollisionKey(entry.name)
  } catch (cause) {
    throw new CatalogTraversalError('Catalog page contains an invalid catalog sibling name', { cause })
  }
  return {
    nameBytes: TEXT_ENCODER.encode(entry.name),
    nameKey,
    nodeKey: encodeBase64Url(entry.id),
  }
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const shared = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < shared; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0)
    if (difference !== 0) return difference
  }
  return left.byteLength - right.byteLength
}
