import { afterEach, describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import * as outputContract from '../../src/output/backends/contract'
import { ReceiveStore } from '../../src/output/store/database'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import { ReceiverController } from '../../src/ui/controller'
import { PreviewController } from '../../src/ui/preview-controller'
import { resolveWorkspaceTaskLayout } from '../../src/ui/experience/retained-match'
import { EMPTY_PREVIEW, type ReceiverFailure } from '../../src/ui/model'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'
import {
  entry,
  FakeJoined,
  identity,
  identityText,
  rootDirectory,
  turn,
  waitFor,
  directPreviewSnapshot,
  DiagnosticPreviewJoined,
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
    expect(() => previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry, rootDirectory)).not.toThrow()
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
    previews.openAutomaticPhoto(source, entry, rootDirectory)
    expect(snapshot.preview.state).toBe('loading')
    previews.yieldToReceiving()
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    previews.open(source, entry, rootDirectory)
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
    previews.openAutomaticPhoto(source, entry, rootDirectory)
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
    previews.openAutomaticPhoto(source, entry, rootDirectory)
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
      previews.openAutomaticPhoto(source, { ...entry, expectedSize: 5n * 1024n * 1024n }, rootDirectory)
      expect(joined.events).toEqual([])
      previews.openAutomaticPhoto(source, entry, rootDirectory)
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
    const controller = new ReceiverController(gateway)
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
    const controller = new ReceiverController(gateway)
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => {
      const browse = controller.getSnapshot().browse
      return browse.kind === 'loading' && browse.discoveredEntries === 257n
    })
    expect(controller.getSnapshot().entryCount).toBe(0)
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
    const controller = new ReceiverController(gateway)

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
      inspectSelection: () => new Promise<never>(() => undefined),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      close: async () => undefined,
    } as unknown as JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway)

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().rows.length === 1)
    expect(controller.getSnapshot().output.offers?.kind).toBe('confirming')
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.offers?.kind).toBe('confirming')
    await controller.dispose()
  })
})

