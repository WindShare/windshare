import { snapshotMaterializationRootRelativePath } from '../../../transfer/job/coordinate/direct-tree'
import { inspectFileSystemComponent } from '../../browser/filesystem-component-inspection'
import { createOwnedObjectId, requireOwnedObjectId } from '../../browser/filesystem-file-record'
import {
  readFSACompatibleNamePairHandle,
  type FSACompatibleNamePairHandleRepository,
  type PersistedFSAOperationBinding,
} from '../../browser/indexeddb-root-binding'
import {
  MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS,
  compatibleNameRepairSummary,
  compatibleNameOperationBootstrapV1,
  compatibleNameMappingV1,
  compatibleNameOperationHeaderV1,
  type CompatibleNameEntryKind,
  type CompatibleNameMappingV1,
  type CompatibleNameOperationSnapshotV1,
  type CompatibleNamePairKind,
  type CompatibleNamePairPlacement,
  type CompatibleNamePendingTerminalOutcomeV1,
  type CompatibleNameRepairSummary,
  type CompatibleNameTerminalFooterState,
} from './model'
import { generateCompatibleNamePrimaryToken } from './naming'
import {
  LogicalSiblingNamespaceAuthority,
  PhysicalPathResolver,
  type CompatiblePhysicalChild,
} from './resolver'
import {
  provenRestorationTemplateProvider,
  type RestorationTemplate,
} from './restoration-template'
import {
  createCompatibleNameProjector,
  type CompatibleNameProjector,
} from './projector'
import {
  compatibleNameSidecarPlacement,
  decodeCompatibleNameSidecar,
  encodeCompatibleNameSidecarFooter,
  encodeCompatibleNameSidecarHeader,
} from './sidecar-codec'
import type {
  ActivatePreparedCompatibleNameRootRepairOptions,
  CompatibleNameActivationLedger,
  CompatibleNameEmptyRepairRemoval,
  CompatibleNameRejectedComponentInput,
  CompatibleNameRepairProjectionSource,
  CompatibleNameRootRepairPreparationOptions,
} from './path-authority'
import {
  RESTORATION_PAIR_READABLE_PREFIX,
  RESTORATION_SIDECAR_EXTENSION,
  allocateCandidate,
  assertDescendantRejection,
  assertPreparedSnapshot,
  pairIdentity,
  pairPhysicalName,
} from './root-repair'
import {
  DurableCompatibleNameRepairProjection,
  FSAOwnedSidecarWriter,
  equalBytes,
  readFileBytes,
  replaceAndCloseFile,
  sameLogicalPath,
  sameRepairSummary,
} from './sidecar-runtime'

const TEXT_ENCODER = new TextEncoder()

export class CompatibleNameCoordinator {
  readonly resolver: PhysicalPathResolver
  readonly repairProjection: CompatibleNameRepairProjectionSource
  readonly #ledger: CompatibleNameActivationLedger
  readonly #template: RestorationTemplate
  readonly #pairHandles: FSACompatibleNamePairHandleRepository
  readonly #rootMapping: CompatibleNameMappingV1 | undefined
  readonly #projection = new DurableCompatibleNameRepairProjection()
  readonly #committedMappings = new Map<number, CompatibleNameMappingV1>()
  #projector: CompatibleNameProjector | undefined
  #repairSummary: CompatibleNameRepairSummary | undefined
  #pairReadyVerified = false
  #closed = false

