import { afterEach, describe, expect, it, vi } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { V2CatalogScanProgressListener } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  type V2ConnectivityActivation,
  V2ConnectivityRouteAuthority,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2FilePreview } from '../../src/preview/v2-preview'
import type {
  AuthenticatedDiscoveryRequest,
  AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'

const entry: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identity(2),
  idText: identityText(2),
  name: 'photo.png',
  expectedSize: 100n,
})

class FakeJoined {
  readonly descriptor = {
    shareInstance: identity(7),
    shareInstanceId: identityText(7),
    syntheticRoot: identity(1),
    syntheticRootId: identityText(1),
  }
  readonly recoveryIdentity = 'share.recovery'
  readonly protocolSessionId = identityText(8)
  readonly selection = new V2SelectionPolicy(true)
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly events: string[] = []
  readonly previewSignals: AbortSignal[] = []
  previewCount = 0
  sessionCloses = 0
  closes = 0
  progressOnPage: bigint | undefined
  pageGate: Promise<void> | undefined
  #scanProgress: V2CatalogScanProgressListener | undefined

  constructor(fileEntry = entry) {
    this.entry = fileEntry
  }

  rootDirectory(): V2BrowseDirectory {
    return {
      id: identity(1),
      idText: identityText(1),
      name: 'Shared files',
      path: [],
      ancestry: [identityText(1)],
    }
  }

