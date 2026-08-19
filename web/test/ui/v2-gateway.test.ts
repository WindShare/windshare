import { beforeEach, describe, expect, it, vi } from 'vitest'

const captured = vi.hoisted(() => ({
  sessionOptions: [] as Array<Record<string, unknown>>,
  factoryOptions: [] as Array<Record<string, unknown>>,
  supervisorOptions: [] as Array<Record<string, unknown>>,
}))

vi.mock('../../src/crypto/suite02-link', () => ({
  decodeSuite02CapabilityKey: vi.fn(),
  parseSuite02CapabilityLink: vi.fn(async () => ({
    shareId: 'share',
    pkHash: new Uint8Array(32),
    readSecret: new Uint8Array(32),
    relayHints: ['http://relay.test'],
  })),
}))

vi.mock('../../src/crypto/bytes', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../src/crypto/bytes')>()
  return {
    ...actual,
    encodeBase64Url: vi.fn(actual.encodeBase64Url),
  }
})

vi.mock('../../src/catalog/v2-records', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../src/catalog/v2-records')>()
  return {
    ...actual,
    openV2ShareDescriptor: vi.fn(async () => ({
      shareInstanceId: 'share-instance',
      syntheticRoot: new Uint8Array(16),
      syntheticRootId: 'root',
    })),
  }
})

vi.mock('../../src/catalog/v2-page-store', () => ({
  IndexedDbV2CatalogPageStore: {
    open: vi.fn(async () => ({})),
  },
}))

vi.mock('../../src/catalog/v2-client', () => ({
  V2CatalogClient: class {
    close(): void {}
  },
}))

vi.mock('../../src/transport/relay/v2-receiver', () => ({
  dialV2RelayReceiver: vi.fn(async () => ({
    channel: {},
    descriptorObject: new Uint8Array([1]),
    close: async () => undefined,
  })),
}))

vi.mock('../../src/session/v2-runtime', () => ({
  V2ReceiverSessionRuntime: class {
    static async connect(options: Record<string, unknown>) {
      captured.sessionOptions.push(options)
      return {
        initialLaneId: 7,
        close: async () => undefined,
      }
    }
  },
}))

vi.mock('../../src/receiver/v2-session-factory', () => ({
  V2BrowserSessionFactory: class {
    constructor(options: Record<string, unknown>) {
      captured.factoryOptions.push(options)
    }

    close(): void {}
  },
}))

vi.mock('../../src/receiver/v2-supervisor', () => ({
  V2ReceiverReconnectSupervisor: class {
    readonly catalogOperations = {}

    constructor(options: Record<string, unknown>) {
      captured.supervisorOptions.push(options)
    }

    async close(): Promise<void> {}
  },
}))

import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogPage } from '../../src/catalog/v2-records'
import { offerArtifacts } from '../../src/output/planning'
import {
  createSelectionSpec,
  selectionRulesSpecFromPolicy,
} from '../../src/transfer/intent'
import {
  V2BrowseNavigationError,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
  type V2BrowseDirectory,
} from '../../src/ui/v2-gateway'
import {
  discoverAuthenticatedSelection,
  nextProjectionEpoch,
  RetryableProjectionDiscoveryError,
  SelectionProjectionController,
  type SelectionProjectionState,
} from '../../src/transfer/projection'
import {
  environment,
  handoffTarget,
  workspaceOffer,
} from '../output/planning/fixture'

