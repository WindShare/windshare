import { catalogNameCollisionKey, snapshotPortableCatalogPath } from '../../../catalog/path-policy'
import type {
  PersistentDirectoryNamespaceClaim,
  PersistentOutputNamespaceClaimPort,
} from '../../persistent-tree/contracts'
import {
  compatibleNameMappingV1,
  compatibleNameOperationHeaderV1,
  compatibleMappingPhysicalParent,
  compatiblePairPhysicalParent,
  type CompatibleNameEntryKind,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationHeaderV1,
  type CompatibleNameOperationSnapshotV1,
} from './model'

function pathKey(path: readonly string[]): string {
  return path.map(component => `${component.length}:${component}`).join('/')
}

function mappingKey(path: readonly string[], entryKind: CompatibleNameEntryKind): string {
  return `${entryKind}\0${pathKey(path)}`
}

/**
 * Catalog membership is retained independently from repair activation because discovery can bind
 * directories before the first native refusal. Binding never performs I/O; only candidate allocation
 * asks whether a generated physical component is already an authenticated logical child.
 */
export class LogicalSiblingNamespaceAuthority implements PersistentOutputNamespaceClaimPort {
  readonly #claims = new Map<string, PersistentDirectoryNamespaceClaim['logicalSiblingMembership']>()

  bindDirectoryNamespace(claim: PersistentDirectoryNamespaceClaim): void {
    const artifactPath = claim.artifactPath.length === 0
      ? Object.freeze([])
      : snapshotPortableCatalogPath(claim.artifactPath)
    const key = pathKey(artifactPath)
    const existing = this.#claims.get(key)
    if (existing !== undefined &&
        (existing.directoryId !== claim.logicalSiblingMembership.directoryId ||
          existing.generation !== claim.logicalSiblingMembership.generation)) {
      throw new TypeError('logical sibling namespace authority changed for an admitted directory')
    }
    this.#claims.set(key, claim.logicalSiblingMembership)
  }

  membership(pathInput: readonly string[]) {
    const path = pathInput.length === 0 ? Object.freeze([]) : snapshotPortableCatalogPath(pathInput)
    return this.#claims.get(pathKey(path))
  }
}

export type CompatiblePhysicalChild =
  | Readonly<{ kind: 'logical'; logicalComponent: string }>
  | Readonly<{ kind: 'restoration-pair' }>
  | Readonly<{ kind: 'unknown' }>

/**
 * Physical names stay behind this session-local boundary so checkpoints, catalog identities, and
 * progress can remain entirely logical. Durable mutations are adopted only after their ledger call
 * succeeds, making the in-memory view a faithful cache rather than a second source of truth.
 */
export class PhysicalPathResolver {
  #header: CompatibleNameOperationHeaderV1
  readonly #mappings = new Map<string, CompatibleNameMappingV1>()
  readonly #physicalByParent = new Map<string, CompatibleNameMappingV1>()

  constructor(snapshot: CompatibleNameOperationSnapshotV1) {
    this.#header = compatibleNameOperationHeaderV1(snapshot.header)
    for (const mapping of snapshot.mappings) this.adoptMapping(mapping)
  }

  get header(): CompatibleNameOperationHeaderV1 {
    return this.#header
  }

  get operationId(): string {
    return this.#header.operationId
  }

  get physicalRootName(): string {
    return this.#header.root.physicalName
  }

  mapping(
    logicalPathInput: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): CompatibleNameMappingV1 | undefined {
    const logicalPath = snapshotPortableCatalogPath(logicalPathInput)
    return this.#mappings.get(mappingKey(logicalPath, entryKind))
  }

  physicalComponent(
    logicalPathInput: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): string {
    const logicalPath = snapshotPortableCatalogPath(logicalPathInput)
    return this.#mappings.get(mappingKey(logicalPath, entryKind))?.physicalComponent ??
      logicalPath.at(-1)!
  }

