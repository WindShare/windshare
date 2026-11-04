import { afterEach, describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import { ReceiverController } from '../../src/ui/controller'
import { PreviewController } from '../../src/ui/preview-controller'
import { EMPTY_PREVIEW } from '../../src/ui/model'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'
import {
  entry,
  FakeJoined,
  identity,
  identityText,
  turn,
  waitFor,
  directPreviewSnapshot,
  completedProjectionSource
} from './v2-preview-fixture'

afterEach(() => vi.unstubAllGlobals())

describe('automatic photo ownership', () => {
  it('does not fail catalog publication when speculative connectivity admission races replacement', () => {
    const joined = new FakeJoined()
    joined.beginPreviewConnectivity = () => { throw new DOMException('Generation replaced', 'AbortError') }
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    expect(() => previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry)).not.toThrow()
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
  })

  it('yields pending speculation synchronously and leaves an explicit preview usable', async () => {
    const joined = new FakeJoined()
    const automaticPhotoPreview = joined.preview.bind(joined)
    const source = Object.assign(joined, { automaticPhotoPreview }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry)
    expect(snapshot.preview.state).toBe('loading')
    previews.yieldToReceiving()
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    previews.open(source, entry)
    previews.yieldToReceiving()
    await turn()
    expect(snapshot.preview.state).toBe('image')
    expect(joined.previewSignals[1]?.aborted).toBe(false)
    await previews.close()
  })

  it('retains decoded automatic photo while receiving and closes it on explicit dismissal', async () => {
    const joined = new FakeJoined()
    joined.previewCount = 1
    const source = Object.assign(joined, { automaticPhotoPreview: joined.preview.bind(joined) }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry)
    await turn()
    expect(snapshot.preview.state).toBe('image')
    previews.yieldToReceiving()
    expect(snapshot.preview.state).toBe('image')
    expect(joined.sessionCloses).toBe(0)
    previews.cancel()
    await turn()
    expect(joined.sessionCloses).toBe(1)
  })

  it('returns to explicit Preview when automatic header inspection exceeds its budget', async () => {
    const joined = new FakeJoined()
    const source = Object.assign(joined, {
      automaticPhotoPreview: () => Promise.reject(new AutomaticPhotoDeferredError('Use Preview for this larger image')),
    }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry)
    await turn()
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    expect(joined.events).toEqual(['begin-preview-connectivity', 'close-preview-connectivity'])
  })

  it('bounds stalled speculation and does not activate an oversized candidate', async () => {
    vi.useFakeTimers()
    try {
      const joined = new FakeJoined()
      const source = Object.assign(joined, { automaticPhotoPreview: joined.preview.bind(joined) }) as unknown as JoinedBrowserShare
      let snapshot = directPreviewSnapshot()
      const previews = new PreviewController({
        snapshot: () => snapshot,
        publish: next => { snapshot = next },
        publicError: () => 'preview failed',
      })
      previews.openAutomaticPhoto(source, { ...entry, expectedSize: 5n * 1024n * 1024n })
      expect(joined.events).toEqual([])
      previews.openAutomaticPhoto(source, entry)
      await vi.advanceTimersByTimeAsync(5_000)
      expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
      expect(joined.previewSignals[0]?.aborted).toBe(true)
      await previews.close()
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('v2 preview click controller boundary', () => {
  it('starts each explicit preview immediately, replaces the old preview, and never opens an output picker', async () => {
    const showSaveFilePicker = vi.fn()
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker,
    })
    const joined = new FakeJoined({ ...entry, name: 'photo.bin' })
    const gateway = {
      join: async () => joined as unknown as JoinedBrowserShare,
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().phase === 'browsing')
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
    controller.previewMediaFailed(Number.MAX_SAFE_INTEGER)
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
      join: async () => joined as unknown as JoinedBrowserShare,
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().browse.status.includes('257 entries discovered'))
    expect(controller.getSnapshot().browse.status).toContain('257 entries discovered')
    expect(controller.getSnapshot().browse.status).toContain('total still unknown')
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
    const pending: Array<(joined: JoinedBrowserShare) => void> = []
    const gateway = {
      join: () => new Promise<JoinedBrowserShare>((resolve) => pending.push(resolve)),
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })

    controller.initialize({ capabilityInput: 'first-key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => pending.length === 1)
    controller.submitKey('second-key')
    await waitFor(() => pending.length === 2)
    expect(pending).toHaveLength(2)

    pending[1]?.(second as unknown as JoinedBrowserShare)
    await waitFor(() => controller.getSnapshot().rows[0]?.name === 'second.png')
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    pending[0]?.(first as unknown as JoinedBrowserShare)
    await waitFor(() => first.closes === 1)
    expect(first.closes).toBe(1)
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    await controller.dispose()
  })

  it('keeps visible selection facts independent from unresolved artifact projection', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const selection = new SelectionPolicy(false)
    const joined = {
      descriptor: {
        shareInstance: identity(7),
        shareInstanceId: identityText(7),
        syntheticRoot: identity(1),
        syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection,
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => ({
        id: identity(1), idText: identityText(1), name: 'Shared files', path: [], ancestry: [identityText(1)],
      }),
      page: async (directory: BrowseDirectory) => {
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
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      close: async () => undefined,
    } as unknown as JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, {
      receive: {
        retained: {
          list: () => Promise.resolve(Object.freeze({
            operations: Object.freeze([]),
            presentationFailures: Object.freeze([]),
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
    await waitFor(() => controller.getSnapshot().rows.length === 1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    await controller.dispose()
  })
})
