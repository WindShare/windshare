import {
  V2CatalogClient,
  V2DirectoryFailureError,
  type V2CatalogScanProgressListener,
} from '../catalog/v2-client'
import { IndexedDbV2CatalogPageStore } from '../catalog/v2-page-store'
import { openV2ShareDescriptor, type V2CatalogEntry, type V2ShareDescriptor } from '../catalog/v2-records'
import {
  frozenV2SelectionPolicy,
  V2SelectionPolicy,
  type V2FrozenSelectionPolicy,
} from '../catalog/v2-selection'
import { snapshotPortableCatalogPath } from '../catalog/path-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type { V2ConnectivityTraceSource } from '../connectivity/diagnostics'
import type { V2PeerRecoveryDependencies } from '../connectivity/v2-peer-recovery'
import type {
  V2ConnectivityActivation,
  V2ContentLaneAdmissionObservation,
  V2ContentLaneDetachmentObservation,
} from '../connectivity/v2-receiver-policy'
import {
  decodeSuite02CapabilityKey,
  parseSuite02CapabilityLink,
  type Suite02CapabilityLink,
} from '../crypto/suite02-link'
import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import { V2BrowserSessionFactory } from '../receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../receiver/v2-supervisor'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import type { V2ProtocolSessionIdentity } from '../session/v2-identities'
import { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type {
  V2BlockDispatchObservation,
  V2BlockRouteObservation,
} from '../content/v2-broker'
import { V2FilePreview } from '../preview/v2-preview'
import { projectAuthenticatedV2Generation } from '../transfer/discovery/v2-projection-evidence'
import { TransferJob, type TransferJobOptions } from '../transfer/v2-job'
import type { ReceiveIntent } from '../transfer/intent'
import type { V2PlanExecutionAuthority } from '../transfer/output-session'
import {
  RetryableProjectionDiscoveryError,
  type AuthenticatedDiscoveryRequest,
  type AuthenticatedDiscoverySource,
  type AuthenticatedProjectionEvidence,
  type SelectedRootFact,
  type SettledLayoutBasisProof,
} from '../transfer/projection'
import { dialV2RelayReceiver, type V2RelayReceiverConnection } from '../transport/relay/v2-receiver'

export interface V2BrowseDirectory {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly name: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
}

export interface V2BrowsePage {
  readonly directory: V2BrowseDirectory
  readonly pageIndex: number
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly entries: readonly V2CatalogEntry[]
}

export class V2BrowseNavigationError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2BrowseNavigationError'
  }
}

export function v2SelectionPolicyFromIntent(
  intent: ReceiveIntent,
): V2FrozenSelectionPolicy {
  if (intent.selection.rules.mode !== 'node-id') {
    throw new TypeError('Browser resume requires node-identity selection authority')
  }
  return frozenV2SelectionPolicy(
    intent.selection.rules.defaultSelected,
    intent.selection.rules.rules.map((rule) => {
      const id = decodeBase64Url(rule.id)
      if (id === undefined) throw new TypeError('Resumed selection rule identity is invalid')
      return Object.freeze({ kind: rule.kind, id, selected: rule.selected })
    }),
  )
}

export class V2JoinedBrowserShare {
  readonly descriptor: V2ShareDescriptor
  readonly recoveryIdentity: string
  readonly selection = new V2SelectionPolicy(true)
  readonly #supervisor: V2ReceiverReconnectSupervisor
  readonly #catalog: V2CatalogClient
  #closeTask: Promise<void> | undefined

  constructor(options: {
    readonly descriptor: V2ShareDescriptor
    readonly supervisor: V2ReceiverReconnectSupervisor
    readonly catalog: V2CatalogClient
    readonly recoveryIdentity: string
  }) {
    this.descriptor = options.descriptor
    this.recoveryIdentity = options.recoveryIdentity
    this.#supervisor = options.supervisor
    this.#catalog = options.catalog
  }

