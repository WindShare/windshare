import { afterEach, describe, expect, it, vi } from 'vitest'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import * as outputContract from '../../src/output/backends/contract'
import { ReceiverController } from '../../src/ui/controller'
import {
  singleFileDownloadState,
  type SingleFileDownloadReadiness,
} from '../../src/ui/share-content/single-file-admission'
import { createMockDownloadWindow } from '../output/opfs-fixture'
import { entry, identity, identityText, rootDirectory, waitForController } from './v2-preview-fixture'

const MIB = 1024n * 1024n

function makeReadiness(
  overrides: Partial<SingleFileDownloadReadiness> = {},
): SingleFileDownloadReadiness {
  return {
    startAdmission: { allowed: true, reason: null, canReleaseCurrent: false },
    capabilitiesState: 'ready',
    activeSingleFileId: null,
    singleFilePickerPendingFromBytes: null,
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
    createTransferJob: () => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: async () => ({
        status: 'completed' as const,
        completedFiles: 1n, completedBytes: 16n,
        discoveredFiles: 1n, discoveredBytes: 16n, discoveryComplete: true,
        failedFiles: 0n, failedDirectories: 0n,
      }),
    }),
    close: async () => undefined,
  } as unknown as JoinedBrowserShare
}

describe('singleFileDownloadState', () => {
  it('marks the active file as downloading with highest precedence over admission and probing gates', () => {
    const state = singleFileDownloadState('file-a', 100n * MIB, makeReadiness({
      activeSingleFileId: 'file-a',
      startAdmission: { allowed: false, reason: 'downloading', canReleaseCurrent: false },
      capabilitiesState: 'probing',
      singleFilePickerPendingFromBytes: 64n * MIB + 1n,
    }))
    expect(state.downloading).toBe(true)
    expect(state.disabledReason).not.toBeNull()
  })

  it('disables non-active files when startAdmission blocks new downloads', () => {
    const state = singleFileDownloadState('file-b', 100n, makeReadiness({
      activeSingleFileId: 'file-a',
      startAdmission: { allowed: false, reason: 'downloading', canReleaseCurrent: false },
      capabilitiesState: 'failed',
    }))
    expect(state.downloading).toBe(false)
    expect(state.disabledReason).not.toBeNull()
  })

  it('disables files while capabilities are probing, but keeps failed capabilities clickable for retry', () => {
    const probing = singleFileDownloadState('file-a', 100n, makeReadiness({
      capabilitiesState: 'probing',
    }))
    expect(probing.downloading).toBe(false)
    expect(probing.disabledReason).not.toBeNull()

    // Failed probing stays clickable even if a picker floor is present so clicks retry probing.
    const failed = singleFileDownloadState('file-a', 100n * MIB, makeReadiness({
      capabilitiesState: 'failed',
      singleFilePickerPendingFromBytes: 64n * MIB + 1n,
    }))
    expect(failed).toEqual({ downloading: false, disabledReason: null })
  })

  it('gates only files at or above singleFilePickerPendingFromBytes while share lookup is pending', () => {
    const floor = 64n * MIB + 1n
    const readiness = makeReadiness({ singleFilePickerPendingFromBytes: floor })

    expect(singleFileDownloadState('small', 64n * MIB, readiness)).toEqual({
      downloading: false,
      disabledReason: null,
    })
    expect(singleFileDownloadState('unknown', undefined, readiness)).toEqual({
      downloading: false,
      disabledReason: null,
    })

    const atFloor = singleFileDownloadState('large', floor, readiness)
    expect(atFloor.downloading).toBe(false)
    expect(atFloor.disabledReason).not.toBeNull()
  })

  it('allows immediate single-file downloads when ready and admitted', () => {
    expect(singleFileDownloadState('file-a', 100n, makeReadiness())).toEqual({
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
})
