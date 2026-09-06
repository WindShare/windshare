import {
  V2CatalogClient,
  type V2CatalogScanProgressListener,
} from '../catalog/v2-client'
import { IndexedDbV2CatalogPageStore } from '../catalog/v2-page-store'
import { type V2CatalogEntry, type V2ShareDescriptor } from '../catalog/v2-records'
import {
  frozenV2SelectionPolicy,
  V2SelectionPolicy,
  type V2FrozenSelectionPolicy,
} from '../catalog/v2-selection'
import { snapshotPortableCatalogPath } from '../catalog/path-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type { V2ConnectivityTraceSource } from '../connectivity/diagnostics'
import type { V2PeerRecoveryDependencies } from '../connectivity/peer-set/path'
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
import { bindLocalOutputFailureProtocolAttempt } from '../output/diagnostics'
import { V2BrowserSessionFactory } from '../receiver/v2-session-factory'
import { joinBrowserRelays } from '../receiver/browser-join'
import { receiverRelayBases } from '../receiver/relay-race'
import type { ReceiverPathActivitySnapshot } from '../receiver/path-activity'
import type { V2ConnectivityPolicy } from '../connectivity/v2-receiver-policy'
import { V2ReceiverReconnectSupervisor } from '../receiver/v2-supervisor'
import type { V2ProtocolGenerationListener } from '../receiver/v2-supervisor'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import type { V2ProtocolSessionIdentity } from '../session/v2-identities'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type {
  V2BlockDispatchObservation,
  V2BlockRouteObservation,
} from '../content/v2-broker'
import { V2FilePreview } from '../preview/v2-preview'
import { TransferJob, type TransferJobOptions } from '../transfer/v2-job'
import { type ReceiveIntent } from '../transfer/intent'
import type { V2PlanExecutionAuthority } from '../transfer/output-session'
import type { AuthenticatedDiscoverySource } from '../transfer/projection'
import { type V2RelayReceiverConnection } from '../transport/relay/v2-receiver'
import { V2JoinedProjectionSource } from './selection-discovery/source'

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
  #selection = new V2SelectionPolicy(true)

  get selection(): V2SelectionPolicy { return this.#selection }

  replaceSelection(selection: V2SelectionPolicy): void {
    this.#selection = selection
  }

  selectOnlyFile(entry: V2CatalogEntry, ancestry: readonly string[]): void {
    if (entry.kind !== 'file') throw new TypeError('Single-file selection requires a file')
    const selection = new V2SelectionPolicy(false)
    selection.toggle(entry, ancestry)
    this.#selection = selection
  }
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

  subscribeConnection(listener: (snapshot: import('../receiver/connection-state').ReceiverConnectionSnapshot) => void): () => void {
    return this.#supervisor.connection.subscribe(listener)
  }

  subscribePathActivity(listener: (snapshot: ReceiverPathActivitySnapshot) => void): () => void {
    return this.#supervisor.pathActivity.subscribe(listener)
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

  subscribeProtocolGeneration(listener: V2ProtocolGenerationListener): () => void {
    return this.#supervisor.subscribeProtocolGeneration(listener)
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
      | 'onProgress'
      | 'onMeasure'
      | 'trace'
      | 'transferJobId'
      | 'incidentScope'
      | 'outputSettlementDeadline'
    >> & {
      readonly selection?: V2FrozenSelectionPolicy
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
    entry: V2CatalogEntry,
    connectivity: V2ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<V2FilePreview> {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return V2FilePreview.open(entry, content.revisions, content.broker, signal)
  }

  automaticPhotoPreview(
    entry: V2CatalogEntry,
    connectivity: V2ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<V2FilePreview> {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return V2FilePreview.openAutomaticPhoto(entry, content.revisions, content.broker, signal)
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

export interface V2BrowserReceiverGatewayOptions {
  readonly relayBases?: readonly string[]
  readonly policy?: V2ConnectivityPolicy
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
  readonly #relayBases: readonly string[] | undefined
  readonly #policy: V2ConnectivityPolicy
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
      const relayBases = this.#relayBases ?? receiverRelayBases(capability.relayHints.length > 0
        ? capability.relayHints : [new URL(pageUrl).origin])
      const initial = await joinBrowserRelays(relayBases, capability,
        signal ?? new AbortController().signal, this.#protocolTrace)
      relay = initial.relay
      session = initial.session
      const descriptor = initial.descriptor
      signal?.throwIfAborted()
      const recoveryIdentity = [
        capability.shareId,
        encodeBase64Url(capability.pkHash),
        descriptor.shareInstanceId,
      ].join('.')
      sessionFactory = new V2BrowserSessionFactory({
        relayBases,
        capability,
        descriptor,
        descriptorObject: relay.descriptorObject,
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      })
      supervisor = new V2ReceiverReconnectSupervisor({
        descriptor,
        initial,
        policy: this.#policy,
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