  rootDirectory(): V2BrowseDirectory {
    return Object.freeze({
      id: this.descriptor.syntheticRoot.slice(),
      idText: this.descriptor.syntheticRootId,
      name: 'Shared files',
      path: Object.freeze([]),
      ancestry: Object.freeze([this.descriptor.syntheticRootId]),
    })
  }

  childDirectory(parent: V2BrowseDirectory, entry: V2CatalogEntry): V2BrowseDirectory {
    if (entry.kind !== 'directory') throw new TypeError('Catalog entry is not a directory')
    if (parent.ancestry.includes(entry.idText)) {
      throw new V2BrowseNavigationError('Catalog path contains an ancestor identity cycle')
    }
    let path: readonly string[]
    try {
      // Navigation and transfer discovery share one prospective-path admission.
      // Checking before I/O keeps an over-depth or over-byte route unpublished.
      path = snapshotPortableCatalogPath([...parent.path, entry.name])
    } catch (error) {
      throw new V2BrowseNavigationError('Catalog path exceeds portable path admission', {
        cause: error,
      })
    }
    return Object.freeze({
      id: entry.id.slice(),
      idText: entry.idText,
      name: entry.name,
      path,
      ancestry: Object.freeze([...parent.ancestry, entry.idText]),
    })
  }

  subscribeCatalogScanProgress(listener: V2CatalogScanProgressListener): () => void {
    return this.#catalog.subscribeScanProgress(listener)
  }

  async page(
    directory: V2BrowseDirectory,
    pageIndex: number,
    options: { readonly signal?: AbortSignal; readonly explicitRetry?: boolean } = {},
  ): Promise<V2BrowsePage> {
    const committed = await this.#catalog.loadDirectory(directory.id, options)
    const page = await this.#catalog.page(committed, pageIndex, options.signal)
    return Object.freeze({
      directory,
      pageIndex,
      pageCount: committed.pageCount,
      entryCount: committed.entryCount,
      omittedCount: committed.omittedCount,
      entries: page.entries,
    })
  }

  beginPreviewConnectivity(): V2ConnectivityActivation {
    return this.#supervisor.beginConnectivity('preview')
  }

  beginDownloadConnectivity(): V2ConnectivityActivation {
    return this.#supervisor.beginConnectivity('download')
  }

  get protocolSessionId(): string {
    return this.#supervisor.protocolSessionId
  }

  get protocolSessionIdentity(): V2ProtocolSessionIdentity {
    return this.#supervisor.protocolSessionIdentity
  }

  projectionSource(
    selection: V2FrozenSelectionPolicy,
    explicitRetry = false,
  ): AuthenticatedDiscoverySource {
    return new V2JoinedProjectionSource({
      descriptor: this.descriptor,
      catalog: this.#catalog,
      selection,
      protocolSessionId: () => this.#supervisor.protocolSessionId,
      explicitRetry,
    })
  }

  transferJob(
    plans: V2PlanExecutionAuthority,
    intent: ReceiveIntent,
    connectivity: V2ConnectivityActivation,
    callbacks: Partial<Pick<
      TransferJobOptions,
      'onProgress' | 'onMeasure' | 'trace' | 'transferJobId' | 'incidentScope'
    >> & {
      readonly selection?: V2FrozenSelectionPolicy
    } = {},
  ): TransferJob {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    const { selection = this.selection.snapshot(), ...jobCallbacks } = callbacks
    return new TransferJob({
      descriptor: this.descriptor,
      catalog: this.#catalog,
      selection,
      revisions: content.revisions,
      broker: content.broker,
      lanes: content.lanes,
      plans,
      intent,
      ...jobCallbacks,
    })
  }