describe('preview target download, single-photo blob reuse, and workspace task dismissal', () => {
  afterEach(() => vi.restoreAllMocks())

  it('retains activeTarget across loading, image, video, and error states and clears only on cancel or close', async () => {
    const blob = new Blob([Uint8Array.of(1, 2, 3)], { type: 'image/png' })
    const imageJoined = {
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      preview: async () => ({
        current: { kind: 'image', name: entry.name, url: 'blob:img', mimeType: 'image/png', width: 20, height: 10 },
        imageBlob: blob,
        seek: async () => { throw new Error('not video') },
        close: async () => undefined,
      }),
    } as unknown as JoinedBrowserShare
    const videoJoined = new DiagnosticPreviewJoined() as unknown as JoinedBrowserShare
    const failingJoined = new DiagnosticPreviewJoined({ openFailure: new Error('decode failed') }) as unknown as JoinedBrowserShare

    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })

    previews.open(imageJoined, entry, rootDirectory)
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })
    await turn()
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory, imageBlob: blob })

    previews.open(videoJoined, entry, rootDirectory)
    await turn()
    expect(snapshot.preview.state).toBe('video')
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })

    previews.open(failingJoined, entry, rootDirectory)
    await turn()
    expect(snapshot.preview.state).toBe('error')
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })

    previews.cancel()
    expect(previews.activeTarget()).toBeUndefined()

    previews.open(imageJoined, entry, rootDirectory)
    await turn()
    await previews.close()
    expect(previews.activeTarget()).toBeUndefined()
  })

  it('saves decoded preview images during active downloads, closes loading/error only after activation, and keeps video open', async () => {
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const { windowPort, downloads } = createMockDownloadWindow()
    const photoBlob = new Blob([Uint8Array.of(10, 20, 30)], { type: 'image/png' })
    const photoEntry = { ...entry, id: identity(2), idText: identityText(2), name: 'photo.png', expectedSize: 100n }
    const brokenEntry = { ...entry, id: identity(3), idText: identityText(3), name: 'broken.png', expectedSize: 100n }
    const videoEntry = { ...entry, id: identity(4), idText: identityText(4), name: 'clip.mp4', expectedSize: 100n }
    const bigEntry = { ...entry, id: identity(5), idText: identityText(5), name: 'giant.bin', expectedSize: 80n * 1024n * 1024n }

    let releaseJob!: () => void
    const jobRunning = new Promise<void>(resolve => { releaseJob = resolve })
    let jobCalls = 0
    const selection = new SelectionPolicy(true)
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection,
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 4, omittedCount: 0n,
        entries: [photoEntry, brokenEntry, videoEntry, bigEntry],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 4, knownBytes: 400n, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => {
        jobCalls += 1
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          run: async () => {
            if (jobCalls === 1) await jobRunning
            return {
              status: 'completed' as const,
              completedFiles: 1n, completedBytes: 100n,
              discoveredFiles: 1n, discoveredBytes: 100n, discoveryComplete: true,
              failedFiles: 0n, failedDirectories: 0n,
            }
          },
        }
      },
      preview: async (target: typeof photoEntry) => {
        if (target.idText === brokenEntry.idText) throw new Error('corrupt image')
        if (target.idText === videoEntry.idText) {
          return {
            current: {
              kind: 'video', name: target.name, url: 'blob:vid', mimeType: 'video/mp4',
              width: 320, height: 180, durationSeconds: 10, keyframeSeconds: 0, decodeSeconds: 0,
            },
            seek: async () => { throw new Error('unused') },
            close: async () => undefined,
          }
        }
        return {
          current: { kind: 'image', name: target.name, url: 'blob:photo', mimeType: 'image/png', width: 20, height: 10 },
          imageBlob: photoBlob,
          seek: async () => { throw new Error('unused') },
          close: async () => undefined,
        }
      },
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().output.capabilitiesState === 'ready' && controller.getSnapshot().rows.length === 4)

    // Start a video preview and download it — modal stays open while transfer runs.
    controller.previewFile(videoEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'video')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'downloading')
    expect(controller.getSnapshot().preview.state).toBe('video')

    // While the video is actively downloading, switch to a decoded image and save it locally!
    controller.previewFile(photoEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'image')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'saved-image' })
    expect(downloads).toHaveLength(1)
    expect(downloads[0]!.fileName).toBe('photo.png')

    // Finish the background download.
    releaseJob()
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'download-started')

    // Error preview closes once transfer activates on the same file.
    controller.previewFile(brokenEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    await waitFor(() => controller.getSnapshot().preview.state === 'idle')
    await waitFor(() => controller.getSnapshot().output.activeTaskId === null)

    // Switching away before error preview's transfer activates keeps the newly opened preview alive.
    controller.previewFile(brokenEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    controller.previewFile(photoEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'image')
    await waitFor(() => controller.getSnapshot().output.activeTaskId === null)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'image', fileId: photoEntry.idText })

    // Preview startup failure stays local to preview result without polluting global snapshot.error.
    const bigPreviewFail = vi.spyOn(joined, 'preview').mockRejectedValueOnce(new Error('not previewable'))
    controller.previewFile(bigEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    const failedResult = controller.downloadPreviewedFile()
    expect(failedResult).toEqual({
      kind: 'failed',
      failure: { kind: 'save-unavailable', reason: 'portable-limit-exceeded' },
    })
    expect(controller.getSnapshot().error).toBeNull()
    expect(bigPreviewFail).toHaveBeenCalledTimes(1)

    await controller.dispose()
  })
})

