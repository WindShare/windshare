import type { V2CommittedDirectory } from '../../catalog/v2-page-store'
import { V2_CATALOG_IDENTITY_BYTES } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url, equalBytes } from '../../crypto/bytes'
import type { AuthenticatedDirectory, DirectoryCursor, DirectoryWork, TransferJobOptions } from './contract'
import { V2CatalogTraversalError } from './contract'
import { descriptorRootId } from './identity'

export class V2JobRootAuthority {
  readonly #options: TransferJobOptions
  readonly #selection: V2FrozenSelectionPolicy
  readonly #signal: AbortSignal
  #committed: V2CommittedDirectory | undefined

  constructor(
    options: TransferJobOptions,
    selection: V2FrozenSelectionPolicy,
    signal: AbortSignal,
  ) {
    this.#options = options
    this.#selection = selection
    this.#signal = signal
  }

  get committed(): V2CommittedDirectory | undefined {
    return this.#committed
  }

  async load(): Promise<V2CommittedDirectory> {
    if (this.#committed !== undefined) return this.#committed
    this.#committed = await loadSyntheticRoot(this.#options, this.#signal)
    return this.#committed
  }

  cursor(): DirectoryCursor {
    return syntheticRootCursor(this.#options, this.#selection)
  }

  authenticated(committed: V2CommittedDirectory): DirectoryWork {
    return authenticatedRootWork(this.cursor(), committed)
  }

  async direct(
    admit: (
      cursor: DirectoryCursor,
      committed: V2CommittedDirectory,
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
): Promise<V2CommittedDirectory> {
  const root = await options.catalog.loadDirectory(
    options.descriptor.syntheticRoot,
    { signal },
  )
  if (root === undefined || !equalBytes(root.directoryId, options.descriptor.syntheticRoot) ||
      root.generation.byteLength !== V2_CATALOG_IDENTITY_BYTES ||
      root.generation.every(byte => byte === 0) || root.omittedCount !== 0n) {
    throw new V2CatalogTraversalError('Synthetic root committed generation is unavailable')
  }
  return root
}

export function syntheticRootCursor(
  options: TransferJobOptions,
  selection: V2FrozenSelectionPolicy,
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
  committed: V2CommittedDirectory,
): DirectoryWork {
  const root: AuthenticatedDirectory = Object.freeze({
    directoryId: cursor.idText,
    generation: encodeBase64Url(committed.generation),
    sourcePath: Object.freeze([]),
    artifactPath: Object.freeze([]),
  })
  return Object.freeze({ cursor, materializeParent: async () => root })
}