  preview(
    entry: V2CatalogEntry,
    connectivity: V2ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<V2FilePreview> {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return V2FilePreview.open(entry, content.revisions, content.broker, signal)
  }

  close(): Promise<void> {
    this.#closeTask ??= this.#close()
    return this.#closeTask
  }

  async #close(): Promise<void> {
    const failures: unknown[] = []
    try {
      this.#catalog.close()
    } catch (error) {
      failures.push(error)
    }
    const results = await Promise.allSettled([this.#supervisor.close()])
    for (const result of results) {
      if (result.status === 'rejected') failures.push(result.reason)
    }
    if (failures.length > 0) throw new AggregateError(failures, 'Closing the joined share failed')
  }
}

interface ProjectionDirectoryCursor {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
  readonly selected: boolean
  readonly selectedDirectoryRoot?: Readonly<{
    directoryId: string
    sourcePath: string
  }>
}

class V2JoinedProjectionSource implements AuthenticatedDiscoverySource {
  readonly #descriptor: V2ShareDescriptor
  readonly #catalog: V2CatalogClient
  readonly #selection: V2FrozenSelectionPolicy
  readonly #protocolSessionId: () => string
  readonly #capturedProtocolSessionId: string
  readonly #explicitRetry: boolean

  constructor(options: {
    readonly descriptor: V2ShareDescriptor
    readonly catalog: V2CatalogClient
    readonly selection: V2FrozenSelectionPolicy
    readonly protocolSessionId: () => string
    readonly explicitRetry: boolean
  }) {
    this.#descriptor = options.descriptor
    this.#catalog = options.catalog
    this.#selection = options.selection
    this.#protocolSessionId = options.protocolSessionId
    this.#capturedProtocolSessionId = options.protocolSessionId()
    this.#explicitRetry = options.explicitRetry
  }

  async *discover(request: AuthenticatedDiscoveryRequest) {
    const rootSelected = this.#selection.directorySelected(
      this.#descriptor.syntheticRootId,
      [],
    )
    const summary = new ProjectionDiscoverySummary(rootSelected)
    const seen = new Set<string>()
    const root: ProjectionDirectoryCursor = Object.freeze({
      id: this.#descriptor.syntheticRoot.slice(),
      idText: this.#descriptor.syntheticRootId,
      path: Object.freeze([]),
      ancestry: Object.freeze([this.#descriptor.syntheticRootId]),
      selected: rootSelected,
    })
    yield* this.#discoverDirectory(root, request, summary, seen)
    request.signal.throwIfAborted()
    this.#requireSameProtocolSession()
    const layoutBasis = summary.layoutBasis()
    // Committed generation evidence owns target settlement. Completion only
    // closes discovery with the cross-generation layout proof; replaying the
    // request here would claim targets whose authority was already consumed.
    return Object.freeze({
      ...(layoutBasis === undefined ? {} : { layoutBasis }),
    })
  }

  async *#discoverDirectory(
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
    seen: Set<string>,
  ): AsyncGenerator<AuthenticatedProjectionEvidence, void> {
    request.signal.throwIfAborted()
    this.#requireSameProtocolSession()
    if (seen.has(cursor.idText)) {
      throw new V2BrowseNavigationError('Catalog projection contains a repeated directory identity')
    }
    seen.add(cursor.idText)

    const committed = await this.#loadCommittedDirectory(cursor, request)
    const evidence = await this.#projectDirectory(committed, cursor, request)
    summary.observe(evidence)
    yield evidence

    for await (const child of this.#discoverableChildren(committed, cursor, request, summary)) {
      yield* this.#discoverDirectory(child, request, summary, seen)
    }
  }

  async #loadCommittedDirectory(
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
  ) {
    try {
      const committed = await this.#catalog.loadDirectory(cursor.id, {
        signal: request.signal,
        explicitRetry: this.#explicitRetry,
      })
      request.signal.throwIfAborted()
      this.#requireSameProtocolSession()
      return committed
    } catch (error) {
      if (error instanceof V2DirectoryFailureError && error.failure.retryable) {
        throw new RetryableProjectionDiscoveryError('catalog-temporarily-unavailable', {
          cause: error,
        })
      }
      throw error
    }
  }

  #projectDirectory(
    committed: Awaited<ReturnType<V2CatalogClient['loadDirectory']>>,
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
  ): Promise<AuthenticatedProjectionEvidence> {
    return projectAuthenticatedV2Generation({
      committed,
      pages: this.#catalog.pages(committed, request.signal),
      selection: this.#selection,
      directoryAncestry: cursor.ancestry,
      directoryPath: cursor.path,
      containingDirectorySelected: cursor.selected,
      unsettledTargets: request.unsettledTargets,
      signal: request.signal,
    })
  }

  async *#discoverableChildren(
    committed: Awaited<ReturnType<V2CatalogClient['loadDirectory']>>,
    cursor: ProjectionDirectoryCursor,
    request: AuthenticatedDiscoveryRequest,
    summary: ProjectionDiscoverySummary,
  ): AsyncGenerator<ProjectionDirectoryCursor, void> {
    for await (const page of this.#catalog.pages(committed, request.signal)) {
      for (const entry of page.entries) {
        request.signal.throwIfAborted()
        const child = this.#projectionChild(cursor, entry, summary)
        if (child !== undefined) yield child
      }
    }
  }

  #projectionChild(
    cursor: ProjectionDirectoryCursor,
    entry: V2CatalogEntry,
    summary: ProjectionDiscoverySummary,
  ): ProjectionDirectoryCursor | undefined {
    const selected = this.#selection.selected(entry, cursor.ancestry)
    if (cursor.selectedDirectoryRoot !== undefined && !selected) {
      summary.markDirectoryRootPartial(cursor.selectedDirectoryRoot.directoryId)
    }
    if (entry.kind !== 'directory' ||
        !this.#selection.shouldDiscover(entry.idText, cursor.ancestry)) return undefined

    const path = snapshotPortableCatalogPath([...cursor.path, entry.name])
    const selectedDirectoryRoot = projectionDirectoryRoot(cursor, entry.idText, path, selected)
    return Object.freeze({
      id: entry.id.slice(),
      idText: entry.idText,
      path,
      ancestry: Object.freeze([...cursor.ancestry, entry.idText]),
      selected,
      ...(selectedDirectoryRoot === undefined ? {} : { selectedDirectoryRoot }),
    })
  }

  #requireSameProtocolSession(): void {
    if (this.#protocolSessionId() !== this.#capturedProtocolSessionId) {
      throw new RetryableProjectionDiscoveryError('receiver-reconnecting')
    }
  }
}