  private constructor(input: Readonly<{
    ledger: CompatibleNameActivationLedger
    snapshot: CompatibleNameOperationSnapshotV1
    template: RestorationTemplate
    pairHandles: FSACompatibleNamePairHandleRepository
  }>) {
    this.#ledger = input.ledger
    this.#template = input.template
    this.#pairHandles = input.pairHandles
    this.repairProjection = this.#projection
    this.resolver = new PhysicalPathResolver(input.snapshot)
    this.#repairSummary = input.snapshot.header.repairSummary
    for (const mapping of input.snapshot.mappings) {
      if (mapping.commitOrdinal !== undefined) this.#committedMappings.set(mapping.commitOrdinal, mapping)
    }
    this.#rootMapping = input.snapshot.mappings.find(mapping =>
      mapping.logicalPath.length === 1 &&
      mapping.logicalPath[0] === input.snapshot.header.root.logicalName &&
      mapping.physicalComponent === input.snapshot.header.root.physicalName)
    if (input.snapshot.header.activationState === 'active' && this.#repairSummary !== undefined) {
      this.#projection.publish(this.#repairSummary)
    }
  }

  static async activate(
    options: ActivatePreparedCompatibleNameRootRepairOptions,
  ): Promise<CompatibleNameCoordinator> {
    const ledger = await options.openLedger()
    try {
      const snapshot = await ledger.loadOperation(options.prepared.bootstrap.header.operationId)
      if (snapshot === undefined) {
        throw new DOMException('Durable compatible-name bootstrap is missing', 'InvalidStateError')
      }
      assertPreparedSnapshot(options.prepared.bootstrap, snapshot)
      const coordinator = new CompatibleNameCoordinator({
        ledger,
        snapshot,
        template: options.prepared.template,
        pairHandles: options.pairHandles,
      })
      await options.mutations.run(
        'create-file',
        () => coordinator.ensurePairReady(options.prepared.parent),
      )
      return coordinator
    } catch (error) {
      ledger.close()
      throw error
    }
  }

  static reopen(input: Readonly<{
    ledger: CompatibleNameActivationLedger
    snapshot: CompatibleNameOperationSnapshotV1
    template: RestorationTemplate
    pairHandles: FSACompatibleNamePairHandleRepository
  }>): CompatibleNameCoordinator {
    return new CompatibleNameCoordinator(input)
  }

  static async activateDescendant(input: Readonly<{
    binding: PersistedFSAOperationBinding
    rejected: CompatibleNameRejectedComponentInput
    namespaceClaims: LogicalSiblingNamespaceAuthority
    pairHandles: FSACompatibleNamePairHandleRepository
    openLedger: () => Promise<CompatibleNameActivationLedger>
    preparation: CompatibleNameRootRepairPreparationOptions
  }>): Promise<CompatibleNameCoordinator> {
    assertDescendantRejection(input.rejected)
    const template = (input.preparation.templateProvider ?? provenRestorationTemplateProvider)
      .select(input.preparation.platform)
    const primaryToken = generateCompatibleNamePrimaryToken(input.preparation.randomBits)
    const randomOwnedObjectId = input.preparation.randomOwnedObjectId ?? createOwnedObjectId
    const claims = new WeakMap<FileSystemDirectoryHandle, Set<string>>()
    const target = await allocateCandidate({
      parent: input.rejected.parent,
      operationId: input.binding.intent.operationId,
      logicalPath: input.rejected.artifactPath,
      entryKind: input.rejected.entryKind,
      primaryToken,
      claims,
      membership: input.namespaceClaims.membership(input.rejected.artifactPath.slice(0, -1)),
      physicalName: candidate => candidate.physicalComponent,
      diagnosticStage: input.rejected.rejection.stage,
    })
    const script = await allocateCandidate({
      parent: input.rejected.pairParent,
      operationId: input.binding.intent.operationId,
      logicalPath: [`${RESTORATION_PAIR_READABLE_PREFIX}${template.scriptFileExtension}`],
      entryKind: 'file',
      primaryToken,
      claims,
      membership: input.namespaceClaims.membership([]),
      physicalName: candidate => pairPhysicalName(candidate.token, template.scriptFileExtension),
      diagnosticStage: 'fsa.file.entry.inspect',
    })
    const sidecar = await allocateCandidate({
      parent: input.rejected.pairParent,
      operationId: input.binding.intent.operationId,
      logicalPath: [`${RESTORATION_PAIR_READABLE_PREFIX}${RESTORATION_SIDECAR_EXTENSION}`],
      entryKind: 'file',
      primaryToken,
      claims,
      membership: input.namespaceClaims.membership([]),
      physicalName: candidate => pairPhysicalName(candidate.token, RESTORATION_SIDECAR_EXTENSION),
      diagnosticStage: 'fsa.file.entry.inspect',
    })
    const header = compatibleNameOperationHeaderV1({
      operationId: input.binding.intent.operationId,
      primaryToken,
      authorityRef: input.binding.reservation.authorityRef,
      root: {
        logicalName: input.binding.reservation.logicalReservedName,
        physicalName: input.binding.reservation.physicalName,
      },
      templateId: template.id,
      pairPlacement: 'inside-logical-root',
      pair: {
        script: pairIdentity(input.binding.intent.operationId, 'script', script, randomOwnedObjectId),
        sidecar: pairIdentity(input.binding.intent.operationId, 'sidecar', sidecar, randomOwnedObjectId),
      },
      activationState: 'prepared',
    })
    const initialMapping = compatibleNameMappingV1({
      operationId: input.binding.intent.operationId,
      logicalPath: input.rejected.artifactPath,
      entryKind: input.rejected.entryKind,
      physicalComponent: target.physicalComponent,
      attempt: target.attempt,
      token: target.token,
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    const bootstrap = compatibleNameOperationBootstrapV1({ header, initialMapping })
    const ledger = await input.openLedger()
    try {
      const snapshot = await ledger.bootstrapOperation(bootstrap)
      const coordinator = new CompatibleNameCoordinator({
        ledger,
        snapshot,
        template,
        pairHandles: input.pairHandles,
      })
      // Descendant activation is entered only through the caller's ordered multi-parent scope.
      await coordinator.ensurePairReady(input.rejected.pairParent)
      return coordinator
    } catch (error) {
      ledger.close()
      throw error
    }
  }

  get pairPlacement(): CompatibleNamePairPlacement {
    return this.resolver.header.pairPlacement
  }

  get rootEntryKind(): CompatibleNameEntryKind | undefined {
    return this.#rootMapping?.entryKind
  }

  get repairSummary(): CompatibleNameRepairSummary | undefined {
    return this.#repairSummary
  }

  get pendingTerminalOutcome(): CompatibleNamePendingTerminalOutcomeV1 | undefined {
    return this.resolver.header.pendingTerminalOutcome
  }

  mapping(artifactPath: readonly string[], entryKind: CompatibleNameEntryKind) {
    return this.resolver.mapping(this.#ledgerPath(artifactPath, entryKind), entryKind)
  }

  hasMappingForCreatedTarget(
    artifactPath: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): boolean {
    return (artifactPath.length === 0 && this.#rootMapping?.entryKind === entryKind) ||
      this.mapping(artifactPath, entryKind) !== undefined
  }

  physicalComponent(artifactPath: readonly string[], entryKind: CompatibleNameEntryKind): string {
    const logicalPath = this.#ledgerPath(artifactPath, entryKind)
    return this.resolver.physicalComponent(logicalPath, entryKind)
  }

  hasClaimedLogicalSibling(parentArtifactPath: readonly string[], component: string): boolean {
    return this.resolver.hasClaimedLogicalSibling(this.#ledgerParentPath(parentArtifactPath), component)
  }

  physicalChild(
    parentArtifactPath: readonly string[],
    physicalComponent: string,
    entryKind: CompatibleNameEntryKind,
  ): CompatiblePhysicalChild {
    return this.resolver.physicalChild(
      this.#ledgerParentPath(parentArtifactPath),
      physicalComponent,
      entryKind,
    )
  }

  async claimRejectedComponent(
    input: CompatibleNameRejectedComponentInput,
    namespaceClaims: LogicalSiblingNamespaceAuthority,
  ): Promise<string> {
    this.#assertOpen()
    assertDescendantRejection(input)
    const existing = this.mapping(input.artifactPath, input.entryKind)
    if (existing !== undefined) return existing.physicalComponent
    const logicalPath = this.#ledgerPath(input.artifactPath, input.entryKind)
    const candidate = await allocateCandidate({
      parent: input.parent,
      operationId: this.resolver.operationId,
      logicalPath,
      entryKind: input.entryKind,
      primaryToken: this.resolver.header.primaryToken,
      claims: new WeakMap<FileSystemDirectoryHandle, Set<string>>(),
      resolver: this.resolver,
      membership: namespaceClaims.membership(input.artifactPath.slice(0, -1)),
      physicalName: value => value.physicalComponent,
      diagnosticStage: input.rejection.stage,
    })
    const claimed = await this.#ledger.claimMapping({
      operationId: this.resolver.operationId,
      logicalPath,
      entryKind: input.entryKind,
      physicalComponent: candidate.physicalComponent,
      attempt: candidate.attempt,
      token: candidate.token,
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    this.resolver.adoptMapping(claimed)
    return claimed.physicalComponent
  }

  async ensurePairReady(parent: FileSystemDirectoryHandle): Promise<void> {
    this.#assertOpen()
    if (this.#pairReadyVerified) return
    const header = this.resolver.header
    const pairWasAlreadyOwned = header.pair.script.ownershipState === 'owned' &&
      header.pair.sidecar.ownershipState === 'owned'
    const initialSidecarBytes = TEXT_ENCODER.encode(
      encodeCompatibleNameSidecarHeader({
        operationId: header.operationId,
        placement: compatibleNameSidecarPlacement(header.pairPlacement),
      }) + encodeCompatibleNameSidecarFooter({ committedCount: 0, state: 'active' }),
    )
    await this.#ensureOwnedPairFile(
      parent,
      'script',
      TEXT_ENCODER.encode(this.#template.source),
    )
    await this.#ensureOwnedPairFile(parent, 'sidecar', initialSidecarBytes)
    if (!pairWasAlreadyOwned) {
      // A newly prepared pair must be pristine before the first target mutation. On reopen, the
      // ownership-verified projector is the authority that truncates or rebuilds a crash-torn tail.
      await this.#validateReopenableSidecar(await this.#openPairFile(parent, 'sidecar'))
    }
    this.#pairReadyVerified = true
    if (this.resolver.header.activationState === 'active') await this.#ensureProjector(parent)
  }

  async recordCompatibleTargetCreated(
    artifactPath: readonly string[],
    entryKind: CompatibleNameEntryKind,
    pairParent: FileSystemDirectoryHandle,
  ): Promise<void> {
    this.#assertOpen()
    const mapping = artifactPath.length === 0 && this.#rootMapping?.entryKind === entryKind
      ? this.#rootMapping
      : this.mapping(artifactPath, entryKind)
    if (mapping === undefined) return
    const summary = this.#summary(undefined, false)
    this.resolver.adoptHeader(await this.#ledger.recordCompatibleTargetCreated({
      operationId: this.resolver.operationId,
      logicalPath: mapping.logicalPath,
      entryKind,
      repairSummary: summary,
    }))
    this.#adoptRepairSummary(this.resolver.header.repairSummary!)
    await this.#ensureProjector(pairParent)
  }

  async persistPendingTerminalOutcome(
    outcome: CompatibleNamePendingTerminalOutcomeV1,
  ): Promise<void> {
    this.#assertOpen()
    const projector = this.#requireProjector()
    const summary = this.#summary(projector.observeFooter(), true)
    this.resolver.adoptHeader(await this.#ledger.persistPendingTerminalOutcome({
      operationId: this.resolver.operationId,
      outcome,
      repairSummary: summary,
    }))
    this.#adoptRepairSummary(summary)
  }

  async drainTerminalProjector(
    state: CompatibleNameTerminalFooterState,
  ): Promise<CompatibleNameRepairSummary> {
    this.#assertOpen()
    const observation = await this.#requireProjector().drainTerminal(state)
    const summary = this.#summary(observation, false)
    // The projector callback already persisted this exact observation. Keeping the
    // explicit equality check makes terminal publication depend on durable summary truth.
    if (!sameRepairSummary(this.#repairSummary, summary)) {
      throw new DOMException('Compatible-name terminal repair summary was not persisted', 'InvalidStateError')
    }
    return summary
  }

  async clearPendingTerminalOutcome(): Promise<void> {
    this.#assertOpen()
    const summary = this.#repairSummary
    if (summary === undefined) {
      throw new DOMException('Compatible-name terminal repair summary is unavailable', 'InvalidStateError')
    }
    this.resolver.adoptHeader(await this.#ledger.clearPendingTerminalOutcome({
      operationId: this.resolver.operationId,
      repairSummary: summary,
    }))
  }

  async commitVerifiedRootDirectory(ownedObjectIdInput: string): Promise<void> {
    this.#assertOpen()
    if (this.#rootMapping?.entryKind !== 'directory') return
    await this.#commitDirectory(this.#rootMapping.logicalPath, ownedObjectIdInput)
  }

  async commitVerifiedDirectory(
    artifactPath: readonly string[],
    ownedObjectIdInput: string,
  ): Promise<void> {
    this.#assertOpen()
    const mapping = this.mapping(artifactPath, 'directory')
    if (mapping === undefined) return
    await this.#commitDirectory(mapping.logicalPath, ownedObjectIdInput)
  }

  async commitFinalFile(artifactPath: readonly string[], ownedObjectIdInput: string): Promise<void> {
    this.#assertOpen()
    const mapping = this.#rootMapping?.entryKind === 'file'
      ? this.#rootMapping
      : this.mapping(artifactPath, 'file')
    if (mapping === undefined) return
    const ownedObjectId = requireOwnedObjectId(ownedObjectIdInput)
    this.#adoptCommittedMapping(await this.#ledger.commitMapping({
      operationId: mapping.operationId,
      logicalPath: mapping.logicalPath,
      entryKind: 'file',
      ownedObjectId,
    }))
  }

  async removeVerifiedEmptyRepair(
    parent: FileSystemDirectoryHandle,
  ): Promise<Exclude<CompatibleNameEmptyRepairRemoval, { kind: 'absent' }>> {
    this.#assertOpen()
    const committed = await this.#ledger.scanCommittedMappings(this.resolver.operationId, 0)
    if (committed.length !== 0) {
      return Object.freeze({ kind: 'not-empty', committedCount: committed.length })
    }
    await this.ensurePairReady(parent)
    const verified = new Set<CompatibleNamePairKind>()
    for (const pairKind of ['sidecar', 'script'] as const) {
      const current = await this.#openOwnedPairFile(parent, pairKind)
      if (pairKind === 'script') {
        if (!equalBytes(await readFileBytes(current), TEXT_ENCODER.encode(this.#template.source))) {
          throw new DOMException('Compatible-name script changed before cleanup', 'InvalidStateError')
        }
      } else {
        await this.#validateActiveSidecar(current)
      }
      verified.add(pairKind)
    }
    const removedObjectIds: string[] = []
    const removedHandleIds: string[] = []
    for (const pairKind of ['sidecar', 'script'] as const) {
      const identity = this.resolver.header.pair[pairKind]
      if (!verified.has(pairKind)) {
        throw new DOMException('Compatible-name pair verification was lost', 'InvalidStateError')
      }
      await parent.removeEntry(identity.physicalName)
      removedObjectIds.push(identity.ownedObjectId)
      removedHandleIds.push(identity.handleId)
    }
    await this.#ledger.removeVerifiedEmptyOperation(this.resolver.header)
    return Object.freeze({
      kind: 'removed',
      removedObjectIds: Object.freeze(removedObjectIds),
      removedHandleIds: Object.freeze(removedHandleIds),
    })
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#ledger.close()
  }

  async #commitDirectory(logicalPath: readonly string[], ownedObjectIdInput: string): Promise<void> {
    const ownedObjectId = requireOwnedObjectId(ownedObjectIdInput)
    const owned = await this.#ledger.recordVerifiedDirectoryOwnership({
      operationId: this.resolver.operationId,
      logicalPath,
      entryKind: 'directory',
      ownedObjectId,
    })
    this.resolver.adoptMapping(owned)
    this.#adoptCommittedMapping(await this.#ledger.commitMapping({
      operationId: this.resolver.operationId,
      logicalPath,
      entryKind: 'directory',
      ownedObjectId,
    }))
  }

  #adoptCommittedMapping(mapping: CompatibleNameMappingV1): void {
    this.resolver.adoptMapping(mapping)
    const ordinal = mapping.commitOrdinal
    if (ordinal === undefined) {
      throw new DOMException('Compatible-name committed mapping omitted its ordinal', 'InvalidStateError')
    }
    const existing = this.#committedMappings.get(ordinal)
    this.#committedMappings.set(ordinal, mapping)
    if (existing !== undefined) return

    const summary = this.#summary(this.#repairSummary?.latestObservedFooter, true)
    // commitMapping has already persisted this count/sample update atomically. The
    // projector notification deliberately has no promise for the worker to await.
    this.#adoptRepairSummary(summary)
    this.#requireProjector().markDirty()
  }

  async #ensureOwnedPairFile(
    parent: FileSystemDirectoryHandle,
    pairKind: CompatibleNamePairKind,
    initialBytes: Uint8Array,
  ): Promise<void> {
    const identity = this.resolver.header.pair[pairKind]
    if (identity.ownershipState === 'owned') {
      const current = await this.#openOwnedPairFile(parent, pairKind)
      if (pairKind === 'script' && !equalBytes(await readFileBytes(current), initialBytes)) {
        throw new DOMException('Compatible-name restoration script changed', 'InvalidStateError')
      }
      return
    }
    const inspection = await inspectFileSystemComponent({
      verifiedParent: parent,
      component: identity.physicalName,
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'diagnostic',
    })
    if (inspection !== 'absent') {
      throw new DOMException('A claimed compatible-name restoration pair entry is occupied', 'InvalidStateError')
    }
    const created = await parent.getFileHandle(identity.physicalName, { create: true })
    await replaceAndCloseFile(created, initialBytes)
    const reopened = await this.#openPairFile(parent, pairKind)
    if (!await reopened.isSameEntry(created) ||
        !equalBytes(await readFileBytes(reopened), initialBytes)) {
      throw new DOMException('Compatible-name restoration pair ownership could not be verified', 'InvalidStateError')
    }
    this.resolver.adoptHeader(await this.#ledger.recordPairOwnership({
      operationId: this.resolver.operationId,
      pairKind,
      handle: reopened,
    }))
  }

  async #openOwnedPairFile(
    parent: FileSystemDirectoryHandle,
    pairKind: CompatibleNamePairKind,
  ): Promise<FileSystemFileHandle> {
    const persisted = await readFSACompatibleNamePairHandle({
      repository: this.#pairHandles,
      header: this.resolver.header,
      pairKind,
    })
    const current = await this.#openPairFile(parent, pairKind)
    if (!await current.isSameEntry(persisted)) {
      throw new DOMException('Compatible-name restoration pair ownership changed', 'InvalidStateError')
    }
    return current
  }

  #openPairFile(
    parent: FileSystemDirectoryHandle,
    pairKind: CompatibleNamePairKind,
  ): Promise<FileSystemFileHandle> {
    return parent.getFileHandle(this.resolver.header.pair[pairKind].physicalName)
  }

  async #validateActiveSidecar(handle: FileSystemFileHandle): Promise<void> {
    const checkpoint = decodeCompatibleNameSidecar(await readFileBytes(handle))
    const header = this.resolver.header
    if (checkpoint.header.operationId !== header.operationId ||
        checkpoint.header.placement !== compatibleNameSidecarPlacement(header.pairPlacement) ||
        checkpoint.footer.state !== 'active' || checkpoint.trailingByteLength !== 0) {
      throw new DOMException(
        'Compatible-name sidecar does not contain a valid active checkpoint',
        'InvalidStateError',
      )
    }
  }

  async #validateReopenableSidecar(handle: FileSystemFileHandle): Promise<void> {
    const checkpoint = decodeCompatibleNameSidecar(await readFileBytes(handle))
    const header = this.resolver.header
    const pendingState = header.pendingTerminalOutcome?.footerState
    const acceptedState = checkpoint.footer.state === 'active' ||
      (pendingState !== undefined && checkpoint.footer.state === pendingState)
    if (checkpoint.header.operationId !== header.operationId ||
        checkpoint.header.placement !== compatibleNameSidecarPlacement(header.pairPlacement) ||
        !acceptedState || checkpoint.trailingByteLength !== 0) {
      throw new DOMException(
        'Compatible-name sidecar does not match its active or pending terminal checkpoint',
        'InvalidStateError',
      )
    }
  }

  async #persistProjectedCheckpoint(
    observation: Readonly<{ committedCount: number; state: 'active' | CompatibleNameTerminalFooterState }>,
  ): Promise<void> {
    const summary = this.#summary(
      observation,
      observation.committedCount !== this.#committedMappings.size,
    )
    this.resolver.adoptHeader(await this.#ledger.persistRepairSummary(
      this.resolver.operationId,
      summary,
    ))
    this.#adoptRepairSummary(summary)
  }

  #summary(
    footer: CompatibleNameRepairSummary['latestObservedFooter'],
    pendingCatchUp = footer === undefined
      ? this.#committedMappings.size !== 0
      : footer.committedCount !== this.#committedMappings.size,
  ): CompatibleNameRepairSummary {
    const logicalPathSample: readonly (readonly string[])[] = Object.freeze(
      [...this.#committedMappings.values()]
        .sort((left, right) => (left.commitOrdinal ?? 0) - (right.commitOrdinal ?? 0))
        .reduce<readonly (readonly string[])[]>((sample, mapping) => {
          if (sample.length >= MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS ||
              sample.some(path => sameLogicalPath(path, mapping.logicalPath))) return sample
          return [...sample, mapping.logicalPath]
        }, []),
    )
    const header = this.resolver.header
    return compatibleNameRepairSummary({
      committedCount: this.#committedMappings.size,
      logicalPathSample,
      pairDisplayNames: {
        script: header.pair.script.physicalName,
        sidecar: header.pair.sidecar.physicalName,
      },
      placement: header.pairPlacement,
      runCommand: `powershell.exe -NoProfile -ExecutionPolicy Bypass -File ` +
        `".\\${header.pair.script.physicalName}" ` +
        `-SidecarPath ".\\${header.pair.sidecar.physicalName}"`,
      ...(footer === undefined ? {} : { latestObservedFooter: footer }),
      pendingCatchUp,
    })
  }

  #adoptRepairSummary(summary: CompatibleNameRepairSummary): void {
    this.#repairSummary = summary
    this.#projection.publish(summary)
  }

  async #ensureProjector(parent: FileSystemDirectoryHandle): Promise<void> {
    if (this.#projector !== undefined) return
    this.#projector = await createCompatibleNameProjector({
      operationId: this.resolver.operationId,
      pairPlacement: this.pairPlacement,
      ledger: this.#ledger,
      writer: new FSAOwnedSidecarWriter(
        () => this.#openOwnedPairFile(parent, 'sidecar'),
      ),
      checkpointed: observation => this.#persistProjectedCheckpoint(observation),
    })
  }

  #requireProjector(): CompatibleNameProjector {
    if (this.#projector === undefined) {
      throw new DOMException('Compatible-name projector is not active', 'InvalidStateError')
    }
    return this.#projector
  }

  #ledgerPath(
    artifactPathInput: readonly string[],
    entryKind: CompatibleNameEntryKind,
  ): readonly string[] {
    const artifactPath = snapshotMaterializationRootRelativePath(artifactPathInput)
    if (this.#rootMapping?.entryKind === 'file' && entryKind === 'file') {
      return this.#rootMapping.logicalPath
    }
    return this.pairPlacement === 'beside-mapped-root'
      ? Object.freeze([this.resolver.header.root.logicalName, ...artifactPath])
      : artifactPath
  }

  #ledgerParentPath(parentArtifactPathInput: readonly string[]): readonly string[] {
    const parentArtifactPath = parentArtifactPathInput.length === 0
      ? Object.freeze([])
      : snapshotMaterializationRootRelativePath(parentArtifactPathInput)
    return this.pairPlacement === 'beside-mapped-root'
      ? Object.freeze([this.resolver.header.root.logicalName, ...parentArtifactPath])
      : parentArtifactPath
  }

  #assertOpen(): void {
    if (this.#closed) {
      throw new DOMException('Compatible-name coordinator is closed', 'InvalidStateError')
    }
  }
}
