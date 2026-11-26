import { assert, describe, expect, it } from 'vitest'
import { frozenSelectionPolicy, SelectionPolicy } from '../../src/catalog/selection'
import { byteRange } from '../../src/content/geometry'
import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import { MISSING_COMMITTED_FILE_MESSAGE } from '../../src/output/backends/contract'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { nextFSALifecycle } from '../../src/output/backends/fsa-settlement'
import { openPortableSession } from '../../src/output/backends/portable'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { ReceiveStore } from '../../src/output/store/database'
import { summarizeStoredTaskFiles } from '../../src/output/store/types'
import { TransferJob, type SettledTransferJobResult, type TransferJobFacts } from '../../src/transfer/job'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'
import { FakeDirectoryHandle, MemoryReceiveStore } from './fsa-fixture'
import { id, makeShareFixture } from '../transfer/share-fixture'

const SIGNAL = new AbortController().signal
const SPEC = Object.freeze({ shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] })

function settled(
  status: Exclude<SettledTransferJobResult['status'], 'partial'>,
  overrides: Partial<TransferJobFacts> = {},
): SettledTransferJobResult {
  return {
    status,
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 0n,
    discoveredBytes: 0n,
    discoveryComplete: true,
    failedFiles: 0n,
    failedDirectories: 0n,
    ...overrides,
  }
}

function createMockDownloadWindow(): {
  readonly windowPort: BrowserDownloadWindow
  readonly blobs: Blob[]
} {
  const blobs: Blob[] = []
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        if (blob instanceof Blob) blobs.push(blob)
        return `blob:mock-${blobs.length}`
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => undefined,
        remove: () => undefined,
      }) as unknown as HTMLAnchorElement,
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Pick<Document, 'createElement' | 'body'>,
    setTimeout: (() => 0) as unknown as typeof setTimeout,
    clearTimeout: () => undefined,
  }
  return { windowPort, blobs }
}

