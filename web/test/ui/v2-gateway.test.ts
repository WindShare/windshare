import { beforeEach, describe, expect, it, vi } from 'vitest'

const captured = vi.hoisted(() => ({
  sessionOptions: [] as Array<Record<string, unknown>>,
  factoryOptions: [] as Array<Record<string, unknown>>,
  supervisorOptions: [] as Array<Record<string, unknown>>,
  catalogOptions: [] as Array<Record<string, unknown>>,
  openCatalogPageStore: vi.fn(),
}))

vi.mock('../../src/crypto/suite02-link', () => ({
  decodeSuite02CapabilityKey: vi.fn(),
  parseSuite02CapabilityLink: vi.fn(async () => ({
    shareId: 'share',
    pkHash: new Uint8Array(32),
    readSecret: new Uint8Array(32),
    relays: ['http://relay.test'],
    nodeAliases: [],
  })),
}))

vi.mock('../../src/crypto/bytes', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../src/crypto/bytes')>()
  return {
    ...actual,
    encodeBase64Url: vi.fn(actual.encodeBase64Url),
  }
})

vi.mock('../../src/catalog/records', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../src/catalog/records')>()
  return {
    ...actual,
    openShareDescriptor: vi.fn(async () => ({
      shareInstanceId: 'share-instance',
      syntheticRoot: new Uint8Array(16),
      syntheticRootId: 'root',
    })),
  }
})

vi.mock('../../src/catalog/page-store', () => ({
  openCatalogPageStore: captured.openCatalogPageStore,
}))

vi.mock('../../src/catalog/client', () => ({
  CatalogClient: class {
    constructor(options: Record<string, unknown>) {
      captured.catalogOptions.push(options)
    }

    close(): void {}
  },
}))

vi.mock('../../src/transport/relay/receiver', () => ({
  dialRelayReceiver: vi.fn(async () => ({
    channel: { unavailable: new AbortController().signal },
    descriptorObject: new Uint8Array([1]),
    close: async () => undefined,
  })),
}))

vi.mock('../../src/session/runtime', () => ({
  ReceiverSessionRuntime: class {
    static async connect(options: Record<string, unknown>) {
      captured.sessionOptions.push(options)
      return {
        initialLaneId: 7,
        close: async () => undefined,
      }
    }
  },
}))

vi.mock('../../src/receiver/session-factory', () => ({
  BrowserSessionFactory: class {
    constructor(options: Record<string, unknown>) {
      captured.factoryOptions.push(options)
    }

    close(): void {}
  },
}))

vi.mock('../../src/receiver/supervisor', () => ({
  ReceiverReconnectSupervisor: class {
    readonly catalogOperations = {}

    constructor(options: Record<string, unknown>) {
      captured.supervisorOptions.push(options)
    }

    async close(): Promise<void> {}
  },
}))

import { encodeBase64Url } from '../../src/crypto/bytes'
import { defaultICEEndpointPool } from '../../src/connectivity/ice-policy/defaults'
import { resolveLinkRelays } from '../../src/application/network/resolve'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogPage } from '../../src/catalog/records'
import { planArtifactOffers } from '../../src/output/backends/contract'
import { createCatalogStorageObserver } from '../../src/application/diagnostics/receive-trace'
import { validateTraceEventPayloadV2 } from '../../src/diagnostics/export/trace-event-payload-v2'
import type { TraceEventObservationV2 } from '../../src/diagnostics/trace/model'
import {
  BrowseNavigationError,
  BrowserReceiverGateway,
  JoinedBrowserShare,
  type BrowseDirectory,
  type CatalogStorageObservation,
} from '../../src/application/browser-share/gateway'

