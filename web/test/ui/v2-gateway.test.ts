import { beforeEach, describe, expect, it, vi } from 'vitest'

const captured = vi.hoisted(() => ({
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

vi.mock('../../src/crypto/bytes', () => ({
  encodeBase64Url: vi.fn(() => 'pk-hash'),
}))

vi.mock('../../src/catalog/v2-records', () => ({
  openV2ShareDescriptor: vi.fn(async () => ({
    shareInstanceId: 'share-instance',
    syntheticRoot: new Uint8Array(16),
    syntheticRootId: 'root',
  })),
}))

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
    static async connect() {
      return {
        initialLaneId: 7,
        close: async () => undefined,
      }
    }
  },
}))

vi.mock('../../src/receiver/v2-session-factory', () => ({
  V2BrowserSessionFactory: class {
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

import {
  V2BrowseNavigationError,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
  type V2BrowseDirectory,
} from '../../src/ui/v2-gateway'

describe('v2 browser gateway connectivity injection', () => {
  beforeEach(() => {
    captured.supervisorOptions.length = 0
  })

  it('forwards the real-offer factory and authenticated block observer to the supervisor', async () => {
    const offersFactory = () => ({ offer: vi.fn() }) as never
    const onBlockFetched = vi.fn()
    const gateway = new V2BrowserReceiverGateway({ offersFactory, onBlockFetched })

    const joined = await gateway.join(
      'https://receiver.test/s/share#key',
      'https://receiver.test/s/share',
    )
    const options = captured.supervisorOptions.at(-1)

    expect(options?.offersFactory).toBe(offersFactory)
    expect(options?.onBlockFetched).toBe(onBlockFetched)
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
