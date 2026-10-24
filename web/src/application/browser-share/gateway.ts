import {
  CatalogClient,
  type CatalogScanProgressListener,
} from '../../catalog/client'
import { IndexedDbCatalogPageStore } from '../../catalog/page-store'
import { type CatalogEntry, type ShareDescriptor } from '../../catalog/records'
import {
  SelectionPolicy,
  type FrozenSelectionPolicy,
} from '../../catalog/selection'
import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type { OfferChannelFactory } from '../../connectivity/peer-offer'
import type { ConnectivityTraceSource } from '../../connectivity/diagnostics'
import type { PeerRecoveryDependencies } from '../../connectivity/peer-set/path'
import type {
  ConnectivityActivation,
  ContentLaneAdmissionObservation,
  ContentLaneDetachmentObservation,
} from '../../connectivity/receiver-policy'
import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import { capabilityFromInput } from './capability-input'
import { encodeBase64Url } from '../../crypto/bytes'
import { bindLocalOutputFailureProtocolAttempt } from '../../output/diagnostics'
import { BrowserSessionFactory } from '../../receiver/session-factory'
import { joinBrowserRelays } from '../../receiver/browser-join'
import type { InitialJoinOptions } from '../../receiver/initial-join'
import { receiverRelayBases } from '../../receiver/relay-race'
import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import type { ConnectivityPolicy } from '../../connectivity/receiver-policy'
import { ReceiverReconnectSupervisor } from '../../receiver/supervisor'
import type { ProtocolGenerationListener } from '../../receiver/supervisor'
import type { ProtocolTraceSource } from '../../session/diagnostics'
import type { ProtocolSessionIdentity } from '../../session/identities'
import type { ReceiverSessionRuntime } from '../../session/runtime'
import type {
  BlockDispatchObservation,
  BlockRouteObservation,
} from '../../content/broker'
import { FilePreview } from '../../preview/preview'
import { TransferJob, type TransferJobOptions } from '../../transfer/job'
import { type ReceiveIntent } from '../../transfer/intent'
import type { PlanExecutionAuthority } from '../../transfer/output/session'
import type { AuthenticatedDiscoverySource } from '../../transfer/projection'
import { type RelayReceiverConnection } from '../../transport/relay/receiver'
import { JoinedProjectionSource } from './discovery/source'

export interface BrowseDirectory {
  readonly id: Uint8Array<ArrayBuffer>
  readonly idText: string
  readonly name: string
  readonly path: readonly string[]
  readonly ancestry: readonly string[]
}

export interface BrowsePage {
  readonly directory: BrowseDirectory
  readonly pageIndex: number
  readonly pageCount: number
  readonly entryCount: number
  readonly omittedCount: bigint
  readonly entries: readonly CatalogEntry[]
}

export class BrowseNavigationError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'BrowseNavigationError'
  }
}

export class JoinedBrowserShare {
  readonly descriptor: ShareDescriptor
  readonly recoveryIdentity: string
  #selection = new SelectionPolicy(true)