describe('single-file settlement invariants across Portable and FSA', () => {
  it('refuses to trigger a 0-byte portable download when a single-file task settles completed without committing its file', async () => {
    const { windowPort, blobs } = createMockDownloadWindow()
    const traces: OutputTraceEvent[] = []
    const session = openPortableSession({
      task: {
        taskId: 'portable-missing-commit',
        selection: SPEC,
        selectionKey: 'sel-portable-missing-commit',
        artifact: { kind: 'original-file', name: 'report.pdf', singleFileId: 'f1' },
        lifecycle: initialReceiveState('portable-missing-commit'),
      },
      windowPort,
      diagnostics: {
        backend: 'portable',
        trace: { current: event => traces.push(event) },
      },
    })

    const record = await session.settle(settled('completed'))
    await session.close()

    expect(blobs).toHaveLength(0)
    expect(record.lifecycle).toMatchObject({
      kind: 'failed',
      reason: 'error',
      detail: MISSING_COMMITTED_FILE_MESSAGE,
    })
    expect(traces).toContainEqual(expect.objectContaining({
      eventName: 'settlement',
      payload: expect.objectContaining({
        backend: 'portable',
        task_id: 'portable-missing-commit',
        transition: 'failed',
        reason: 'error',
        missing_committed_file: true,
      }),
    }))
  })

  it('settles Portable single-file as source-changed without download when target disappears before discovery, while still delivering real 0-byte files', async () => {
    // 1. Missing explicit file target -> failed (source-changed), no browser download.
    const missingFixture = makeShareFixture([
      { seed: 10, name: 'other.txt', bytes: new Uint8Array([1, 2, 3]) },
    ])
    const missingSelection = frozenSelectionPolicy(false, [{ kind: 'file', id: id(99), selected: true }])
    const missingSpec = selectionSpecFromPolicy(
      missingFixture.descriptor.shareInstanceId,
      missingFixture.descriptor.syntheticRootId,
      missingSelection,
    )
    const missingWin = createMockDownloadWindow()
    const missingSession = openPortableSession({
      task: {
        taskId: 'portable-vanished',
        selection: missingSpec,
        selectionKey: selectionKey(missingSpec),
        artifact: { kind: 'original-file', name: 'vanished.pdf', singleFileId: 'missing' },
        lifecycle: initialReceiveState('portable-vanished'),
      },
      windowPort: missingWin.windowPort,
    })
    const missingJobResult = await new TransferJob({
      ...missingFixture,
      selection: missingSelection,
      output: missingSession.transferOutput,
    }).run()
    assert(missingJobResult.status !== 'detached')
    const missingSettled = await missingSession.settle(missingJobResult)
    await missingSession.close()
    expect(missingWin.blobs).toHaveLength(0)
    expect(missingSettled.lifecycle).toMatchObject({ kind: 'failed', reason: 'source-changed' })

    // 2. Genuine 0-byte single file still commits and delivers a 0-byte Blob cleanly.
    const emptyFixture = makeShareFixture([
      { seed: 20, name: '.gitkeep', bytes: new Uint8Array(0) },
    ])
    const emptySelection = new SelectionPolicy(true).snapshot()
    const emptySpec = selectionSpecFromPolicy(
      emptyFixture.descriptor.shareInstanceId,
      emptyFixture.descriptor.syntheticRootId,
      emptySelection,
    )
    const emptyWin = createMockDownloadWindow()
    const emptySession = openPortableSession({
      task: {
        taskId: 'portable-real-zero-byte',
        selection: emptySpec,
        selectionKey: selectionKey(emptySpec),
        artifact: { kind: 'original-file', name: '.gitkeep', singleFileId: 'zero' },
        lifecycle: initialReceiveState('portable-real-zero-byte'),
      },
      windowPort: emptyWin.windowPort,
    })
    const emptyJobResult = await new TransferJob({
      ...emptyFixture,
      selection: emptySelection,
      output: emptySession.transferOutput,
    }).run()
    assert(emptyJobResult.status !== 'detached')
    expect(emptyJobResult.status).toBe('completed')
    const emptySettled = await emptySession.settle(emptyJobResult)
    await emptySession.close()
    expect(emptyWin.blobs).toHaveLength(1)
    expect(emptyWin.blobs[0]!.size).toBe(0)
    expect(emptySettled.lifecycle).toMatchObject({ kind: 'download-started', fileName: '.gitkeep' })
  })

  it('fails FSA single-file settlement and cleans up untouched placeholders or truncated partial files when no file committed', async () => {
    // 1. Pure lifecycle function enforces cleanupPending on single-file completed with 0 stored files.
    const directFailed = nextFSALifecycle(
      initialReceiveState('fsa-single-zero'),
      settled('completed'),
      { singleFile: true, stored: summarizeStoredTaskFiles([]) },
    )
    expect(directFailed).toMatchObject({
      kind: 'failed',
      reason: 'error',
      detail: MISSING_COMMITTED_FILE_MESSAGE,
      cleanupPending: true,
    })

    // Empty folder (singleFile: false) with 0 files still settles as saved cleanly.
    const emptyFolder = nextFSALifecycle(
      initialReceiveState('fsa-empty-folder'),
      settled('completed'),
      { singleFile: false, stored: summarizeStoredTaskFiles([]) },
    )
    expect(emptyFolder).toMatchObject({ kind: 'saved', savedFiles: 0n, savedBytes: 0n })

    // 2. Fresh FSA single-file session settling completed without commit removes its 0-byte placeholder.
    const placeholderMem = new MemoryReceiveStore()
    const placeholderStore = placeholderMem as unknown as ReceiveStore
    const placeholderParent = new FakeDirectoryHandle('Downloads', placeholderMem.events)
    const placeholderSession = await openNewFSASession({
      task: {
        taskId: 'fsa-single-placeholder',
        selection: SPEC,
        selectionKey: 'sel-placeholder',
        artifact: { kind: 'directory-tree', name: 'ghost.bin', singleFileId: 'f1' },
        lifecycle: initialReceiveState('fsa-single-placeholder'),
      },
      parentHandle: placeholderParent as unknown as FileSystemDirectoryHandle,
      store: placeholderStore,
    })
    expect(placeholderParent.children.has('ghost.bin')).toBe(true)
    const placeholderSettled = await placeholderSession.settle(settled('completed'))
    await placeholderSession.close()
    expect(placeholderSettled.lifecycle).toMatchObject({
      kind: 'failed',
      reason: 'error',
      detail: MISSING_COMMITTED_FILE_MESSAGE,
    })
    if (placeholderSettled.lifecycle.kind === 'failed') {
      expect(placeholderSettled.lifecycle.cleanupPending).toBeUndefined()
    }
    expect(placeholderParent.children.has('ghost.bin')).toBe(false)

    // 3. Resumed FSA single-file with prior truncated partial file cleans up disk & IDB when source disappears.
    const resumeMem = new MemoryReceiveStore()
    const resumeStore = resumeMem as unknown as ReceiveStore
    const resumeParent = new FakeDirectoryHandle('Downloads', resumeMem.events)
    const missingFixture = makeShareFixture([
      { seed: 30, name: 'other.bin', bytes: new Uint8Array([1, 2, 3, 4]) },
    ])
    const targetId = id(31)
    const targetSelection = frozenSelectionPolicy(false, [{ kind: 'file', id: targetId, selected: true }])
    const targetSpec = selectionSpecFromPolicy(
      missingFixture.descriptor.shareInstanceId,
      missingFixture.descriptor.syntheticRootId,
      targetSelection,
    )
    const run1 = await openNewFSASession({
      task: {
        taskId: 'fsa-single-truncated',
        selection: targetSpec,
        selectionKey: selectionKey(targetSpec),
        artifact: { kind: 'directory-tree', name: 'video.mp4', singleFileId: targetSpec.rules[0]!.id },
        lifecycle: initialReceiveState('fsa-single-truncated'),
      },
      parentHandle: resumeParent as unknown as FileSystemDirectoryHandle,
      store: resumeStore,
    })
    const partialWriter = await run1.transferOutput.openFile({
      fileId: targetSpec.rules[0]!.id,
      path: ['video.mp4'],
      sourcePath: ['video.mp4'],
      revision: 'r1',
      exactSize: 100n,
    }, SIGNAL)
    await partialWriter.writeAt(0n, Uint8Array.of(9, 8, 7, 6))
    await partialWriter.pause([byteRange(0n, 4n)])
    const paused = await run1.settle(settled('paused', { pauseReason: 'user' }))
    await run1.close()
    expect(resumeParent.children.has('video.mp4')).toBe(true)

    const run2 = await reopenFSASession({ task: paused, store: resumeStore, allowPermissionPrompt: false })
    const jobResult = await new TransferJob({
      ...missingFixture,
      selection: targetSelection,
      output: run2.transferOutput,
    }).run()
    assert(jobResult.status !== 'detached')
    expect(jobResult.status).toBe('source-changed')
    const terminal = await run2.settle(jobResult)
    await run2.close()

    expect(terminal.lifecycle).toMatchObject({ kind: 'failed', reason: 'source-changed' })
    if (terminal.lifecycle.kind === 'failed') {
      expect(terminal.lifecycle.cleanupPending).toBeUndefined()
    }
    expect(resumeParent.children.has('video.mp4')).toBe(false)
    expect(await resumeStore.listTaskFiles('fsa-single-truncated')).toEqual([])
  })

  it('keeps genuine 0-byte single-file FSA downloads saved on disk', async () => {
    const memory = new MemoryReceiveStore()
    const store = memory as unknown as ReceiveStore
    const parent = new FakeDirectoryHandle('Downloads', memory.events)
    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-real-zero-byte',
        selection: SPEC,
        selectionKey: 'sel-real-zero-byte',
        artifact: { kind: 'directory-tree', name: '.gitkeep', singleFileId: 'f0' },
        lifecycle: initialReceiveState('fsa-real-zero-byte'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store,
    })

    const writer = await session.transferOutput.openFile({
      fileId: 'f0',
      path: ['.gitkeep'],
      sourcePath: ['.gitkeep'],
      revision: 'r0',
      exactSize: 0n,
    }, SIGNAL)
    await writer.commit([])
    const record = await session.settle(settled('completed', { completedFiles: 1n, discoveredFiles: 1n }))
    await session.close()

    expect(record.lifecycle).toMatchObject({ kind: 'saved', savedFiles: 1n, savedBytes: 0n })
    expect(parent.children.has('.gitkeep')).toBe(true)
  })
})