describe('v2 browser gateway connectivity injection', () => {
  beforeEach(() => {
    captured.sessionOptions.length = 0
    captured.factoryOptions.length = 0
    captured.supervisorOptions.length = 0
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
    const gateway = new V2BrowserReceiverGateway({
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
})

describe('v2 joined-share path admission', () => {
  const joined = new V2JoinedBrowserShare({
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
      .toThrow(V2BrowseNavigationError)
    expect(() => joined.childDirectory(parent, directoryEntry('parent', 'parent-loop')))
      .toThrow(V2BrowseNavigationError)
  })

  it('applies the shared 256-component and 32-KiB prospective path admission', () => {
    const deepPath = Array.from({ length: 256 }, (_unused, index) => `depth-${index}`)
    const deepParent = browseDirectory(
      'deep-parent',
      deepPath,
      ['root', ...deepPath.map((_name, index) => `depth-id-${index}`)],
    )
    expect(() => joined.childDirectory(deepParent, directoryEntry('too-deep', 'child')))
      .toThrow(V2BrowseNavigationError)

    const maximumParentPath = Array.from({ length: 128 }, () => 'a'.repeat(255))
    const byteParent = browseDirectory(
      'byte-parent',
      maximumParentPath,
      ['root', ...maximumParentPath.map((_name, index) => `byte-id-${index}`)],
    )
    expect(() => joined.childDirectory(byteParent, directoryEntry('too-large', 'b')))
      .toThrow(V2BrowseNavigationError)
  })
})

describe('v2 joined-share projection authority', () => {
  it('turns a reconnect generation change into an explicit retryable projection fence', async () => {
    const supervisor = { protocolSessionId: 'session-1' }
    const joined = new V2JoinedBrowserShare({
      descriptor: {
        syntheticRoot: identity(4),
        syntheticRootId: 'root',
      } as never,
      supervisor: supervisor as never,
      catalog: {} as never,
      recoveryIdentity: 'projection-fence',
    })
    const source = joined.projectionSource(joined.selection.snapshot())
    supervisor.protocolSessionId = 'session-2'

    const discovery = source.discover({
      epoch: nextProjectionEpoch(0n),
      selectionDigest: 'selection',
      retainedGenerations: Object.freeze([]),
      unsettledTargets: Object.freeze([]),
      signal: new AbortController().signal,
    })

    await expect(discovery.next()).rejects.toMatchObject({
      name: RetryableProjectionDiscoveryError.name,
      reason: 'receiver-reconnecting',
    })
  })

  it('settles a share-wide synthetic root once and offers its workspace ZIP', async () => {
    const fixture = projectionCatalogFixture()
    const supervisor = { protocolSessionId: 'session-1' }
    const joined = new V2JoinedBrowserShare({
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
    const frozenSelection = joined.selection.snapshot()
    const selection = await createSelectionSpec({
      shareInstance: fixture.shareInstanceId,
      syntheticRoot: fixture.syntheticRootId,
      rules: selectionRulesSpecFromPolicy(frozenSelection),
    })
    const controller = new SelectionProjectionController()
    controller.beginSelection(selection)
    const states: SelectionProjectionState[] = []

    for await (const state of discoverAuthenticatedSelection(
      controller,
      joined.projectionSource(frozenSelection),
      new AbortController().signal,
    )) {
      states.push(state)
    }

    const final = states.at(-1)
    if (final === undefined) throw new Error('projection did not publish completion')
    expect(states.map((state) => state.projection.unsettledTargets.length)).toEqual([1, 0, 0, 0])
    expect(final).toMatchObject({
      discovery: { kind: 'complete' },
      projection: {
        unsettledTargets: [],
        proof: {
          kind: 'tree',
          layoutBasis: { kind: 'synthetic-selection' },
        },
      },
    })

    const offered = await offerArtifacts(
      final.projection,
      final.discovery,
      environment({ targets: [handoffTarget()], workspace: workspaceOffer() }),
    )
    if (offered.kind !== 'artifact-actions') throw new Error('share-wide ZIP was not offered')
    expect(offered.primary).toMatchObject({
      operation: 'download-zip',
      artifactKind: 'zip-archive',
      suggestedName: 'windshare.zip',
      recovery: 'workspace-resumable',
      plan: {
        kind: 'workspace-then-publish',
        workspace: { kind: 'origin-private-workspace' },
        publicationTarget: { kind: 'browser-handoff' },
      },
      artifact: {
        kind: 'zip-archive',
        suggestedName: 'windshare.zip',
        layout: { class: 'synthetic-selection', name: 'windshare' },
      },
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
    pages: (committed: V2CommittedDirectory) => {
      const generation = generations.get(committed.directoryIdText)
      if (generation === undefined) throw new Error('unexpected committed projection generation')
      return catalogPages(generation.page)
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
  readonly entries: V2CatalogPage['entries']
}): Readonly<{ committed: V2CommittedDirectory; page: V2CatalogPage }> {
  const terminalCommitment = commitment(input.commitmentSeed)
  const directoryIdText = encodeBase64Url(input.directoryId)
  const generationText = encodeBase64Url(input.generation)
  const page: V2CatalogPage = Object.freeze({
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
    senderObjectBytes: 128,
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

async function* catalogPages(page: V2CatalogPage): AsyncGenerator<V2CatalogPage> {
  yield page
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
): V2BrowseDirectory {
  return Object.freeze({
    id: identity(2),
    idText,
    name: idText,
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
