import { canonicalizePortableCatalogPath } from '../../catalog/path-policy'
import type { CatalogClient } from '../../catalog/client'
import type { CommittedDirectory } from '../../catalog/page-store'
import {
  CATALOG_IDENTITY_BYTES,
  type CatalogEntry,
  type ShareDescriptor,
} from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  snapshotCanonicalModifiedTime,
} from '../directory-admission'
import type { OriginalFileArtifact } from '../intent'
import type { ExactSingleFileEvidence } from '../output/session'
import { CatalogTraversalError } from './contract'

export interface ExactSingleFileCatalogEvidenceOptions {
  readonly catalog: Pick<CatalogClient, 'loadDirectory' | 'pages'>
  readonly descriptor: Pick<
    ShareDescriptor,
    'shareInstance' | 'syntheticRoot' | 'syntheticRootId'
  >
  readonly selection: FrozenSelectionPolicy
  readonly artifact: OriginalFileArtifact
  readonly signal: AbortSignal
  readonly root?: CommittedDirectory
}

/**
 * Resolves only the frozen OriginalFile path. The full selection traversal may
 * continue concurrently after admission, so this proof cannot become a hidden
 * whole-tree preparation phase.
 */
export async function collectExactSingleFileEvidence(
  options: ExactSingleFileCatalogEvidenceOptions,
): Promise<ExactSingleFileEvidence> {
  options.signal.throwIfAborted()
  const sourcePath = Object.freeze(canonicalizePortableCatalogPath(
    options.artifact.sourcePath,
  ).split('/'))
  let directoryId = options.descriptor.syntheticRoot.slice()
  let directoryIdText = options.descriptor.syntheticRootId
  let ancestry: readonly string[] = Object.freeze([directoryIdText])
  let committed = options.root ?? await options.catalog.loadDirectory(directoryId, {
    signal: options.signal,
  })

  for (let depth = 0; depth < sourcePath.length; depth += 1) {
    committed = requireCommittedDirectory(committed, directoryId, directoryIdText)
    const segment = sourcePath[depth]!
    const entry = await findEntry(options, committed, segment)
    const final = depth === sourcePath.length - 1
    if (final) {
      if (entry.kind !== 'file' || entry.idText !== options.artifact.fileId ||
          !options.selection.selected(entry, ancestry)) {
        throw new CatalogTraversalError(
          'OriginalFile path does not resolve to its frozen selected file identity',
        )
      }
      return Object.freeze({
        fileId: entry.idText,
        containingDirectoryId: committed.directoryIdText,
        generation: encodeBase64Url(committed.generation),
        catalogSize: entry.expectedSize,
        sourcePath,
        ...(entry.modifiedTime === undefined
          ? {}
          : { modifiedTime: snapshotCanonicalModifiedTime(entry.modifiedTime) }),
      })
    }
    if (entry.kind !== 'directory') {
      throw new CatalogTraversalError('OriginalFile ancestor is not a directory')
    }
    directoryId = entry.id.slice()
    directoryIdText = entry.idText
    ancestry = Object.freeze([...ancestry, entry.idText])
    committed = await options.catalog.loadDirectory(directoryId, { signal: options.signal })
  }
  throw new CatalogTraversalError('OriginalFile source path is empty')
}

async function findEntry(
  options: ExactSingleFileCatalogEvidenceOptions,
  directory: CommittedDirectory,
  name: string,
): Promise<CatalogEntry> {
  let pageIndex = 0
  let entryCount = 0
  let matched: CatalogEntry | undefined
  for await (const page of options.catalog.pages(directory, options.signal)) {
    options.signal.throwIfAborted()
    if (page.pageIndex !== pageIndex ||
        !equalBytes(page.shareInstance, options.descriptor.shareInstance) ||
        !equalBytes(page.directoryId, directory.directoryId) ||
        !equalBytes(page.generation, directory.generation) ||
        page.terminal !== (pageIndex === directory.pageCount - 1)) {
      throw new CatalogTraversalError(
        'OriginalFile catalog replay does not match its committed generation',
      )
    }
    for (const entry of page.entries) {
      entryCount += 1
      if (entry.name !== name) continue
      if (matched !== undefined) {
        throw new CatalogTraversalError('OriginalFile catalog path is ambiguous')
      }
      matched = entry
    }
    pageIndex += 1
  }
  if (pageIndex !== directory.pageCount || entryCount !== directory.entryCount || matched === undefined) {
    throw new CatalogTraversalError('OriginalFile catalog path is missing or incomplete')
  }
  return matched
}

function requireCommittedDirectory(
  input: CommittedDirectory | undefined,
  expectedId: Uint8Array,
  expectedIdText: string,
): CommittedDirectory {
  if (input === undefined || input.directoryIdText !== expectedIdText ||
      !equalBytes(input.directoryId, expectedId) ||
      input.generation.byteLength !== CATALOG_IDENTITY_BYTES ||
      input.generation.every(byte => byte === 0) || input.omittedCount !== 0n ||
      !Number.isSafeInteger(input.pageCount) || input.pageCount < 1 ||
      !Number.isSafeInteger(input.entryCount) || input.entryCount < 0) {
    throw new CatalogTraversalError(
      'OriginalFile catalog authority is unavailable or incomplete',
    )
  }
  return input
}