  hasClaimedPhysicalComponent(parentPathInput: readonly string[], component: string): boolean {
    const parentPath = parentPathInput.length === 0
      ? Object.freeze([])
      : snapshotPortableCatalogPath(parentPathInput)
    const physicalParent = this.#physicalParent(parentPath)
    if (this.#physicalByParent.has(physicalParentKey(physicalParent, component))) return true
    return this.#isPairClaim(physicalParent, component)
  }

  hasClaimedLogicalSibling(parentPathInput: readonly string[], component: string): boolean {
    const parentPath = parentPathInput.length === 0
      ? Object.freeze([])
      : snapshotPortableCatalogPath(parentPathInput)
    return this.hasClaimedPhysicalComponent(parentPath, component)
  }

  physicalChild(
    parentLogicalPathInput: readonly string[],
    physicalComponent: string,
    entryKind: CompatibleNameEntryKind,
  ): CompatiblePhysicalChild {
    const parentLogicalPath = parentLogicalPathInput.length === 0
      ? Object.freeze([])
      : snapshotPortableCatalogPath(parentLogicalPathInput)
    const physicalParent = this.#physicalParent(parentLogicalPath)
    if (this.#isPairClaim(physicalParent, physicalComponent)) {
      return Object.freeze({ kind: 'restoration-pair' })
    }
    const mapping = this.#physicalByParent.get(
      physicalParentKey(physicalParent, physicalComponent),
    )
    if (mapping !== undefined) {
      return mapping.entryKind === entryKind
        ? Object.freeze({ kind: 'logical', logicalComponent: mapping.logicalPath.at(-1)! })
        : Object.freeze({ kind: 'unknown' })
    }
    return Object.freeze({ kind: 'logical', logicalComponent: physicalComponent })
  }

  adoptHeader(headerInput: CompatibleNameOperationHeaderV1): void {
    const header = compatibleNameOperationHeaderV1(headerInput)
    if (header.operationId !== this.operationId ||
        header.primaryToken !== this.#header.primaryToken ||
        header.authorityRef !== this.#header.authorityRef ||
        header.templateId !== this.#header.templateId ||
        header.pairPlacement !== this.#header.pairPlacement ||
        header.root.logicalName !== this.#header.root.logicalName ||
        header.root.physicalName !== this.#header.root.physicalName ||
        header.pair.script.physicalName !== this.#header.pair.script.physicalName ||
        header.pair.sidecar.physicalName !== this.#header.pair.sidecar.physicalName) {
      throw new TypeError('compatible-name resolver header identity changed')
    }
    const activationRank = { prepared: 0, 'pair-ready': 1, active: 2 } as const
    if (activationRank[header.activationState] < activationRank[this.#header.activationState]) {
      throw new TypeError('compatible-name resolver activation state regressed')
    }
    this.#header = header
  }

  adoptMapping(mappingInput: CompatibleNameMappingV1): void {
    const mapping = compatibleNameMappingV1(mappingInput)
    if (mapping.operationId !== this.operationId) {
      throw new TypeError('compatible-name resolver mapping escaped its operation')
    }
    const key = mappingKey(mapping.logicalPath, mapping.entryKind)
    const existing = this.#mappings.get(key)
    if (existing !== undefined &&
        (existing.id !== mapping.id || existing.physicalComponent !== mapping.physicalComponent ||
          existing.attempt !== mapping.attempt || existing.token !== mapping.token ||
          (existing.ownershipState === 'owned' && mapping.ownershipState !== 'owned') ||
          (existing.ownedObjectId !== undefined && existing.ownedObjectId !== mapping.ownedObjectId) ||
          (existing.commitState === 'committed' &&
            (mapping.commitState !== 'committed' ||
              existing.commitOrdinal !== mapping.commitOrdinal)))) {
      throw new TypeError('compatible-name resolver mapping changed its immutable selection')
    }
    const physicalKey = physicalParentKey(
      compatibleMappingPhysicalParent(this.#header, mapping.logicalPath),
      mapping.physicalComponent,
    )
    const physicalOwner = this.#physicalByParent.get(physicalKey)
    if (physicalOwner !== undefined && physicalOwner.id !== mapping.id) {
      throw new TypeError('compatible-name resolver contains conflicting sibling claims')
    }
    this.#mappings.set(key, mapping)
    this.#physicalByParent.set(physicalKey, mapping)
  }

  #physicalParent(parentPath: readonly string[]): readonly string[] {
    return this.#header.pairPlacement === 'inside-logical-root'
      ? Object.freeze([this.#header.root.logicalName, ...parentPath])
      : parentPath
  }

  #isPairClaim(physicalParent: readonly string[], component: string): boolean {
    if (pathKey(physicalParent) !== pathKey(compatiblePairPhysicalParent(this.#header))) return false
    const key = catalogNameCollisionKey(component)
    return key === catalogNameCollisionKey(this.#header.pair.script.physicalName) ||
      key === catalogNameCollisionKey(this.#header.pair.sidecar.physicalName)
  }
}

function physicalParentKey(parentPath: readonly string[], component: string): string {
  return `${pathKey(parentPath)}\0${catalogNameCollisionKey(component)}`
}