  async page(directory: V2BrowseDirectory) {
    if (this.progressOnPage !== undefined) {
      this.#scanProgress?.({
        directoryId: directory.id,
        attemptId: identity(9),
        discoveredEntries: this.progressOnPage,
      })
    }
    await this.pageGate
    return {
      directory,
      pageIndex: 0,
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      entries: [this.entry],
    }
  }

  subscribeCatalogScanProgress(listener: V2CatalogScanProgressListener): () => void {
    this.#scanProgress = listener
    return () => {
      if (this.#scanProgress === listener) this.#scanProgress = undefined
    }
  }

  projectionSource(): AuthenticatedDiscoverySource {
    return completedProjectionSource()
  }

  beginPreviewConnectivity(): V2ConnectivityActivation {
    this.events.push('begin-preview-connectivity')
    let closed = false
    const routes = new V2ConnectivityRouteAuthority()
    return {
      routes,
      observeSizeClass: () => undefined,
      close: () => {
        if (closed) return
        closed = true
        routes.close()
        this.events.push('close-preview-connectivity')
      },
    }
  }

  preview(
    _entry: V2CatalogEntry,
    _connectivity: V2ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<V2FilePreview> {
    this.events.push('open-preview')
    this.previewSignals.push(signal)
    this.previewCount += 1
    if (this.previewCount === 1) {
      return new Promise((_resolve, reject) => {
        const abort = () => reject(signal.reason)
        signal.addEventListener('abort', abort, { once: true })
      })
    }
    return Promise.resolve({
      current: {
        kind: 'image',
        name: entry.name,
        url: 'blob:second',
        mimeType: 'image/png',
        width: 20,
        height: 10,
      },
      seek: async () => { throw new Error('not a video') },
      close: async () => {
        this.events.push('close-preview-session')
        this.sessionCloses += 1
      },
    } as unknown as V2FilePreview)
  }

  async close(): Promise<void> { this.closes += 1 }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

async function turn(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

afterEach(() => vi.unstubAllGlobals())

describe('v2 preview click controller boundary', () => {
  it('starts each explicit preview immediately, replaces the old preview, and never opens an output picker', async () => {
    const showSaveFilePicker = vi.fn()
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker,
    })
    const joined = new FakeJoined()
    const gateway = {
      join: async () => joined as unknown as V2JoinedBrowserShare,
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().phase).toBe('browsing')

    controller.previewFile('stale-row')
    expect(joined.events).toEqual([])

    controller.previewFile(entry.idText)
    expect(joined.events.slice(0, 2)).toEqual(['begin-preview-connectivity', 'open-preview'])
    expect(controller.getSnapshot().preview.state).toBe('loading')
    controller.previewFile(entry.idText)
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    await turn()
    expect(controller.getSnapshot().preview).toMatchObject({
      state: 'image',
      url: 'blob:second',
    })
    expect(showSaveFilePicker).not.toHaveBeenCalled()
    controller.previewMediaFailed('blob:stale')
    expect(controller.getSnapshot().preview.state).toBe('image')
    expect(joined.sessionCloses).toBe(0)

    controller.cancelPreview()
    expect(joined.events.slice(-2)).toEqual([
      'close-preview-connectivity',
      'close-preview-session',
    ])
    await turn()
    expect(controller.getSnapshot().preview.state).toBe('idle')
    expect(joined.sessionCloses).toBe(1)
    await controller.dispose()
  })

  it('shows authenticated scan milestones without inventing an exact total', async () => {
    vi.stubGlobal('window', { navigator: { storage: {} } })
    const joined = new FakeJoined()
    let releasePage!: () => void
    joined.progressOnPage = 257n
    joined.pageGate = new Promise<void>((resolve) => { releasePage = resolve })
    const gateway = {
      join: async () => joined as unknown as V2JoinedBrowserShare,
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().status).toContain('257 entries discovered')
    expect(controller.getSnapshot().status).toContain('total still unknown')
    releasePage()
    await turn()
    expect(controller.getSnapshot().phase).toBe('browsing')
    await controller.dispose()
  })

  it('closes a stale join that resolves after a newer receiver session owns the UI', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const first = new FakeJoined({ ...entry, idText: 'first', name: 'first.png' })
    const second = new FakeJoined({ ...entry, idText: 'second', name: 'second.png' })
    const pending: Array<(joined: V2JoinedBrowserShare) => void> = []
    const gateway = {
      join: () => new Promise<V2JoinedBrowserShare>((resolve) => pending.push(resolve)),
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })

    controller.initialize({ capabilityInput: 'first-key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    controller.submitKey('second-key')
    await turn()
    expect(pending).toHaveLength(2)

    pending[1]?.(second as unknown as V2JoinedBrowserShare)
    await turn()
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    pending[0]?.(first as unknown as V2JoinedBrowserShare)
    await turn()
    expect(first.closes).toBe(1)
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    await controller.dispose()
  })

  it('keeps visible selection facts independent from unresolved artifact projection', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const selection = new V2SelectionPolicy(false)
    const joined = {
      descriptor: {
        shareInstance: identity(7),
        shareInstanceId: identityText(7),
        syntheticRoot: identity(1),
        syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      selection,
      rootDirectory: () => ({
        id: identity(1), idText: identityText(1), name: 'Shared files', path: [], ancestry: [identityText(1)],
      }),
      page: async (directory: V2BrowseDirectory) => {
        return {
          directory,
          pageIndex: 0,
          pageCount: 2,
          entryCount: 2,
          omittedCount: 0n,
          entries: [entry],
        }
      },
      projectionSource: () => completedProjectionSource(),
      subscribeCatalogScanProgress: () => () => undefined,
      close: async () => undefined,
    } as unknown as V2JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, {
      receive: {
        retained: {
          list: () => Promise.resolve(Object.freeze({
            operations: Object.freeze([]),
            act: () => Promise.reject(new DOMException('No retained operation', 'InvalidStateError')),
            close: () => undefined,
          })),
        },
        environment: () => new Promise<never>(() => undefined),
        startArtifactAuthority: () => {
          throw new Error('output authority must not start from selection projection')
        },
      },
    })

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().output.projection).toBeNull()
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    await controller.dispose()
  })
})

function completedProjectionSource(): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: async function* (request: AuthenticatedDiscoveryRequest) {
      yield* []
      return Object.freeze({ settledTargets: request.unsettledTargets })
    },
  })
}
