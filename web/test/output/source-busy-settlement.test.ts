import { describe, expect, it } from 'vitest'
import { retainedSnapshot } from '../../src/application/receive/retained'
import { byteRange } from '../../src/content/geometry'
import { REVISION_CODE_BUSY } from '../../src/content/flow'
import { RemoteRevisionError } from '../../src/content/session-services'
import { openNewFSASession } from '../../src/output/backends/fsa'
import { openNewOPFSSession, reopenOPFSSession } from '../../src/output/backends/opfs'
import { openPortableSession } from '../../src/output/backends/portable'
import type { ReceiveStore } from '../../src/output/store/database'
import { createReceivedProtocolError } from '../../src/session/diagnostics'
import { createProtocolOperationIdentity, createProtocolSessionIdentity } from '../../src/session/identities'
import type { SettledTransferJobResult } from '../../src/transfer/job'
import { initialReceiveState } from '../../src/transfer/state'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { FakeDirectoryHandle, MemoryReceiveStore as FSAMemoryStore } from './fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore as OPFSMemoryStore } from './opfs-fixture'

const SPEC = Object.freeze({ shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] })

function busyProtocolError(): RemoteRevisionError {
  return new RemoteRevisionError(createReceivedProtocolError({
    requestKind: 'open_revisions',
    correlation: Object.freeze({
      protocolSessionId: createProtocolSessionIdentity(new Uint8Array(16).fill(1)),
      protocolOperationId: createProtocolOperationIdentity(1n),
    }),
    content: { scope: 'revision', code: REVISION_CODE_BUSY, retryable: false },
  }))
}

function singleBusyResult(): SettledTransferJobResult {
  return {
    status: 'source-busy',
    completedFiles: 0n,
    completedBytes: 0n,
    discoveredFiles: 1n,
    discoveredBytes: 10n,
    discoveryComplete: true,
    failedFiles: 1n,
    failedDirectories: 0n,
    error: busyProtocolError(),
    sourceRevisionFailures: {
      shareInstance: 's1',
      count: 1n,
      files: [{ entryId: 'f1', path: ['budget.xlsx'], sourcePath: ['budget.xlsx'], reason: 'source-busy' }],
    },
    contentWarning: {
      selectedFileCount: 1n,
      completedFileCount: 0n,
      missingFiles: [{ path: ['budget.xlsx'], reason: 'source-busy' }],
    },
  }
}

