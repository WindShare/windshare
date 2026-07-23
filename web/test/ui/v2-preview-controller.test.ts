import { afterEach, describe, expect, it, vi } from 'vitest'

import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import type { V2CatalogScanProgressListener } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  type V2ConnectivityActivation,
  V2ConnectivityRouteAuthority,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2FilePreview } from '../../src/preview/v2-preview'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'

const entry: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identity(2),
  idText: 'file',
  name: 'photo.png',
  expectedSize: 100n,
})

class FakeJoined {
  readonly descriptor = { syntheticRootId: 'root' }
  readonly recoveryIdentity = 'share.recovery'
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
      idText: 'root',
      name: 'Shared files',
      path: [],
      ancestry: ['root'],
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

  beginDownloadConnectivity(sizeClass: 'small' | 'large' | 'unknown'): V2ConnectivityActivation {
    this.events.push(`begin-download-${sizeClass}`)
    const routes = new V2ConnectivityRouteAuthority()
    return {
      routes,
      observeSizeClass: (observed) => this.events.push(`observe-download-${observed}`),
      close: () => {
        routes.close()
        this.events.push('close-download-connectivity')
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
    const controller = new V2ReceiverController(gateway)
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

  it('records download connectivity before size classification and picker acquisition', async () => {
    const joined = new FakeJoined()
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => {
        joined.events.push('picker')
        return Promise.reject(new DOMException('test picker stop', 'AbortError'))
      },
    })
    const gateway = {
      join: async () => joined as unknown as V2JoinedBrowserShare,
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway)
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    joined.events.length = 0

    controller.startDownload()
    expect(joined.events.slice(0, 3)).toEqual([
      'begin-download-unknown',
      'observe-download-small',
      'picker',
    ])
    await turn()
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
    const controller = new V2ReceiverController(gateway)
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
    const controller = new V2ReceiverController(gateway)

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

  it('keeps an explicit selection actionable before every root page is visited', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const selection = new V2SelectionPolicy(false)
    const joined = {
      descriptor: { syntheticRootId: 'root' },
      recoveryIdentity: 'share.recovery',
      selection,
      rootDirectory: () => ({
        id: identity(1), idText: 'root', name: 'Shared files', path: [], ancestry: ['root'],
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
      subscribeCatalogScanProgress: () => () => undefined,
      close: async () => undefined,
    } as unknown as V2JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway)

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().canStart).toBe(false)
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().canStart).toBe(true)
    expect(controller.getSnapshot().selectionTotalKnown).toBe(false)
    await controller.dispose()
  })
})
