import { snapshotMaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import type { PersistentDirectoryNamespaceClaim } from '../../persistent-tree/contracts'
import type { PathComponentRejectedError } from '../../browser/filesystem-component-inspection'
import type {
  FSACompatibleNamePairHandleRepository,
  PersistedFSAOperationBinding,
} from '../../browser/indexeddb-root-binding'
import { readFSAOwnedDirectoryBinding } from '../../browser/indexeddb-root-binding'
import {
  fsaDirectoryHandleId,
  verifySameDirectory,
} from '../../browser/filesystem-directory-authority'
import type { FSARootMutationAuthority } from '../../browser/namespace-mutation'
import {
  fsaAuthorityCacheForRoot,
  type FSAAuthorityCache,
  type FSAVerifiedDirectoryAuthority,
} from '../../browser/mutation-coordination/authority-cache'
import type { CompatibleNameLedger } from './ledger'
import type {
  CompatibleNameEntryKind,
  CompatibleNameOperationBootstrapV1,
  CompatibleNamePairPlacement,
  CompatibleNamePendingTerminalOutcomeV1,
  CompatibleNameRepairSummary,
  CompatibleNameTerminalFooterState,
} from './model'
import type { CompatibleNameRandomBitsSource } from './naming'
import {
  LogicalSiblingNamespaceAuthority,
  type CompatiblePhysicalChild,
} from './resolver'
import type {
  RestorationTemplate,
  RestorationTemplateProvider,
} from './restoration-template'
import { CompatibleNameCoordinator } from './coordinator-runtime'
import { selectMatchingTemplate } from './root-repair'

export type CompatibleNameActivationLedger = Pick<
  CompatibleNameLedger,
  | 'readHeader'
  | 'loadOperation'
  | 'bootstrapOperation'
  | 'claimMapping'
  | 'recordPairOwnership'
  | 'recordCompatibleTargetCreated'
  | 'recordVerifiedDirectoryOwnership'
  | 'commitMapping'
  | 'scanCommittedMappings'
  | 'persistRepairSummary'
  | 'persistPendingTerminalOutcome'
  | 'readPendingTerminalOutcome'
  | 'clearPendingTerminalOutcome'
  | 'readRepairSummary'
  | 'removeVerifiedEmptyOperation'
  | 'close'
>

export interface CompatibleNameRepairProjectionSource {
  subscribe(listener: (summary: CompatibleNameRepairSummary) => void): () => void
}

export type CompatibleNameEmptyRepairRemoval =
  | Readonly<{ kind: 'absent' }>
  | Readonly<{ kind: 'not-empty'; committedCount: number }>
  | Readonly<{
      kind: 'removed'
      removedObjectIds: readonly string[]
      removedHandleIds: readonly string[]
    }>

export interface CompatibleNameRootRepairRequest {
  readonly rejection: PathComponentRejectedError
  readonly parent: FileSystemDirectoryHandle
  readonly operationId: string
  readonly authorityRef: string
  readonly logicalReservedName: string
  readonly entryKind: CompatibleNameEntryKind
}

export interface PreparedCompatibleNameRootRepair {
  readonly bootstrap: CompatibleNameOperationBootstrapV1
  readonly template: RestorationTemplate
  readonly parent: FileSystemDirectoryHandle
  readonly rejection: PathComponentRejectedError
}

export type CompatibleNameRootRepairFactory = (
  input: CompatibleNameRootRepairRequest,
) => Promise<PreparedCompatibleNameRootRepair>

export interface CompatibleNameRootRepairPreparationOptions {
  readonly platform: string
  readonly templateProvider?: RestorationTemplateProvider
  readonly randomBits?: CompatibleNameRandomBitsSource
  readonly randomOwnedObjectId?: () => string
}

export interface ActivatePreparedCompatibleNameRootRepairOptions {
  readonly prepared: PreparedCompatibleNameRootRepair
  readonly mutations: FSARootMutationAuthority
  readonly pairHandles: FSACompatibleNamePairHandleRepository
  readonly openLedger: () => Promise<CompatibleNameActivationLedger>
}

export interface CompatibleNameRejectedComponentInput {
  readonly rejection: PathComponentRejectedError
  readonly artifactPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly parent: FileSystemDirectoryHandle
  readonly parentAuthority: FSAVerifiedDirectoryAuthority
  readonly pairParent: FileSystemDirectoryHandle
  readonly pairParentAuthority: FSAVerifiedDirectoryAuthority
}

interface CompatibleNamePathAuthorityOptions {
  readonly binding: PersistedFSAOperationBinding
  readonly mutations: FSARootMutationAuthority
  readonly pairHandles: FSACompatibleNamePairHandleRepository
  readonly openLedger: () => Promise<CompatibleNameActivationLedger>
  readonly preparation: CompatibleNameRootRepairPreparationOptions
  readonly coordinator?: CompatibleNameCoordinator
}

/**
 * This session owner is deliberately not a resolver or coordinator. Ordinary operations retain only
 * their lazy factories and authenticated namespace bindings until an exact native refusal activates
 * repair, while reopen performs one header point-read before deciding whether either object exists.
 */
export class CompatibleNamePathAuthority {
  readonly namespaceClaims = new LogicalSiblingNamespaceAuthority()
  readonly #binding: PersistedFSAOperationBinding
  readonly #mutations: FSARootMutationAuthority
  readonly #authorities: FSAAuthorityCache
  readonly #pairHandles: FSACompatibleNamePairHandleRepository
  readonly #openLedger: () => Promise<CompatibleNameActivationLedger>
  readonly #preparation: CompatibleNameRootRepairPreparationOptions
  readonly #activationListeners = new Set<(source: CompatibleNameRepairProjectionSource) => void>()
  #coordinator: CompatibleNameCoordinator | undefined
  #closed = false

  private constructor(options: CompatibleNamePathAuthorityOptions) {
    this.#binding = options.binding
    this.#mutations = options.mutations
    this.#authorities = fsaAuthorityCacheForRoot({
      owner: options.mutations,
      binding: options.binding,
      rootParentIdentity: options.mutations.rootParentIdentity,
    })
    this.#pairHandles = options.pairHandles
    this.#openLedger = options.openLedger
    this.#preparation = options.preparation
    this.#coordinator = options.coordinator
  }

  static create(options: CompatibleNamePathAuthorityOptions): CompatibleNamePathAuthority {
    return new CompatibleNamePathAuthority(options)
  }

  static async openForReopen(
    options: Omit<CompatibleNamePathAuthorityOptions, 'coordinator'>,
  ): Promise<CompatibleNamePathAuthority> {
    const ledger = await options.openLedger()
    try {
      const header = await ledger.readHeader(options.binding.intent.operationId)
      if (header === undefined) {
        ledger.close()
        return new CompatibleNamePathAuthority(options)
      }
      const snapshot = await ledger.loadOperation(options.binding.intent.operationId)
      if (snapshot === undefined) {
        throw new DOMException('Compatible-name header lost its mapping snapshot', 'InvalidStateError')
      }
      const template = selectMatchingTemplate(options.preparation, snapshot.header.templateId)
      const coordinator = CompatibleNameCoordinator.reopen({
        ledger,
        snapshot,
        template,
        pairHandles: options.pairHandles,
      })
      return new CompatibleNamePathAuthority({ ...options, coordinator })
    } catch (error) {
      ledger.close()
      throw error
    }
  }

  get active(): boolean {
    return this.#coordinator !== undefined
  }

  get rootEntryKind(): CompatibleNameEntryKind | undefined {
    return this.#coordinator?.rootEntryKind
  }

  get pairPlacement(): CompatibleNamePairPlacement | undefined {
    return this.#coordinator?.pairPlacement
  }

  get repairProjection(): CompatibleNameRepairProjectionSource | undefined {
    return this.#coordinator?.repairProjection
  }

  subscribeRepairProjectionActivation(
    listener: (source: CompatibleNameRepairProjectionSource) => void,
  ): () => void {
    this.#assertOpen()
    this.#activationListeners.add(listener)
    const projection = this.#coordinator?.repairProjection
    if (projection !== undefined) listener(projection)
    return () => { this.#activationListeners.delete(listener) }
  }

  repairSummary(): CompatibleNameRepairSummary | undefined {
    return this.#coordinator?.repairSummary
  }

  pendingTerminalOutcome(): CompatibleNamePendingTerminalOutcomeV1 | undefined {
    return this.#coordinator?.pendingTerminalOutcome
  }

  bindDirectoryNamespace(claim: PersistentDirectoryNamespaceClaim): void {
    this.#assertOpen()
    this.namespaceClaims.bindDirectoryNamespace(claim)
  }

  physicalComponent(
    artifactPathInput: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): string {
    const artifactPath = snapshotMaterializationRootRelativePath(artifactPathInput)
    return this.#coordinator?.physicalComponent(artifactPath, entryKind) ?? artifactPath.at(-1)!
  }

  hasMapping(artifactPath: readonly string[], entryKind: CompatibleNameEntryKind): boolean {
    return this.#coordinator?.mapping(artifactPath, entryKind) !== undefined
  }

  hasLateLogicalCollision(
    artifactPathInput: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): boolean {
    const artifactPath = snapshotMaterializationRootRelativePath(artifactPathInput)
    const coordinator = this.#coordinator
    return coordinator !== undefined && coordinator.mapping(artifactPath, entryKind) === undefined &&
      coordinator.hasClaimedLogicalSibling(artifactPath.slice(0, -1), artifactPath.at(-1)!)
  }

  physicalChild(
    parentArtifactPath: readonly string[],
    physicalComponent: string,
    entryKind: CompatibleNameEntryKind,
  ): CompatiblePhysicalChild {
    return this.#coordinator?.physicalChild(parentArtifactPath, physicalComponent, entryKind) ??
      Object.freeze({ kind: 'logical', logicalComponent: physicalComponent })
  }

  pairHandleIds(): readonly string[] {
    const header = this.#coordinator?.resolver.header
    return header === undefined
      ? Object.freeze([])
      : Object.freeze([header.pair.script.handleId, header.pair.sidecar.handleId])
  }

  async resolveRejectedComponent(input: CompatibleNameRejectedComponentInput): Promise<string> {
    this.#assertOpen()
    const parents = input.parentAuthority.schedulerIdentity ===
      input.pairParentAuthority.schedulerIdentity
      ? [input.parentAuthority.schedulerIdentity]
      : [
          input.parentAuthority.schedulerIdentity,
          input.pairParentAuthority.schedulerIdentity,
        ]
    const physicalName = await this.#mutations.scheduler.runNamespace(
      parents,
      'repair-compatible-name',
      async () => {
        let coordinator = this.#coordinator
        if (coordinator === undefined) {
          coordinator = await CompatibleNameCoordinator.activateDescendant({
            binding: this.#binding,
            rejected: input,
            namespaceClaims: this.namespaceClaims,
            pairHandles: this.#pairHandles,
            openLedger: this.#openLedger,
            preparation: this.#preparation,
          })
          this.#coordinator = coordinator
          this.#publishActivation(coordinator.repairProjection)
          return coordinator.physicalComponent(input.artifactPath, input.entryKind)
        }
        return coordinator.claimRejectedComponent(input, this.namespaceClaims)
      },
    )
    this.#authorities.invalidateSubtree(input.artifactPath)
    return physicalName
  }

  async ensurePairReady(root: FileSystemDirectoryHandle | undefined): Promise<void> {
    this.#assertOpen()
    const coordinator = this.#coordinator
    if (coordinator === undefined) return
    const pairParent = coordinator.pairPlacement === 'inside-logical-root'
      ? root
      : this.#binding.parent
    if (pairParent === undefined) {
      throw new DOMException('Compatible-name pair parent is unavailable', 'InvalidStateError')
    }
    let pairParentAuthority = coordinator.pairPlacement === 'inside-logical-root'
      ? this.#authorities.directory([])
      : this.#authorities.pickedParent()
    if (pairParentAuthority === undefined && root !== undefined) {
      const handleId = await fsaDirectoryHandleId(this.#binding.reservation, [])
      const persisted = await readFSAOwnedDirectoryBinding({
        repository: this.#pairHandles,
        reservation: this.#binding.reservation,
        handleId,
        diagnosticTarget: 'root',
      })
      if (persisted === undefined || !await verifySameDirectory(
        root,
        persisted.handle,
        this.#binding.intent.operationId,
        undefined,
        'fsa.root.handle.verify',
      )) {
        this.#authorities.invalidateOperation()
        throw new DOMException('Compatible-name pair authority is unavailable', 'InvalidStateError')
      }
      pairParentAuthority = this.#authorities.installDirectory({
        ...persisted,
        canonicalPath: [],
        physicalName: this.#binding.reservation.physicalName,
        handle: root,
      })
    }
    if (pairParentAuthority === undefined) {
      throw new DOMException('Compatible-name pair authority is unavailable', 'InvalidStateError')
    }
    await this.#mutations.scheduler.runNamespace(
      [pairParentAuthority.schedulerIdentity],
      'repair-compatible-name',
      () => coordinator.ensurePairReady(pairParent),
    )
  }

  async recordCompatibleTargetCreated(
    artifactPath: readonly string[],
    entryKind: CompatibleNameEntryKind,
    root: FileSystemDirectoryHandle | undefined,
  ): Promise<void> {
    this.#assertOpen()
    const coordinator = this.#coordinator
    if (coordinator === undefined || !coordinator.hasMappingForCreatedTarget(artifactPath, entryKind)) return
    const pairParent = coordinator.pairPlacement === 'inside-logical-root'
      ? root
      : this.#binding.parent
    if (pairParent === undefined) {
      throw new DOMException('Compatible-name pair parent is unavailable', 'InvalidStateError')
    }
    await coordinator.recordCompatibleTargetCreated(artifactPath, entryKind, pairParent)
  }

  persistPendingTerminalOutcome(
    outcome: CompatibleNamePendingTerminalOutcomeV1,
  ): Promise<void> {
    return this.#coordinator?.persistPendingTerminalOutcome(outcome) ?? Promise.resolve()
  }

  drainTerminalProjector(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameRepairSummary | undefined> {
    return this.#coordinator?.drainTerminalProjector(state) ?? Promise.resolve(undefined)
  }

  clearPendingTerminalOutcome(): Promise<void> {
    return this.#coordinator?.clearPendingTerminalOutcome() ?? Promise.resolve()
  }

  commitVerifiedRootDirectory(ownedObjectId: string): Promise<void> {
    const coordinator = this.#coordinator
    return coordinator === undefined
      ? Promise.resolve()
      : coordinator.commitVerifiedRootDirectory(ownedObjectId)
  }

  commitVerifiedDirectory(path: readonly string[], ownedObjectId: string): Promise<void> {
    const coordinator = this.#coordinator
    return coordinator === undefined
      ? Promise.resolve()
      : coordinator.commitVerifiedDirectory(path, ownedObjectId)
  }

  commitFinalFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const coordinator = this.#coordinator
    return coordinator === undefined
      ? Promise.resolve()
      : coordinator.commitFinalFile(path, ownedObjectId)
  }

  async removeVerifiedEmptyRepair(
    root: FileSystemDirectoryHandle | undefined,
  ): Promise<CompatibleNameEmptyRepairRemoval> {
    return this.#mutations.run(
      'remove-entry',
      () => this.removeVerifiedEmptyRepairWithinMutation(root),
    )
  }

  async removeVerifiedEmptyRepairWithinMutation(
    root: FileSystemDirectoryHandle | undefined,
  ): Promise<CompatibleNameEmptyRepairRemoval> {
    const coordinator = this.#coordinator
    if (coordinator === undefined) return Object.freeze({ kind: 'absent' })
    const pairParent = coordinator.pairPlacement === 'inside-logical-root'
      ? root
      : this.#binding.parent
    if (pairParent === undefined) {
      throw new DOMException('Compatible-name pair parent is unavailable', 'InvalidStateError')
    }
    const result = await coordinator.removeVerifiedEmptyRepair(pairParent)
    if (result.kind === 'removed') {
      coordinator.close()
      this.#coordinator = undefined
    }
    return result
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#activationListeners.clear()
    this.#coordinator?.close()
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Compatible-name path authority is closed', 'InvalidStateError')
    }
  }

  #publishActivation(source: CompatibleNameRepairProjectionSource): void {
    for (const listener of this.#activationListeners) {
      try {
        listener(source)
      } catch {
        // Runtime presentation cannot interfere with repair activation ownership.
      }
    }
  }
}