describe('v2 browser gateway connectivity injection', () => {
  beforeEach(() => {
    captured.sessionOptions.length = 0
    captured.factoryOptions.length = 0
    captured.supervisorOptions.length = 0
    captured.catalogOptions.length = 0
    captured.openCatalogPageStore.mockReset()
    captured.openCatalogPageStore.mockResolvedValue({
      store: { coordinationScope: 'cross-context', close: vi.fn() },
      mode: 'indexeddb',
    })
  })

  it('forwards lazy protocol and connectivity traces through every generation boundary', async () => {
    const offersFactory = () => ({ offer: vi.fn() }) as never
    const nativePeerUsable = () => true
    const protocolTrace = { current: vi.fn() }
    const connectivityTrace = { current: vi.fn() }
    const onBlockDispatched = vi.fn()
    const onBlockFetched = vi.fn()
    const onContentLaneAdmitted = vi.fn()
    const onContentLaneDetached = vi.fn()
    const gateway = new BrowserReceiverGateway({
      network: { resolve: resolveLinkRelays, icePool: defaultICEEndpointPool },
      offersFactory,
      nativePeerUsable,
      protocolTrace,
      connectivityTrace,
      onBlockDispatched,
      onBlockFetched,
      onContentLaneAdmitted,
      onContentLaneDetached,
    })

    const joined = await gateway.join(
      'https://receiver.test/s/share#key',
      'https://receiver.test/s/share',
    )
    const options = captured.supervisorOptions.at(-1)

    expect(options?.offersFactory).toBe(offersFactory)
    expect(options?.nativePeerUsable).toBe(nativePeerUsable)
    expect(options?.connectivityTrace).toBe(connectivityTrace)
    expect(captured.sessionOptions.at(-1)?.protocolTrace).toBe(protocolTrace)
    expect(captured.factoryOptions.at(-1)?.protocolTrace).toBe(protocolTrace)
    expect(options?.onBlockDispatched).toBe(onBlockDispatched)
    expect(options?.onBlockFetched).toBe(onBlockFetched)
    expect(options?.onContentLaneAdmitted).toBe(onContentLaneAdmitted)
    expect(options?.onContentLaneDetached).toBe(onContentLaneDetached)
    await joined.close()
  })

  it('joins with the fallback memory catalog store and emits a validated storage_fallback trace', async () => {
    const denial = new DOMException('Storage denied', 'SecurityError')
    const fallbackStore = { coordinationScope: 'process', close: vi.fn() }
    captured.openCatalogPageStore.mockResolvedValueOnce({
      store: fallbackStore,
      mode: 'memory',
      fallback: { reason: 'storage-denied', failure: denial },
    })
    const events: TraceEventObservationV2[] = []
    const observations: CatalogStorageObservation[] = []
    const traceObserver = createCatalogStorageObserver({ current: event => events.push(event) })
    const gateway = new BrowserReceiverGateway({
      network: { resolve: resolveLinkRelays, icePool: defaultICEEndpointPool },
      onCatalogStorageFallback: event => {
        observations.push(event)
        traceObserver(event)
        throw new Error('observer failures cannot abort join')
      },
    })

    const joined = await gateway.join(
      'https://receiver.test/s/share#key',
      'https://receiver.test/s/share',
    )

    expect(captured.catalogOptions.at(-1)?.store).toBe(fallbackStore)
    expect(observations).toEqual([{
      shareInstanceId: 'share-instance',
      transition: 'storage_fallback',
      reason: 'storage-denied',
      failure: denial,
    }])
    expect(events).toHaveLength(1)
    expect(events[0]?.eventName).toBe('catalog_storage')
    expect(() => validateTraceEventPayloadV2(events[0]!.eventName, events[0]!.payload)).not.toThrow()
    expect(events[0]?.payload).toMatchObject({
      share_instance_id: 'share-instance',
      transition: 'storage_fallback',
      reason: 'storage_denied',
    })
    await joined.close()
  })
})

describe('v2 joined-share path admission', () => {
  const joined = new JoinedBrowserShare({
    descriptor: {
      syntheticRoot: identity(1),
      syntheticRootId: 'root',
    } as never,
    supervisor: {} as never,
    catalog: {} as never,
    recoveryIdentity: 'path-admission',
  })

  it('rejects synthetic-root and ancestor identity cycles before constructing a child route', () => {
    const parent = browseDirectory('parent', ['Parent'], ['root', 'parent'])
    expect(() => joined.childDirectory(parent, directoryEntry('root', 'root-loop')))
      .toThrow(BrowseNavigationError)
    expect(() => joined.childDirectory(parent, directoryEntry('parent', 'parent-loop')))
      .toThrow(BrowseNavigationError)
  })

  it('applies the shared 256-component and 32-KiB prospective path admission', () => {
    const deepPath = Array.from({ length: 256 }, (_unused, index) => `depth-${index}`)
    const deepParent = browseDirectory(
      'deep-parent',
      deepPath,
      ['root', ...deepPath.map((_name, index) => `depth-id-${index}`)],
    )
    expect(() => joined.childDirectory(deepParent, directoryEntry('too-deep', 'child')))
      .toThrow(BrowseNavigationError)

    const maximumParentPath = Array.from({ length: 128 }, () => 'a'.repeat(255))
    const byteParent = browseDirectory(
      'byte-parent',
      maximumParentPath,
      ['root', ...maximumParentPath.map((_name, index) => `byte-id-${index}`)],
    )
    expect(() => joined.childDirectory(byteParent, directoryEntry('too-large', 'b')))
      .toThrow(BrowseNavigationError)
  })
})

