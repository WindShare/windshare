import {
  V2CatalogClient,
  type V2CatalogScanProgressListener,
} from '../catalog/v2-client'
import { IndexedDbV2CatalogPageStore } from '../catalog/v2-page-store'
import { openV2ShareDescriptor, type V2CatalogEntry, type V2ShareDescriptor } from '../catalog/v2-records'
import { V2SelectionPolicy } from '../catalog/v2-selection'
import { snapshotPortableCatalogPath } from '../catalog/path-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type {
  V2ConnectivityActivation,
  V2ContentSizeClass,
} from '../connectivity/v2-receiver-policy'
import {
  decodeSuite02CapabilityKey,
  parseSuite02CapabilityLink,
  type Suite02CapabilityLink,
} from '../crypto/suite02-link'
import { encodeBase64Url } from '../crypto/bytes'
import { V2BrowserSessionFactory } from '../receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../receiver/v2-supervisor'
import { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type { V2BlockRouteObservation } from '../content/v2-broker'
import { V2FilePreview } from '../preview/v2-preview'
import { V2TransferJob, type V2TransferJobOptions } from '../transfer/v2-job'
import type { OutputSession } from '../transfer/output-session'
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

  beginDownloadConnectivity(
    sizeClass: V2ContentSizeClass,
  ): V2ConnectivityActivation {
    return this.#supervisor.beginConnectivity('download', sizeClass)
  }

  transferJob(
    output: OutputSession,
    connectivity: V2ConnectivityActivation,
    callbacks: Pick<V2TransferJobOptions, 'onProgress' | 'onMeasure'> = {},
  ): V2TransferJob {
    const content = this.#supervisor.content.forRoutes(connectivity.routes)
    return new V2TransferJob({
      descriptor: this.descriptor,
      catalog: this.#catalog,
      selection: this.selection,
      revisions: content.revisions,
      broker: content.broker,
      lanes: content.lanes,
      output,
      ...callbacks,
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

export interface V2BrowserReceiverGatewayOptions {
  readonly offersFactory?: () => OfferChannelFactory
  readonly onBlockFetched?: (observation: V2BlockRouteObservation) => void
}

export class V2BrowserReceiverGateway {
  readonly #offersFactory: (() => OfferChannelFactory) | undefined
  readonly #onBlockFetched: ((observation: V2BlockRouteObservation) => void) | undefined

  constructor(options: V2BrowserReceiverGatewayOptions = {}) {
    this.#offersFactory = options.offersFactory
    this.#onBlockFetched = options.onBlockFetched
  }

  async join(input: string, pageUrl: string, signal?: AbortSignal): Promise<V2JoinedBrowserShare> {
    signal?.throwIfAborted()
    const capability = await capabilityFromInput(input, pageUrl)
    signal?.throwIfAborted()
    const relayBase = capability.relayHints[0] ?? new URL(pageUrl).origin
    let relay: V2RelayReceiverConnection | undefined
    let session: V2ReceiverSessionRuntime | undefined
    let catalog: V2CatalogClient | undefined
    let supervisor: V2ReceiverReconnectSupervisor | undefined
    let sessionFactory: V2BrowserSessionFactory | undefined
    try {
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
      })
      sessionFactory = new V2BrowserSessionFactory({
        relayBase,
        capability,
        descriptor,
        descriptorObject: relay.descriptorObject,
      })
      supervisor = new V2ReceiverReconnectSupervisor({
        descriptor,
        initial: {
          relay,
          session,
          relayLaneId: session.initialLaneId,
        },
        sessionFactory,
        ...(this.#offersFactory === undefined
          ? {}
          : { offersFactory: this.#offersFactory }),
        ...(this.#onBlockFetched === undefined
          ? {}
          : { onBlockFetched: this.#onBlockFetched }),
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
      capability.readSecret.fill(0)
    }
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