  get selection(): SelectionPolicy { return this.#selection }

  replaceSelection(selection: SelectionPolicy): void {
    this.#selection = selection
  }

  selectOnlyFile(entry: CatalogEntry, ancestry: readonly string[]): void {
    if (entry.kind !== 'file') throw new TypeError('Single-file selection requires a file')
    const selection = new SelectionPolicy(false)
    selection.toggle(entry, ancestry)
    this.#selection = selection
  }
  readonly #supervisor: ReceiverReconnectSupervisor
  readonly #catalog: CatalogClient
  #closeTask: Promise<void> | undefined

  constructor(options: {
    readonly descriptor: ShareDescriptor
    readonly supervisor: ReceiverReconnectSupervisor
    readonly catalog: CatalogClient
    readonly recoveryIdentity: string
  }) {
    this.descriptor = options.descriptor
    this.recoveryIdentity = options.recoveryIdentity
    this.#supervisor = options.supervisor
    this.#catalog = options.catalog
  }

  subscribeConnection(listener: (snapshot: import('../../receiver/connection-state').ReceiverConnectionSnapshot) => void): () => void {
    return this.#supervisor.connection.subscribe(listener)
  }

  requestReconnect(): void { this.#supervisor.requestReconnect() }

  subscribePathActivity(listener: (snapshot: ReceiverPathActivitySnapshot) => void): () => void {
    return this.#supervisor.pathActivity.subscribe(listener)
  }

  rootDirectory(): BrowseDirectory {
    return Object.freeze({
      id: this.descriptor.syntheticRoot.slice(),
      idText: this.descriptor.syntheticRootId,
      name: 'Shared files',
      path: Object.freeze([]),
      ancestry: Object.freeze([this.descriptor.syntheticRootId]),
    })
  }

  childDirectory(parent: BrowseDirectory, entry: CatalogEntry): BrowseDirectory {
    if (entry.kind !== 'directory') throw new TypeError('Catalog entry is not a directory')
    if (parent.ancestry.includes(entry.idText)) {
      throw new BrowseNavigationError('Catalog path contains an ancestor identity cycle')
    }
    let path: readonly string[]
    try {
      // Navigation and transfer discovery share one prospective-path admission.
      // Checking before I/O keeps an over-depth or over-byte route unpublished.
      path = snapshotPortableCatalogPath([...parent.path, entry.name])
    } catch (error) {
      throw new BrowseNavigationError('Catalog path exceeds portable path admission', {
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

  subscribeCatalogScanProgress(listener: CatalogScanProgressListener): () => void {
    return this.#catalog.subscribeScanProgress(listener)
  }

  async page(
    directory: BrowseDirectory,
    pageIndex: number,
    options: { readonly signal?: AbortSignal; readonly explicitRetry?: boolean } = {},
  ): Promise<BrowsePage> {
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

  beginPreviewConnectivity(): ConnectivityActivation {
    return this.#supervisor.beginConnectivity('preview')
  }

  beginDownloadConnectivity(): ConnectivityActivation {
    return this.#supervisor.beginConnectivity('download')
  }

  get protocolSessionId(): string {
    return this.#supervisor.protocolSessionId
  }

  get protocolSessionIdentity(): ProtocolSessionIdentity {
    return this.#supervisor.protocolSessionIdentity
  }

  subscribeProtocolGeneration(listener: ProtocolGenerationListener): () => void {
    return this.#supervisor.subscribeProtocolGeneration(listener)
  }

  projectionSource(
    selection: FrozenSelectionPolicy,
    explicitRetry = false,
  ): AuthenticatedDiscoverySource {
    return new JoinedProjectionSource({
      descriptor: this.descriptor,
      catalog: this.#catalog,
      selection,
      protocolSessionId: () => this.#supervisor.protocolSessionId,
      explicitRetry,
    })
  }

  transferJob(
    plans: PlanExecutionAuthority,
    intent: ReceiveIntent,
    connectivity: ConnectivityActivation,
    callbacks: Partial<Pick<
      TransferJobOptions,
      | 'onProgress'
      | 'onMeasure'
      | 'trace'
      | 'transferJobId'
      | 'incidentScope'
      | 'outputSettlementDeadline'
    >> & {
      readonly selection?: FrozenSelectionPolicy
    } = {},
  ): TransferJob {
    const protocolGeneration = this.#supervisor.generationId
    const protocolSessionIdentity = this.#supervisor.protocolSessionIdentity
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    const { selection = this.selection.snapshot(), ...jobCallbacks } = callbacks
    if (callbacks.incidentScope !== undefined && callbacks.transferJobId !== undefined) {
      try {
        bindLocalOutputFailureProtocolAttempt(callbacks.incidentScope, {
          transferJobId: callbacks.transferJobId,
          protocolSessionIdentity,
          protocolGeneration,
        })
      } catch {
        // Correlation is observational and cannot prevent transfer construction.
      }
    }
    return new TransferJob({
      descriptor: this.descriptor,
      catalog: this.#catalog,
      selection,
      revisions: content.revisions,
      broker: content.broker,
      lanes: content.lanes,
      revisionCapacity: Object.freeze({
        generation: this.#supervisor,
      }),
      plans,
      intent,
      protocol: Object.freeze({
        sessionId: this.#supervisor.protocolSessionId,
        generation: protocolGeneration,
      }),
      ...jobCallbacks,
    })
  }

  preview(
    entry: CatalogEntry,
    connectivity: ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<FilePreview> {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return FilePreview.open(entry, content.revisions, content.broker, signal)
  }

  automaticPhotoPreview(
    entry: CatalogEntry,
    connectivity: ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<FilePreview> {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return FilePreview.openAutomaticPhoto(entry, content.revisions, content.broker, signal)
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

export interface BrowserReceiverGatewayOptions {
  readonly relayBases?: readonly string[]
  readonly policy?: ConnectivityPolicy
  readonly offersFactory?: () => OfferChannelFactory
  readonly nativePeerUsable?: () => boolean
  readonly protocolTrace?: ProtocolTraceSource
  readonly connectivityTrace?: ConnectivityTraceSource
  readonly peerRecovery?: PeerRecoveryDependencies
  readonly onBlockDispatched?: (observation: BlockDispatchObservation) => void
  readonly onBlockFetched?: (observation: BlockRouteObservation) => void
  readonly onContentLaneAdmitted?: (observation: ContentLaneAdmissionObservation) => void
  readonly onContentLaneDetached?: (observation: ContentLaneDetachmentObservation) => void
}

export class BrowserReceiverGateway {
  readonly #relayBases: readonly string[] | undefined
  readonly #policy: ConnectivityPolicy
  readonly #offersFactory: (() => OfferChannelFactory) | undefined
  readonly #nativePeerUsable: (() => boolean) | undefined
  readonly #protocolTrace: ProtocolTraceSource | undefined
  readonly #connectivityTrace: ConnectivityTraceSource | undefined
  readonly #peerRecovery: PeerRecoveryDependencies | undefined
  readonly #onBlockDispatched: ((observation: BlockDispatchObservation) => void) | undefined
  readonly #onBlockFetched: ((observation: BlockRouteObservation) => void) | undefined
  readonly #onContentLaneAdmitted: (
    (observation: ContentLaneAdmissionObservation) => void
  ) | undefined
  readonly #onContentLaneDetached: (
    (observation: ContentLaneDetachmentObservation) => void
  ) | undefined

  constructor(options: BrowserReceiverGatewayOptions = {}) {
    this.#relayBases = options.relayBases === undefined ? undefined : receiverRelayBases(options.relayBases)
    this.#policy = options.policy ?? 'auto'
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

  async join(input: string, pageUrl: string, signal?: AbortSignal, recovery: InitialJoinOptions = {}): Promise<JoinedBrowserShare> {
    signal?.throwIfAborted()
    let capability: Suite02CapabilityLink | undefined
    let relay: RelayReceiverConnection | undefined
    let session: ReceiverSessionRuntime | undefined
    let catalog: CatalogClient | undefined
    let supervisor: ReceiverReconnectSupervisor | undefined
    let sessionFactory: BrowserSessionFactory | undefined
    try {
      capability = await capabilityFromInput(input, pageUrl)
      signal?.throwIfAborted()
      const relayBases = this.#relayBases ?? receiverRelayBases(capability.relays)
      const initial = await joinBrowserRelays(relayBases, capability,
        signal ?? new AbortController().signal, this.#protocolTrace, recovery)
      relay = initial.relay
      session = initial.session
      const descriptor = initial.descriptor
      signal?.throwIfAborted()
      const recoveryIdentity = [
        capability.shareId,
        encodeBase64Url(capability.pkHash),
        descriptor.shareInstanceId,
      ].join('.')
      sessionFactory = new BrowserSessionFactory({
        relayBases,
        capability,
        descriptor,
        descriptorObject: relay.descriptorObject,
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      })
      supervisor = new ReceiverReconnectSupervisor({
        descriptor,
        initial,
        policy: this.#policy,
        sessionFactory,
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
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
      const store = await IndexedDbCatalogPageStore.open(recoveryIdentity)
      catalog = new CatalogClient({
        descriptor,
        readSecret: capability.readSecret,
        operations: supervisor.catalogOperations,
        store,
        storageIdentity: recoveryIdentity,
      })
      signal?.throwIfAborted()
      return new JoinedBrowserShare({
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
  connectivityTrace: ConnectivityTraceSource | undefined,
  peerRecovery: PeerRecoveryDependencies | undefined,
  onBlockDispatched: ((observation: BlockDispatchObservation) => void) | undefined,
  onBlockFetched: ((observation: BlockRouteObservation) => void) | undefined,
  onContentLaneAdmitted:
    ((observation: ContentLaneAdmissionObservation) => void) | undefined,
  onContentLaneDetached:
    ((observation: ContentLaneDetachmentObservation) => void) | undefined,
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