function projectionDirectoryRoot(
  cursor: ProjectionDirectoryCursor,
  directoryId: string,
  path: readonly string[],
  selected: boolean,
): ProjectionDirectoryCursor['selectedDirectoryRoot'] {
  if (!selected) return undefined
  if (cursor.selectedDirectoryRoot !== undefined) return cursor.selectedDirectoryRoot
  if (cursor.selected) return undefined
  return Object.freeze({ directoryId, sourcePath: path.join('/') })
}

class ProjectionDiscoverySummary {
  readonly #syntheticRootSelected: boolean
  readonly #partialDirectoryRoots = new Set<string>()
  #selectedFileCount = 0
  #selectedDirectoryCount = 0
  #selectedRootCount = 0
  #singleSelectedRoot: SelectedRootFact | undefined

  constructor(syntheticRootSelected: boolean) {
    this.#syntheticRootSelected = syntheticRootSelected
  }

  observe(evidence: AuthenticatedProjectionEvidence): void {
    this.#selectedFileCount = Math.min(
      2,
      this.#selectedFileCount + evidence.metrics.fileCountLowerBound,
    )
    this.#selectedDirectoryCount = Math.min(
      1,
      this.#selectedDirectoryCount + evidence.metrics.directoryCountLowerBound,
    )
    if (this.#selectedRootCount === 0 && evidence.selectedRootCount === 1) {
      this.#singleSelectedRoot = evidence.selectedRoots[0]
    }
    this.#selectedRootCount = Math.min(2, this.#selectedRootCount + evidence.selectedRootCount)
    if (this.#selectedRootCount !== 1) this.#singleSelectedRoot = undefined
  }

  markDirectoryRootPartial(directoryId: string): void {
    this.#partialDirectoryRoots.add(directoryId)
  }

  layoutBasis(): SettledLayoutBasisProof | undefined {
    const treeRequired = this.#selectedDirectoryCount > 0 || this.#selectedFileCount > 1
    if (!treeRequired) return undefined
    const root = this.#singleSelectedRoot
    if (!this.#syntheticRootSelected && this.#selectedRootCount === 1 && root?.kind === 'directory') {
      return Object.freeze({
        kind: this.#partialDirectoryRoots.has(root.directoryId)
          ? 'directory-selection' as const
          : 'complete-directory' as const,
        anchor: Object.freeze({
          directoryId: root.directoryId,
          sourcePath: root.sourcePath,
        }),
      })
    }
    return Object.freeze({ kind: 'synthetic-selection' as const })
  }
}