describe('v2 joined-share projection authority', () => {
  it('forwards ProtocolSession generation subscriptions without synthesizing an initial event', () => {
    const unsubscribe = vi.fn()
    const subscribeProtocolGeneration = vi.fn(() => unsubscribe)
    const joined = new JoinedBrowserShare({
      descriptor: {} as never,
      supervisor: { subscribeProtocolGeneration } as never,
      catalog: {} as never,
      recoveryIdentity: 'generation-subscription',
    })
    const listener = vi.fn()

    const release = joined.subscribeProtocolGeneration(listener)

    expect(subscribeProtocolGeneration).toHaveBeenCalledOnce()
    expect(subscribeProtocolGeneration).toHaveBeenCalledWith(listener)
    expect(listener).not.toHaveBeenCalled()
    release()
    expect(unsubscribe).toHaveBeenCalledOnce()
  })

  it('preserves the sole shared folder identity and recommends its single-object OPFS ZIP', async () => {
    const fixture = projectionCatalogFixture()
    const supervisor = { protocolSessionId: 'session-1' }
    const joined = new JoinedBrowserShare({
      descriptor: {
        shareInstance: fixture.shareInstance,
        shareInstanceId: fixture.shareInstanceId,
        syntheticRoot: fixture.syntheticRoot,
        syntheticRootId: fixture.syntheticRootId,
      } as never,
      supervisor: supervisor as never,
      catalog: fixture.catalog as never,
      recoveryIdentity: 'share-wide-projection',
    })
    const shape = await joined.inspectSelection(joined.selection.snapshot(), new AbortController().signal)
    expect(shape).toEqual({
      kind: 'tree',
      suggestedName: 'shared',
      knownFileCount: 1,
      knownBytes: 68n,
      complete: true,
    })
    const offered = planArtifactOffers({
      epoch: 1n,
      selectionKey: 'selection-key',
      shape,
      capabilities: {
        indexedDb: true,
        fsa: false,
        opfs: true,
        portable: true,
        availableStorageBytes: 100n * 1024n * 1024n,
      },
    })
    if (offered.kind !== 'ready') throw new Error('share-wide ZIP was not offered')
    expect(offered.zip?.backend).toBe('opfs')
    expect(offered.primary).toMatchObject({
      suggestedName: 'shared.zip',
      backend: 'opfs',
      artifactKind: 'zip-archive',
      resumable: true,
    })
  })
})

function projectionCatalogFixture() {
  const shareInstance = identity(10)
  const syntheticRoot = identity(11)
  const childDirectory = identity(12)
  const file = identity(13)
  const root = committedCatalogGeneration({
    shareInstance,
    directoryId: syntheticRoot,
    generation: identity(20),
    commitmentSeed: 30,
    entries: Object.freeze([{
      kind: 'directory' as const,
      id: childDirectory,
      idText: encodeBase64Url(childDirectory),
      name: 'shared',
    }]),
  })
  const child = committedCatalogGeneration({
    shareInstance,
    directoryId: childDirectory,
    generation: identity(21),
    commitmentSeed: 31,
    entries: Object.freeze([{
      kind: 'file' as const,
      id: file,
      idText: encodeBase64Url(file),
      name: 'report.txt',
      expectedSize: 68n,
    }]),
  })
  const generations = new Map([
    [root.committed.directoryIdText, root],
    [child.committed.directoryIdText, child],
  ])
  const catalog = Object.freeze({
    loadDirectory: async (directoryId: Uint8Array<ArrayBuffer>) => {
      const generation = generations.get(encodeBase64Url(directoryId))
      if (generation === undefined) throw new Error('unexpected projection directory')
      return generation.committed
    },
    page: async (committed: CommittedDirectory) => {
      const generation = generations.get(committed.directoryIdText)
      if (generation === undefined) throw new Error('unexpected committed projection generation')
      return generation.page
    },
  })
  return Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot,
    syntheticRootId: encodeBase64Url(syntheticRoot),
    catalog,
  })
}

function committedCatalogGeneration(input: {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly generation: Uint8Array<ArrayBuffer>
  readonly commitmentSeed: number
  readonly entries: CatalogPage['entries']
}): Readonly<{ committed: CommittedDirectory; page: CatalogPage }> {
  const terminalCommitment = commitment(input.commitmentSeed)
  const directoryIdText = encodeBase64Url(input.directoryId)
  const generationText = encodeBase64Url(input.generation)
  const page: CatalogPage = Object.freeze({
    shareInstance: input.shareInstance,
    directoryId: input.directoryId,
    directoryIdText,
    generation: input.generation,
    generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: input.entries,
    omittedCount: 0n,
    objectCommitment: terminalCommitment,
    encodedBytes: 128,
  })
  return Object.freeze({
    committed: Object.freeze({
      directoryIdText,
      generationText,
      directoryId: input.directoryId,
      generation: input.generation,
      pageCount: 1,
      entryCount: input.entries.length,
      omittedCount: 0n,
      terminalCommitment,
    }),
    page,
  })
}

function commitment(seed: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(32)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return value
}

function browseDirectory(
  idText: string,
  path: readonly string[],
  ancestry: readonly string[],
): BrowseDirectory {
  return Object.freeze({
    id: identity(2),
    idText,
    path: Object.freeze([...path]),
    ancestry: Object.freeze([...ancestry]),
  })
}

function directoryEntry(idText: string, name: string) {
  return Object.freeze({ kind: 'directory' as const, id: identity(3), idText, name })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}
