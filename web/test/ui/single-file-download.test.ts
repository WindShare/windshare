import { afterEach, describe, expect, it, vi } from 'vitest'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import * as outputContract from '../../src/output/backends/contract'
import type { ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { ReceiverController } from '../../src/ui/controller'
import { presentSavingActions } from '../../src/ui/saving'
import {
  singleFileDownloadState,
  type SingleFileDownloadReadiness,
} from '../../src/ui/share-content/single-file-admission'
import { commitMockOutputFile } from '../application/receive-coordinator-fixture'
import { createMockDownloadWindow } from '../output/opfs-fixture'
import { entry, identity, identityText, rootDirectory, waitForController } from './v2-preview-fixture'

const MIB = 1024n * 1024n

function makeReadiness(
  overrides: Partial<SingleFileDownloadReadiness> = {},
): SingleFileDownloadReadiness {
  return {
    startAdmission: { allowed: true, reason: null, canReleaseCurrent: false, localChoice: null },
    capabilitiesState: 'ready',
    activeSingleFileId: null,
    language: 'en',
    ...overrides,
  }
}

function makeBrowseShare(
  entries: readonly CatalogEntry[],
  options: Readonly<{ automaticPhotoPreview?: JoinedBrowserShare['automaticPhotoPreview'] }> = {},
): JoinedBrowserShare {
  return {
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
      directory, pageIndex: 0, pageCount: 1, entryCount: entries.length, omittedCount: 0n, entries,
    }),
    inspectSelection: async () => ({
      kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: entries.length, knownBytes: 100n, complete: true,
    }),
    subscribeCatalogScanProgress: () => () => undefined,
    subscribePathActivity: () => () => undefined,
    subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
    subscribeProtocolGeneration: () => () => undefined,
    beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
    beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
    ...(options.automaticPhotoPreview === undefined ? {} : { automaticPhotoPreview: options.automaticPhotoPreview }),
    createTransferJob: (output: TransferOutputSession) => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: async () => {
        const target = entries.find(item => item.kind === 'file') ?? entry
        await commitMockOutputFile(output, target)
        return {
          status: 'completed' as const,
          completedFiles: 1n, completedBytes: 16n,
          discoveredFiles: 1n, discoveredBytes: 16n, discoveryComplete: true,
          failedFiles: 0n, failedDirectories: 0n,
        }
      },
    }),
    close: async () => undefined,
  } as unknown as JoinedBrowserShare
}

describe('singleFileDownloadState', () => {
  it('marks the active file as downloading with highest precedence over admission and probing gates', () => {
    const state = singleFileDownloadState('file-a', makeReadiness({
      activeSingleFileId: 'file-a',
      startAdmission: { allowed: false, reason: 'downloading', canReleaseCurrent: false, localChoice: null },
      capabilitiesState: 'probing',
    }))
    expect(state.downloading).toBe(true)
    expect(state.disabledReason).not.toBeNull()
  })

  it('disables non-active files when startAdmission blocks new downloads', () => {
    const state = singleFileDownloadState('file-b', makeReadiness({
      activeSingleFileId: 'file-a',
      startAdmission: { allowed: false, reason: 'downloading', canReleaseCurrent: false, localChoice: null },
      capabilitiesState: 'failed',
    }))
    expect(state.downloading).toBe(false)
    expect(state.disabledReason).not.toBeNull()
  })

  it('disables files while capabilities are probing, but keeps failed capabilities clickable for retry', () => {
    const probing = singleFileDownloadState('file-a', makeReadiness({
      capabilitiesState: 'probing',
    }))
    expect(probing.downloading).toBe(false)
    expect(probing.disabledReason).not.toBeNull()

    const failed = singleFileDownloadState('file-a', makeReadiness({
      capabilitiesState: 'failed',
    }))
    expect(failed).toEqual({ downloading: false, disabledReason: null })
  })

  it('allows immediate single-file downloads when ready and admitted', () => {
    expect(singleFileDownloadState('file-a', makeReadiness())).toEqual({
      downloading: false,
      disabledReason: null,
    })
  })
})