describe('source-busy and failed contentWarning settlement', () => {
  it('settles single-file source-busy as a first-class failed reason across FSA, OPFS, and Portable without exposing 0x3009', async () => {
    const result = singleBusyResult()

    const fsaStore = new FSAMemoryStore()
    const fsaParent = new FakeDirectoryHandle('Downloads', fsaStore.events)
    const fsaSession = await openNewFSASession({
      task: {
        taskId: 'fsa-busy',
        selection: SPEC,
        selectionKey: 'sel-busy',
        artifact: Object.freeze({ kind: 'directory-tree', name: 'budget.xlsx', singleFileId: 'f1' }),
        lifecycle: initialReceiveState('fsa-busy'),
      },
      parentHandle: fsaParent as unknown as FileSystemDirectoryHandle,
      store: fsaStore as unknown as ReceiveStore,
    })
    const fsaSettled = await fsaSession.settle(result)
    await fsaSession.close()

    const opfsStore = new OPFSMemoryStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort } = createMockDownloadWindow()
    const opfsSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-busy',
        selection: SPEC,
        selectionKey: 'sel-busy',
        artifact: Object.freeze({
          kind: 'original-file',
          name: 'budget.xlsx',
          singleFileId: 'f1',
          singleFilePath: ['budget.xlsx'],
          singleFileSize: 10n,
        }),
        lifecycle: initialReceiveState('opfs-busy'),
      },
      store: opfsStore as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const opfsSettled = await opfsSession.settle(result)
    await opfsSession.close()

    const portableStore = new OPFSMemoryStore()
    const portableSession = openPortableSession({
      task: {
        taskId: 'portable-busy',
        selection: SPEC,
        selectionKey: 'sel-busy',
        artifact: Object.freeze({ kind: 'original-file', name: 'budget.xlsx', singleFileId: 'f1' }),
        lifecycle: initialReceiveState('portable-busy'),
      },
      windowPort,
      store: portableStore as unknown as ReceiveStore,
    })
    const portableSettled = await portableSession.settle(result)
    await portableSession.close()

    for (const record of [fsaSettled, opfsSettled, portableSettled]) {
      expect(record.lifecycle.kind).toBe('failed')
      if (record.lifecycle.kind !== 'failed') continue
      expect(record.lifecycle.reason).toBe('source-busy')
      expect(record.lifecycle.detail).toBeUndefined()
      expect(record.lifecycle.contentWarning).toEqual(result.contentWarning)

      const presented = presentTask(retainedTaskFacts(retainedSnapshot(record, false)))
      expect(presented.stage).toBe('failed')
      expect(presented.transition.reason).toBe('source-busy')
      expect(presented.completeness).toBe('incomplete')
      expect(presented.attention).toBe(false)
      expect(presented.details).toEqual([])
    }
  })

  it('preserves per-file contentWarning on mixed failed tasks without leaking raw revision error codes, while keeping unknown error details', async () => {
    const store = new OPFSMemoryStore()
    const { windowPort } = createMockDownloadWindow()

    const mixedSession = openPortableSession({
      task: {
        taskId: 'portable-mixed',
        selection: SPEC,
        selectionKey: 'sel-mixed',
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Docs.zip' }),
        lifecycle: initialReceiveState('portable-mixed'),
      },
      windowPort,
      store: store as unknown as ReceiveStore,
    })
    const mixedSettled = await mixedSession.settle({
      status: 'failed',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 2n,
      discoveredBytes: 20n,
      discoveryComplete: true,
      failedFiles: 2n,
      failedDirectories: 0n,
      error: busyProtocolError(),
      contentWarning: {
        selectedFileCount: 2n,
        completedFileCount: 0n,
        missingFiles: [
          { path: ['budget.xlsx'], reason: 'source-busy' },
          { path: ['locked.txt'], reason: 'source-unavailable' },
        ],
      },
    })
    await mixedSession.close()

    const mixedPresented = presentTask(retainedTaskFacts(retainedSnapshot(mixedSettled, false)))
    expect(mixedPresented.completeness).toBe('incomplete')
    expect(mixedPresented.details.some(line => line.includes('budget.xlsx'))).toBe(true)
    expect(mixedPresented.details.some(line => line.includes('locked.txt'))).toBe(true)
    expect(mixedPresented.details.some(line => line.includes('0x3009'))).toBe(false)

    const unknownSession = openPortableSession({
      task: {
        taskId: 'portable-unknown',
        selection: SPEC,
        selectionKey: 'sel-unknown',
        artifact: Object.freeze({ kind: 'original-file', name: 'raw.bin', singleFileId: 'f1' }),
        lifecycle: initialReceiveState('portable-unknown'),
      },
      windowPort,
      store: store as unknown as ReceiveStore,
    })
    const unknownSettled = await unknownSession.settle({
      status: 'failed',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 1n,
      discoveredBytes: 10n,
      discoveryComplete: true,
      failedFiles: 1n,
      failedDirectories: 0n,
      error: new Error('unexpected transport reset'),
      contentWarning: {
        selectedFileCount: 1n,
        completedFileCount: 0n,
        missingFiles: [{ path: ['raw.bin'], reason: 'file-failed' }],
      },
    })
    await unknownSession.close()

    const unknownPresented = presentTask(retainedTaskFacts(retainedSnapshot(unknownSettled, false)))
    expect(unknownPresented.details.some(line => line.includes('unexpected transport reset'))).toBe(true)
  })

  it('keeps previously completed OPFS ZIP members partial when a resumed attempt hits source-busy', async () => {
    const store = new OPFSMemoryStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort } = createMockDownloadWindow()
    const payload = new TextEncoder().encode('saved-member')

    const initial = await openNewOPFSSession({
      task: {
        taskId: 'opfs-resume-busy',
        selection: SPEC,
        selectionKey: 'sel-resume-busy',
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Bundle.zip' }),
        lifecycle: initialReceiveState('opfs-resume-busy'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const writer = await initial.transferOutput.openFile({
      fileId: 'fa', path: ['a.txt'], sourcePath: ['a.txt'], revision: 'r1', exactSize: BigInt(payload.byteLength),
    }, new AbortController().signal)
    await writer.writeAt(0n, payload)
    await writer.commit([byteRange(0n, BigInt(payload.byteLength))])
    const paused = await initial.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 1n,
      completedBytes: BigInt(payload.byteLength),
      discoveredFiles: 2n,
      discoveredBytes: 100n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await initial.close()

    const reopened = await reopenOPFSSession({
      task: paused,
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const partial = await reopened.settle(singleBusyResult())
    await reopened.close()

    expect(partial.lifecycle.kind).toBe('partial')
    if (partial.lifecycle.kind !== 'partial') throw new Error('expected partial lifecycle')
    expect(partial.lifecycle.savedFiles).toBe(1n)
    expect(partial.lifecycle.reason).toBe('failures')
    expect(partial.lifecycle.contentWarning?.missingFiles).toEqual([{ path: ['budget.xlsx'], reason: 'source-busy' }])
  })
})