describe('single-photo blob reuse, preview picker isolation, and workspace task dismissal', () => {
  afterEach(() => vi.restoreAllMocks())

  it('reuses single-photo imageBlob on default portable save, but starts transfer on redownload or explicit fsa/opfs', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const picker = vi.fn(() => new Promise<FileSystemDirectoryHandle>(() => undefined))
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker,
    }
    const photoBlob = new Blob([Uint8Array.of(1, 2, 3, 4)], { type: 'image/png' })
    let transferStarts = 0
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 1, omittedCount: 0n, entries: [entry],
      }),
      inspectSelection: async () => ({
        kind: 'single-file' as const, file: entry, sourcePath: [entry.name], exactBytes: entry.expectedSize,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      automaticPhotoPreview: async () => ({
        current: { kind: 'image', name: entry.name, url: 'blob:auto', mimeType: 'image/png', width: 20, height: 10 },
        imageBlob: photoBlob,
        seek: async () => { throw new Error('unused') },
        close: async () => undefined,
      }),
      beginDownloadConnectivity: () => {
        transferStarts += 1
        return { routes: [], close: vi.fn() }
      },
      createTransferJob: () => ({
        pause: vi.fn(),
        stop: vi.fn(),
        run: async () => ({
          status: 'completed' as const,
          completedFiles: 1n, completedBytes: entry.expectedSize,
          discoveredFiles: 1n, discoveredBytes: entry.expectedSize, discoveryComplete: true,
          failedFiles: 0n, failedDirectories: 0n,
        }),
      }),
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort, opfsEnv: env },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() =>
      controller.getSnapshot().preview.state === 'image' &&
      controller.getSnapshot().output.offers?.kind === 'ready',
    )

    // Default portable save uses in-memory imageBlob with 0 transfer!
    controller.chooseArtifact('portable:file', 'default')
    expect(transferStarts).toBe(0)
    expect(downloads).toHaveLength(1)
    expect(controller.getSnapshot().output.lifecycle).toMatchObject({ kind: 'download-started', retained: false })

    // Dismissing a handed-to-browser card hides it in workspace while retaining history.
    const taskSnap = controller.presentation.task.getSnapshot()
    const currentId = taskSnap.current?.operationId
    expect(currentId).toBeDefined()
    const dismissedLayout = resolveWorkspaceTaskLayout(taskSnap, new Set([currentId!]))
    expect(dismissedLayout.primaryTask).toBeNull()
    expect(dismissedLayout.collapseRemoteSaving).toBe(false)

    // Explicit redownload starts a real transfer even for portable.
    controller.chooseArtifact('portable:file', 'redownload')
    await waitFor(() => transferStarts === 1)
    await waitFor(() => controller.getSnapshot().output.activeTaskId === null)

    // Explicit FSA opens the directory picker instead of reusing imageBlob.
    controller.chooseArtifact('fsa:file', 'default')
    expect(picker).toHaveBeenCalledTimes(1)
    expect(controller.getSnapshot().output.activation?.kind).toBe('waiting-picker')
    controller.cancelPreparing()
    await waitFor(() => controller.getSnapshot().output.offers?.kind === 'ready')

    // Explicit OPFS also initiates a real transfer instead of reusing in-memory portable blob.
    controller.chooseArtifact('opfs:file', 'default')
    await waitFor(() => transferStarts === 2)
    expect(controller.getSnapshot().output.backend).toBe('opfs')
    await waitFor(() => controller.getSnapshot().output.activeTaskId === null)

    await controller.dispose()
  })

  it('keeps preview open on cancelled picker or item switch, isolates async startup failures, and skips dismissed OPFS cards while keeping ready-to-save', async () => {
    const mib = 1024n * 1024n
    const fsaEntry = { ...entry, id: identity(6), idText: identityText(6), name: 'big-clip.mp4', expectedSize: 80n * mib }
    const otherEntry = { ...entry, id: identity(8), idText: identityText(8), name: 'other.png', expectedSize: 100n }
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null,
    }))
    let rejectPicker!: (error: unknown) => void
    let pickerCall = 0
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => {
        pickerCall += 1
        return new Promise<FileSystemDirectoryHandle>((_res, rej) => { rejectPicker = rej })
      }),
    }
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 2, omittedCount: 0n, entries: [fsaEntry, otherEntry],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 2, knownBytes: 80n * mib, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      preview: async (target: typeof fsaEntry) => {
        if (target.idText === fsaEntry.idText) throw new Error('preview failed')
        return {
          current: { kind: 'image', name: target.name, url: 'blob:other', mimeType: 'image/png', width: 20, height: 10 },
          imageBlob: new Blob([Uint8Array.of(1)], { type: 'image/png' }),
          seek: async () => { throw new Error('unused') },
          close: async () => undefined,
        }
      },
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().output.capabilitiesState === 'ready' && controller.getSnapshot().rows.length === 2)

    // Cancelling the FSA picker keeps the error preview open with zero errors.
    controller.previewFile(fsaEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    const localFailures: ReceiverFailure[] = []
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    rejectPicker(new DOMException('User cancelled', 'AbortError'))
    await turn()
    expect(controller.getSnapshot().preview.state).toBe('error')
    expect(localFailures).toHaveLength(0)
    expect(controller.getSnapshot().error).toBeNull()

    // Non-abort async startup failure while on the same preview goes ONLY to local onAsyncFailure.
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    rejectPicker(new Error('picker denied'))
    await waitFor(() => localFailures.length === 1)
    expect(controller.getSnapshot().error).toBeNull()
    expect(controller.getSnapshot().preview.state).toBe('error')

    // Async startup failure after switching away falls back to global snapshot.error.
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    controller.previewFile(otherEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'image')
    rejectPicker(new Error('late picker failure'))
    await waitFor(() => controller.getSnapshot().error !== null)
    expect(localFailures).toHaveLength(1)
    expect(pickerCall).toBe(3)

    // Verify ready-to-save is NOT dismissible while a matched OPFS handed-to-browser retained card IS dismissible.
    const baseTaskSnap = controller.presentation.task.getSnapshot()
    const readyTask = {
      operationId: 'opfs-ready',
      stage: 'ready-to-save',
      completeness: 'complete',
      primaryAction: { id: 'save', target: { kind: 'retained', action: 'save' }, disabledReason: null },
      secondaryActions: [],
      destructiveActions: [],
    }
    const readyLayout = resolveWorkspaceTaskLayout(
      { ...baseTaskSnap, current: readyTask as unknown as NonNullable<typeof baseTaskSnap.current> },
      new Set(['opfs-ready']),
    )
    expect(readyLayout.primaryTask?.operationId).toBe('opfs-ready')

    const selectionKey = baseTaskSnap.output.offers?.selectionKey ?? 'sel-1'
    const shareInstance = baseTaskSnap.share?.shareInstance ?? identityText(7)
    const opfsHandedTask = {
      operationId: 'opfs-handed',
      stage: 'handed-to-browser',
      completeness: 'complete',
      primaryAction: { id: 'save', target: { kind: 'retained', action: 'save' }, disabledReason: null },
      secondaryActions: [],
      destructiveActions: [],
    }
    const opfsRetainedSnap = {
      ...baseTaskSnap,
      current: null,
      tasks: [opfsHandedTask as unknown as NonNullable<typeof baseTaskSnap.current>],
      retained: {
        kind: 'available' as const,
        pending: null,
        tasks: [{
          taskId: 'opfs-handed',
          shareInstance,
          selectionKey,
          backend: 'opfs' as const,
          lifecycle: { kind: 'download-started' as const, taskId: 'opfs-handed', generation: 2n, updatedAt: 1, retained: true, fileName: 'a.bin' },
          actions: ['save' as const, 'discard' as const],
          activeElsewhere: false,
          canSavePartial: false,
        }],
      },
    }
    expect(resolveWorkspaceTaskLayout(opfsRetainedSnap).primaryTask?.operationId).toBe('opfs-handed')
    expect(resolveWorkspaceTaskLayout(opfsRetainedSnap).collapseRemoteSaving).toBe(true)
    const dismissedOpfsLayout = resolveWorkspaceTaskLayout(opfsRetainedSnap, new Set(['opfs-handed']))
    expect(dismissedOpfsLayout.primaryTask).toBeNull()
    expect(dismissedOpfsLayout.collapseRemoteSaving).toBe(false)

    await controller.dispose()
  })
})