describe('ReceiverController.downloadFile', () => {
  afterEach(() => vi.restoreAllMocks())

  it('guards non-file targets, publishes synchronous failures, clears stale errors on retry, and settles downloads', async () => {
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const { windowPort, downloads } = createMockDownloadWindow()
    const smallFile: Extract<CatalogEntry, { kind: 'file' }> = {
      ...entry, id: identity(2), idText: identityText(2), name: 'notes.txt', expectedSize: 16n,
    }
    const hugeFile: Extract<CatalogEntry, { kind: 'file' }> = {
      ...entry, id: identity(3), idText: identityText(3), name: 'huge.bin', expectedSize: 80n * MIB,
    }
    const subDir: Extract<CatalogEntry, { kind: 'directory' }> = {
      kind: 'directory', id: identity(4), idText: identityText(4), name: 'Photos',
    }
    const joined = makeBrowseShare([smallFile, hugeFile, subDir])
    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitForController(controller, s => s.output.capabilitiesState === 'ready' && s.rows.length === 3)

    expect(controller.downloadFile('missing-id')).toEqual({ kind: 'blocked', reason: 'actionUnavailable' })
    expect(controller.downloadFile(subDir.idText)).toEqual({ kind: 'blocked', reason: 'actionUnavailable' })
    expect(controller.getSnapshot().error).toBeNull()

    const unavailable = controller.downloadFile(hugeFile.idText)
    expect(unavailable.kind).toBe('unavailable')
    expect(controller.getSnapshot().error).toEqual({ kind: 'save-unavailable', reason: 'portable-limit-exceeded' })

    // A fresh valid click clears the prior banner error and completes the transfer.
    expect(controller.downloadFile(smallFile.idText)).toEqual({ kind: 'started' })
    expect(controller.getSnapshot().error).toBeNull()
    await waitForController(controller, s => s.output.activeTaskId === null && s.output.lifecycle?.kind === 'download-started')
    expect(downloads).toHaveLength(1)
    expect(downloads[0]!.fileName).toBe('notes.txt')

    await controller.dispose()
    expect(controller.downloadFile(smallFile.idText)).toEqual({ kind: 'blocked', reason: 'actionUnavailable' })
  })

  it('yields pending automatic photo speculation when starting a single-file download', async () => {
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const { windowPort, downloads } = createMockDownloadWindow()
    let autoSignal: AbortSignal | undefined
    const joined = makeBrowseShare([entry], {
      automaticPhotoPreview: (_target, _conn, signal) => {
        autoSignal = signal
        return new Promise(() => undefined)
      },
    })
    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitForController(controller, s => s.output.capabilitiesState === 'ready' && s.preview.state === 'loading')

    expect(controller.downloadFile(entry.idText)).toEqual({ kind: 'started' })
    expect(autoSignal?.aborted).toBe(true)
    expect(controller.getSnapshot().preview.state).toBe('idle')
    await waitForController(controller, s => s.output.activeTaskId === null && s.output.lifecycle?.kind === 'download-started')
    expect(downloads).toHaveLength(1)

    await controller.dispose()
  })

  it('blocks folder/file pickers on ended or unavailable connections while keeping local photo and preview image saves', async () => {
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const picker = vi.fn(() => new Promise<FileSystemDirectoryHandle>(() => undefined))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker,
    }
    const photoBlob = new Blob([Uint8Array.of(1, 2, 3, 4)], { type: 'image/png' })
    let emitConnection: ((snapshot: ReceiverConnectionSnapshot) => void) | undefined
    let transferStarts = 0

    // 1. Folder share: ended/unavailable blocks tree picker & row downloads, while loaded preview image still saves locally.
    const fileA: Extract<CatalogEntry, { kind: 'file' }> = { ...entry, id: identity(11), idText: identityText(11), name: 'pic.png', expectedSize: 16n }
    const fileB: Extract<CatalogEntry, { kind: 'file' }> = { ...entry, id: identity(12), idText: identityText(12), name: 'doc.pdf', expectedSize: 16n }
    const folderShare = {
      ...makeBrowseShare([fileA, fileB]),
      subscribeConnection: (listener: (value: ReceiverConnectionSnapshot) => void) => {
        emitConnection = listener
        listener({ kind: 'connected' })
        return () => undefined
      },
      preview: async () => ({
        current: { kind: 'image' as const, name: fileA.name, url: 'blob:img', mimeType: 'image/png', width: 10, height: 10 },
        imageBlob: photoBlob,
        seek: async () => { throw new Error('unused') },
        close: async () => undefined,
      }),
      beginDownloadConnectivity: () => { transferStarts += 1; return { routes: [], close: vi.fn() } },
    } as unknown as JoinedBrowserShare

    const folderController = new ReceiverController(
      { join: async () => folderShare } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    folderController.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/folder' })
    await waitForController(folderController, s => s.output.offers?.kind === 'ready' && s.rows.length === 2)
    folderController.previewFile(fileA.idText)
    await waitForController(folderController, s => s.preview.state === 'image')

    emitConnection?.({ kind: 'unavailable', reason: 'protocol-failed' })
    expect(folderController.getSnapshot().startAdmission).toEqual({
      allowed: false, reason: 'connectionUnavailable', canReleaseCurrent: false, localChoice: null,
    })
    folderController.chooseArtifact('fsa:tree', 'default')
    expect(folderController.downloadFile(fileB.idText)).toEqual({ kind: 'blocked', reason: 'connectionUnavailable' })
    expect(picker).not.toHaveBeenCalled()
    expect(transferStarts).toBe(0)
    expect(folderController.downloadPreviewedFile()).toEqual({ kind: 'saved-image' })
    expect(downloads).toHaveLength(1)
    await folderController.dispose()

    // 2. Single-photo share: local portable choice stays enabled after share ends, while FSA/OPFS/redownload stay blocked.
    const photoShare = {
      ...makeBrowseShare([entry]),
      automaticPhotoPreview: async () => ({
        current: { kind: 'image' as const, name: entry.name, url: 'blob:photo', mimeType: 'image/png', width: 20, height: 10 },
        imageBlob: photoBlob,
        seek: async () => { throw new Error('unused') },
        close: async () => undefined,
      }),
      inspectSelection: async () => ({
        kind: 'single-file' as const, file: entry, sourcePath: [entry.name], exactBytes: entry.expectedSize,
      }),
      subscribeConnection: (listener: (value: ReceiverConnectionSnapshot) => void) => {
        emitConnection = listener
        listener({ kind: 'connected' })
        return () => undefined
      },
      beginDownloadConnectivity: () => { transferStarts += 1; return { routes: [], close: vi.fn() } },
    } as unknown as JoinedBrowserShare

    const photoController = new ReceiverController(
      { join: async () => photoShare } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    photoController.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/photo' })
    await waitForController(photoController, s => s.preview.state === 'image' && s.output.offers?.kind === 'ready')

    emitConnection?.({ kind: 'ended', reason: 'share-stopped' })
    const photoSnap = photoController.getSnapshot()
    expect(photoSnap.startAdmission).toEqual({
      allowed: false,
      reason: 'shareEnded',
      canReleaseCurrent: false,
      localChoice: { choiceId: 'portable:file', allowed: true, reason: null },
    })

    const saving = presentSavingActions({
      offers: photoSnap.output.offers,
      blockedBy: photoSnap.startAdmission.reason,
      localChoice: photoSnap.startAdmission.localChoice,
    })
    expect(saving.disabledReason).toBeNull()
    expect(saving.primary?.choice.choiceId).toBe('portable:file')
    expect(saving.primary?.disabledReason).toBeNull()
    const allChoices = saving.alternatives.flatMap(group => [group.primary, ...group.alternatives])
    expect(allChoices.find(c => c.choice.choiceId === 'opfs:file')?.disabledReason).not.toBeNull()
    expect(allChoices.find(c => c.choice.choiceId === 'fsa:file')?.disabledReason).not.toBeNull()

    // Retry confirmation is suppressed when the connection is already terminal.
    expect(presentSavingActions({
      offers: { kind: 'retry-required', epoch: 1n, selectionKey: 'k' },
      blockedBy: 'shareEnded',
    }).retry).toBe(false)

    // Remote options and explicit redownload stay blocked without opening pickers or transfers.
    photoController.chooseArtifact('fsa:file', 'default')
    photoController.chooseArtifact('opfs:file', 'default')
    photoController.chooseArtifact('portable:file', 'redownload')
    expect(picker).not.toHaveBeenCalled()
    expect(transferStarts).toBe(0)

    // Default portable save still delivers the in-memory photo blob locally.
    photoController.chooseArtifact('portable:file', 'default')
    expect(downloads).toHaveLength(2)
    expect(transferStarts).toBe(0)

    // Closing the preview drops the in-memory blob, so portable default save is now blocked too.
    photoController.cancelPreview()
    expect(photoController.getSnapshot().startAdmission.localChoice).toBeNull()
    photoController.chooseArtifact('portable:file', 'default')
    expect(downloads).toHaveLength(2)
    expect(transferStarts).toBe(0)

    await photoController.dispose()
  })
})