export interface V2BrowserReceiverGatewayOptions {
  readonly offersFactory?: () => OfferChannelFactory
  readonly nativePeerUsable?: () => boolean
  readonly protocolTrace?: V2ProtocolTraceSource
  readonly connectivityTrace?: V2ConnectivityTraceSource
  readonly peerRecovery?: V2PeerRecoveryDependencies
  readonly onBlockDispatched?: (observation: V2BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
  readonly onContentLaneAdmitted?: (observation: V2ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: V2ContentLaneDetachmentObservation) => void
}

export class V2BrowserReceiverGateway {
  readonly #offersFactory: (() => OfferChannelFactory) | undefined
  readonly #nativePeerUsable: (() => boolean) | undefined
  readonly #protocolTrace: V2ProtocolTraceSource | undefined
  readonly #connectivityTrace: V2ConnectivityTraceSource | undefined
  readonly #peerRecovery: V2PeerRecoveryDependencies | undefined
  readonly #onBlockDispatched: ((observation: V2BlockDispatchObservation) => void) | undefined
  readonly #onBlockFetched: ((observation: V2BlockRouteObservation) => void) | undefined
  readonly #onContentLaneAdmitted: (
    (observation: V2ContentLaneAdmissionObservation) => void
  ) | undefined
  readonly #onContentLaneDetached: (
    (observation: V2ContentLaneDetachmentObservation) => void
  ) | undefined

  constructor(options: V2BrowserReceiverGatewayOptions = {}) {
    this.#offersFactory = options.offersFactory
    this.#nativePeerUsable = options.nativePeerUsable
    this.#protocolTrace = options.protocolTrace
    this.#connectivityTrace = options.connectivityTrace
    this.#peerRecovery = options.peerRecovery
    this.#onBlockDispatched = options.onBlockDispatched
    this.#onBlockFetched = options.onBlockFetched
    this.#onContentLaneAdmitted = options.onContentLaneAdmitted
    this.#onContentLaneDetached = options.onContentLaneDetached
  }

  async join(input: string, pageUrl: string, signal?: AbortSignal): Promise<V2JoinedBrowserShare> {
    signal?.throwIfAborted()
    let capability: Suite02CapabilityLink | undefined
    let relay: V2RelayReceiverConnection | undefined
    let session: V2ReceiverSessionRuntime | undefined
    let catalog: V2CatalogClient | undefined
    let supervisor: V2ReceiverReconnectSupervisor | undefined
    let sessionFactory: V2BrowserSessionFactory | undefined
    try {
      capability = await capabilityFromInput(input, pageUrl)
      signal?.throwIfAborted()
      const relayBase = capability.relayHints[0] ?? new URL(pageUrl).origin
      relay = await dialV2RelayReceiver(
        relayBase,
        capability,
        signal === undefined ? {} : { signal },
      )
      const descriptor = await openV2ShareDescriptor(relay.descriptorObject, capability)
      signal?.throwIfAborted()
      const recoveryIdentity = [
        capability.shareId,
        encodeBase64Url(capability.pkHash),
        descriptor.shareInstanceId,
      ].join('.')
      session = await V2ReceiverSessionRuntime.connect({
        descriptor,
        readSecret: capability.readSecret,
        initialChannel: relay.channel,
        ...(signal === undefined ? {} : { signal }),
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      })
      sessionFactory = new V2BrowserSessionFactory({
        relayBase,
        capability,
        descriptor,
        descriptorObject: relay.descriptorObject,
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      })
      supervisor = new V2ReceiverReconnectSupervisor({
        descriptor,
        initial: {
          relay,
          session,
          relayLaneId: session.initialLaneId,
        },
        sessionFactory,
        ...gatewayConnectivityOptions(
          this.#offersFactory,
          this.#nativePeerUsable,
          this.#connectivityTrace,
          this.#peerRecovery,
          this.#onBlockDispatched,
          this.#onBlockFetched,
          this.#onContentLaneAdmitted,
          this.#onContentLaneDetached,
        ),
      })
      const store = await IndexedDbV2CatalogPageStore.open(recoveryIdentity)
      catalog = new V2CatalogClient({
        descriptor,
        readSecret: capability.readSecret,
        operations: supervisor.catalogOperations,
        store,
        storageIdentity: recoveryIdentity,
      })
      signal?.throwIfAborted()
      return new V2JoinedBrowserShare({
        descriptor,
        supervisor,
        catalog,
        recoveryIdentity,
      })
    } catch (error) {
      try {
        catalog?.close()
      } catch {
        // The join failure remains the actionable cause; all other resources are
        // still closed below so a failed storage close cannot interrupt cleanup.
      }
      await Promise.allSettled([
        ...(supervisor === undefined ? [] : [supervisor.close()]),
        ...(supervisor !== undefined || session === undefined ? [] : [session.close()]),
        ...(supervisor !== undefined || relay === undefined ? [] : [relay.close()]),
      ])
      if (supervisor === undefined) sessionFactory?.close()
      throw error
    } finally {
      capability?.readSecret.fill(0)
    }
  }
}

