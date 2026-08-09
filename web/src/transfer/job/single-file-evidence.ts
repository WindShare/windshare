import { canonicalizePortableCatalogPath } from '../../catalog/path-policy'
import type { V2CatalogClient } from '../../catalog/v2-client'
import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import {
  V2_CATALOG_IDENTITY_BYTES,
  type V2CatalogEntry,
  type V2ShareDescriptor,
} from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import {
  snapshotCanonicalModifiedTime,
} from '../directory-admission'
import type { OriginalFileArtifact } from '../intent'
import type { ExactSingleFileEvidence } from '../output-session'
import { V2CatalogTraversalError } from './contract'

export interface ExactSingleFileCatalogEvidenceOptions {
  readonly catalog: Pick<V2CatalogClient, 'loadDirectory' | 'pages'>
  readonly descriptor: Pick<
    V2ShareDescriptor,
    'shareInstance' | 'syntheticRoot' | 'syntheticRootId'
  >
  readonly selection: V2FrozenSelectionPolicy
  readonly artifact: OriginalFileArtifact
  readonly signal: AbortSignal
  readonly root?: V2CommittedDirectory
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
        throw new V2CatalogTraversalError(
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
      throw new V2CatalogTraversalError('OriginalFile ancestor is not a directory')
    }
    directoryId = entry.id.slice()
    directoryIdText = entry.idText
    ancestry = Object.freeze([...ancestry, entry.idText])
    committed = await options.catalog.loadDirectory(directoryId, { signal: options.signal })
  }
  throw new V2CatalogTraversalError('OriginalFile source path is empty')
}

async function findEntry(
  options: ExactSingleFileCatalogEvidenceOptions,
  directory: V2CommittedDirectory,
  name: string,
): Promise<V2CatalogEntry> {
  let pageIndex = 0
  let entryCount = 0
  let matched: V2CatalogEntry | undefined
  for await (const page of options.catalog.pages(directory, options.signal)) {
    options.signal.throwIfAborted()
    if (page.pageIndex !== pageIndex ||
        !equalBytes(page.shareInstance, options.descriptor.shareInstance) ||
        !equalBytes(page.directoryId, directory.directoryId) ||
        !equalBytes(page.generation, directory.generation) ||
        page.terminal !== (pageIndex === directory.pageCount - 1)) {
      throw new V2CatalogTraversalError(
        'OriginalFile catalog replay does not match its committed generation',
      )
    }
    for (const entry of page.entries) {
      entryCount += 1
      if (entry.name !== name) continue
      if (matched !== undefined) {
        throw new V2CatalogTraversalError('OriginalFile catalog path is ambiguous')
      }
      matched = entry
    }
    pageIndex += 1
  }
  if (pageIndex !== directory.pageCount || entryCount !== directory.entryCount || matched === undefined) {
    throw new V2CatalogTraversalError('OriginalFile catalog path is missing or incomplete')
  }
  return matched
}

function requireCommittedDirectory(
  input: V2CommittedDirectory | undefined,
  expectedId: Uint8Array,
  expectedIdText: string,
): V2CommittedDirectory {
  if (input === undefined || input.directoryIdText !== expectedIdText ||
      !equalBytes(input.directoryId, expectedId) ||
      input.generation.byteLength !== V2_CATALOG_IDENTITY_BYTES ||
      input.generation.every(byte => byte === 0) || input.omittedCount !== 0n ||
      !Number.isSafeInteger(input.pageCount) || input.pageCount < 1 ||
      !Number.isSafeInteger(input.entryCount) || input.entryCount < 0) {
    throw new V2CatalogTraversalError(
      'OriginalFile catalog authority is unavailable or incomplete',
    )
  }
  return input
}
