import { describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator, type ReceiveWindowPort } from '../../src/application/receive/coordinator'
import {
  BeforeUnloadGuard,
  type BeforeUnloadEventLike,
  type BeforeUnloadEventTarget,
} from '../../src/application/receive/unload-guard'
import { SelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { TransferJobResult } from '../../src/transfer/job'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { commitMockOutputFile } from './receive-coordinator-fixture'

function createFakeTarget() {
  const listeners = new Set<(event: BeforeUnloadEventLike) => void>()
  const addEventListener = vi.fn((_type: 'beforeunload', listener: (event: BeforeUnloadEventLike) => void) => {
    listeners.add(listener)
  })
  const removeEventListener = vi.fn((_type: 'beforeunload', listener: (event: BeforeUnloadEventLike) => void) => {
    listeners.delete(listener)
  })
  const target: BeforeUnloadEventTarget = { addEventListener, removeEventListener }
  return { target, listeners, addEventListener, removeEventListener }
}

describe('BeforeUnloadGuard', () => {
  it('stays unattached when idle and dynamically toggles the listener across cycles', () => {
    const { target, listeners, addEventListener, removeEventListener } = createFakeTarget()
    const guard = new BeforeUnloadGuard(target)

    expect(guard.armed).toBe(false)
    expect(addEventListener).not.toHaveBeenCalled()

    guard.setArmed(true)
    guard.setArmed(true)
    expect(guard.armed).toBe(true)
    expect(addEventListener).toHaveBeenCalledTimes(1)
    expect(listeners.size).toBe(1)

    const event: BeforeUnloadEventLike = { preventDefault: vi.fn(), returnValue: undefined }
    for (const listener of listeners) listener(event)
    expect(event.preventDefault).toHaveBeenCalledTimes(1)
    expect(event.returnValue).toBe('')

    guard.setArmed(false)
    guard.setArmed(false)
    expect(guard.armed).toBe(false)
    expect(removeEventListener).toHaveBeenCalledTimes(1)
    expect(listeners.size).toBe(0)

    // Subsequent downloads in the same document re-arm and disarm cleanly.
    guard.setArmed(true)
    expect(addEventListener).toHaveBeenCalledTimes(2)
    expect(listeners.size).toBe(1)
    guard.setArmed(false)
    expect(removeEventListener).toHaveBeenCalledTimes(2)
    expect(listeners.size).toBe(0)
  })

  it('detaches on dispose and ignores future arm attempts', () => {
    const { target, listeners, addEventListener, removeEventListener } = createFakeTarget()
    const guard = new BeforeUnloadGuard(target)

    guard.setArmed(true)
    expect(listeners.size).toBe(1)

    guard.dispose()
    guard.dispose()
    expect(guard.armed).toBe(false)
    expect(removeEventListener).toHaveBeenCalledTimes(1)
    expect(listeners.size).toBe(0)

    guard.setArmed(true)
    expect(guard.armed).toBe(false)
    expect(addEventListener).toHaveBeenCalledTimes(1)
    expect(listeners.size).toBe(0)
  })

  it('tolerates targets without DOM event listener methods', () => {
    const guard = new BeforeUnloadGuard(undefined)
    guard.setArmed(true)
    expect(guard.armed).toBe(false)
    guard.setArmed(false)
    guard.dispose()
    expect(guard.armed).toBe(false)
  })

  it('arms beforeunload only while ReceiveCoordinator has an active download session', async () => {
    const fileId = new Uint8Array(16).fill(9)
    const fileIdText = encodeBase64Url(fileId)
    const { target, listeners } = createFakeTarget()
    let resolvePicker!: (handle: FileSystemDirectoryHandle) => void
    const windowPort: ReceiveWindowPort = {
      URL: { createObjectURL: vi.fn(() => 'blob:guard'), revokeObjectURL: vi.fn() },
      document: {
        createElement: vi.fn(() => ({ click: vi.fn(), remove: vi.fn(), style: {} })),
        body: { append: vi.fn() },
      } as unknown as Document,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => new Promise<FileSystemDirectoryHandle>(resolve => { resolvePicker = resolve })),
      addEventListener: target.addEventListener,
      removeEventListener: target.removeEventListener,
    }

    let resolveJob!: (value: TransferJobResult) => void
    let rejectJob!: (error: unknown) => void
    let jobCalls = 0
    const joined = {
      descriptor: { shareInstanceId: 'share-guard', syntheticRootId: 'root-guard' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file: { kind: 'file' as const, id: fileId, idText: fileIdText, name: 'clip.mp4', expectedSize: 32n },
        sourcePath: ['clip.mp4'],
        exactBytes: 32n,
      }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: (output: TransferOutputSession) => {
        jobCalls += 1
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          detach: vi.fn(),
          run: async () => {
            const result = await new Promise<TransferJobResult>((resolve, reject) => {
              resolveJob = resolve
              rejectJob = reject
            })
            if (result.status === 'completed') {
              await commitMockOutputFile(output, { idText: fileIdText, name: 'clip.mp4' })
            }
            return result
          },
        }
      },
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError: vi.fn(),
    })
    coordinator.refreshOffers(joined)
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))
    expect(listeners.size).toBe(0)

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    const fsaChoice = [offers.primary, ...offers.alternatives].find(choice => choice.backend === 'fsa')
    expect(fsaChoice).toBeDefined()
    expect(coordinator.startDownload(fsaChoice!.choiceId)).toBe(true)
    // Waiting on the OS folder picker has no active transfer yet, so beforeunload stays unattached.
    expect(coordinator.getSnapshot().activation?.kind).toBe('waiting-picker')
    expect(listeners.size).toBe(0)
    coordinator.cancelActivation()
    resolvePicker({} as FileSystemDirectoryHandle)

    // 1. Active download arms beforeunload and disarms before publishing settled completion.
    const portableChoice = [offers.primary, ...offers.alternatives].find(choice => choice.backend === 'portable')
    expect(portableChoice).toBeDefined()
    let armedWhenSettledObserved: boolean | undefined
    const unsubscribe = coordinator.subscribe(() => {
      if (coordinator.getSnapshot().lifecycle?.kind === 'download-started') {
        armedWhenSettledObserved = listeners.size > 0
      }
    })
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(1))
    expect(listeners.size).toBe(1)

    const unloadEvent: BeforeUnloadEventLike = { preventDefault: vi.fn(), returnValue: undefined }
    for (const listener of listeners) listener(unloadEvent)
    expect(unloadEvent.preventDefault).toHaveBeenCalledTimes(1)
    expect(unloadEvent.returnValue).toBe('')

    resolveJob({
      status: 'completed',
      completedFiles: 1n,
      completedBytes: 32n,
      discoveredFiles: 1n,
      discoveredBytes: 32n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('download-started'))
    expect(armedWhenSettledObserved).toBe(false)
    expect(listeners.size).toBe(0)
    unsubscribe()

    // 2. Stopping stays guarded while draining and disarms once settled.
    coordinator.startNewDownload()
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(2))
    expect(listeners.size).toBe(1)
    coordinator.performActiveAction('stop')
    expect(coordinator.getSnapshot().settling).toBe('stop')
    expect(listeners.size).toBe(1)
    resolveJob({
      status: 'stopped',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 1n,
      discoveredBytes: 32n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('failed'))
    expect(listeners.size).toBe(0)

    // 3. Failing transfer also disarms cleanly.
    coordinator.startNewDownload()
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(3))
    expect(listeners.size).toBe(1)
    rejectJob(new Error('network lost'))
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('failed'))
    expect(listeners.size).toBe(0)

    // 4. Disposing while a download is mid-flight immediately removes beforeunload.
    coordinator.startNewDownload()
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(4))
    expect(listeners.size).toBe(1)
    const disposed = coordinator.dispose()
    expect(listeners.size).toBe(0)
    resolveJob({
      status: 'detached', completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 32n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await disposed
  })
})
