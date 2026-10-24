import type { CommittedDirectory } from '../../catalog/page-store'
import { CATALOG_IDENTITY_BYTES } from '../../catalog/records'
import type { FrozenSelectionPolicy } from '../../catalog/selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import type { AuthenticatedDirectory, DirectoryCursor, DirectoryWork, TransferJobOptions } from './contract'
import { CatalogTraversalError } from './contract'
import {
  snapshotLogicalArtifactPath,
  snapshotSourceAuthenticationPath,
} from './coordinate/direct-tree'
import { descriptorRootId } from './identity'

export class JobRootAuthority {
  readonly #options: TransferJobOptions
  readonly #selection: FrozenSelectionPolicy
  readonly #signal: AbortSignal
  #committed: CommittedDirectory | undefined

  constructor(
    options: TransferJobOptions,
    selection: FrozenSelectionPolicy,
    signal: AbortSignal,
  ) {
    this.#options = options
    this.#selection = selection
    this.#signal = signal
  }

  get committed(): CommittedDirectory | undefined {
    return this.#committed
  }

  async load(): Promise<CommittedDirectory> {
    if (this.#committed !== undefined) return this.#committed
    this.#committed = await loadSyntheticRoot(this.#options, this.#signal)
    return this.#committed
  }

  cursor(): DirectoryCursor {
    return syntheticRootCursor(this.#options, this.#selection)
  }

  authenticated(committed: CommittedDirectory): DirectoryWork {
    return authenticatedRootWork(this.cursor(), committed)
  }

  async direct(
    admit: (
      cursor: DirectoryCursor,
      committed: CommittedDirectory,
    ) => Promise<AuthenticatedDirectory>,
  ): Promise<DirectoryWork> {
    const committed = await this.load()
    const cursor = this.cursor()
    const root = await admit(cursor, committed)
    return Object.freeze({ cursor, materializeParent: async () => root })
  }
}

export async function loadSyntheticRoot(
  options: TransferJobOptions,
  signal: AbortSignal,
): Promise<CommittedDirectory> {
  const root = await options.catalog.loadDirectory(
    options.descriptor.syntheticRoot,
    { signal },
  )
  if (root === undefined || !equalBytes(root.directoryId, options.descriptor.syntheticRoot) ||
      root.generation.byteLength !== CATALOG_IDENTITY_BYTES ||
      root.generation.every(byte => byte === 0) || root.omittedCount !== 0n) {
    throw new CatalogTraversalError('Synthetic root committed generation is unavailable')
  }
  return root
}

export function syntheticRootCursor(
  options: TransferJobOptions,
  selection: FrozenSelectionPolicy,
): DirectoryCursor {
  const rootId = descriptorRootId(options.descriptor)
  return Object.freeze({
    id: options.descriptor.syntheticRoot.slice(),
    idText: rootId,
    path: Object.freeze([]),
    ancestry: Object.freeze([rootId]),
    selected: selection.directorySelected(rootId, []),
  })
}

export function authenticatedRootWork(
  cursor: DirectoryCursor,
  committed: CommittedDirectory,
): DirectoryWork {
  const root: AuthenticatedDirectory = Object.freeze({
    kind: 'reference',
    directoryId: cursor.idText,
    generation: encodeBase64Url(committed.generation),
    sourceAuthenticationPath: snapshotSourceAuthenticationPath([]),
    logicalArtifactPath: snapshotLogicalArtifactPath([]),
  })
  return Object.freeze({ cursor, materializeParent: async () => root })
}