function gatewayConnectivityOptions(
  offersFactory: (() => OfferChannelFactory) | undefined,
  nativePeerUsable: (() => boolean) | undefined,
  connectivityTrace: V2ConnectivityTraceSource | undefined,
  peerRecovery: V2PeerRecoveryDependencies | undefined,
  onBlockDispatched: ((observation: V2BlockDispatchObservation) => void) | undefined,
  onBlockFetched: ((observation: V2BlockRouteObservation) => void) | undefined,
  onContentLaneAdmitted:
    ((observation: V2ContentLaneAdmissionObservation) => void) | undefined,
  onContentLaneDetached:
    ((observation: V2ContentLaneDetachmentObservation) => void) | undefined,
) {
  return {
    ...(offersFactory === undefined ? {} : { offersFactory }),
    ...(nativePeerUsable === undefined ? {} : { nativePeerUsable }),
    ...(connectivityTrace === undefined ? {} : { connectivityTrace }),
    ...(peerRecovery === undefined ? {} : { peerRecovery }),
    ...(onBlockDispatched === undefined ? {} : { onBlockDispatched }),
    ...(onBlockFetched === undefined ? {} : { onBlockFetched }),
    ...(onContentLaneAdmitted === undefined ? {} : { onContentLaneAdmitted }),
    ...(onContentLaneDetached === undefined ? {} : { onContentLaneDetached }),
  }
}

async function capabilityFromInput(input: string, pageUrl: string): Promise<Suite02CapabilityLink> {
  const trimmed = input.trim()
  if (trimmed.includes('://')) return parseSuite02CapabilityLink(trimmed)
  const capability = await decodeSuite02CapabilityKey(trimmed)
  const current = new URL(pageUrl)
  current.pathname = `/s/${capability.shareId}`
  current.hash = trimmed.startsWith('#') ? trimmed : `#${trimmed}`
  return parseSuite02CapabilityLink(current.href)
}
