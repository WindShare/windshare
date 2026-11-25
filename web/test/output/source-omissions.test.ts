import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { assert, describe, expect, it } from 'vitest'
import {
  executeRetainedTaskAction,
  loadRetainedTasks,
  resolveWorkspaceRetainedMatch,
} from '../../src/application/receive/retained'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { openNewFSASession, reopenFSASession } from '../../src/output/backends/fsa'
import { openNewOPFSSession } from '../../src/output/backends/opfs'
import { openPortableSession } from '../../src/output/backends/portable'
import type { ReceiveStore } from '../../src/output/store/database'
import { TransferJob } from '../../src/transfer/job'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'
import { EMPTY_PROGRESS } from '../../src/ui/model'
import { EMPTY_OUTPUT_PRESENTATION } from '../../src/ui/output'
import { activeTaskFacts, presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { FakeDirectoryHandle, MemoryReceiveStore as FSAMemoryStore } from './fsa-fixture'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore as OPFSMemoryStore } from './opfs-fixture'

function fixedId(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

async function readZipFiles(blob: Blob): Promise<Map<string, Uint8Array>> {
  const reader = new ZipReader(new BlobReader(blob))
  try {
    const entries = await reader.getEntries()
    const files = new Map<string, Uint8Array>()
    for (const entry of entries) {
      if (!entry.directory && 'getData' in entry) {
        files.set(entry.filename, await (entry as FileEntry).getData(new Uint8ArrayWriter()))
      }
    }
    return files
  } finally {
    await reader.close()
  }
}

function makeOmittedTreeShare() {
  const shareInstance = fixedId(1)
  const rootId = fixedId(2)
  const parentId = fixedId(10)
  const subAId = fixedId(11)
  const subBId = fixedId(12)
  const fileAId = fixedId(21)
  const fileBId = fixedId(22)

  const payloadA = new TextEncoder().encode('alpha-bytes')
  const payloadB = new TextEncoder().encode('beta-bytes')

  const parentEntry: Extract<CatalogEntry, { kind: 'directory' }> = Object.freeze({
    kind: 'directory', id: parentId, idText: encodeBase64Url(parentId), name: 'Project',
  })
  const subAEntry: Extract<CatalogEntry, { kind: 'directory' }> = Object.freeze({
    kind: 'directory', id: subAId, idText: encodeBase64Url(subAId), name: 'SubA',
  })
  const subBEntry: Extract<CatalogEntry, { kind: 'directory' }> = Object.freeze({
    kind: 'directory', id: subBId, idText: encodeBase64Url(subBId), name: 'SubB',
  })
  const fileAEntry: Extract<CatalogEntry, { kind: 'file' }> = Object.freeze({
    kind: 'file', id: fileAId, idText: encodeBase64Url(fileAId), name: 'a.txt', expectedSize: BigInt(payloadA.byteLength),
  })
  const fileBEntry: Extract<CatalogEntry, { kind: 'file' }> = Object.freeze({
    kind: 'file', id: fileBId, idText: encodeBase64Url(fileBId), name: 'b.txt', expectedSize: BigInt(payloadB.byteLength),
  })

  const dirs = new Map<string, { rawId: Uint8Array<ArrayBuffer>; entries: readonly CatalogEntry[]; omittedCount: bigint }>([
    [encodeBase64Url(rootId), { rawId: rootId, entries: [parentEntry], omittedCount: 0n }],
    [parentEntry.idText, { rawId: parentId, entries: [subAEntry, subBEntry], omittedCount: 0n }],
    [subAEntry.idText, { rawId: subAId, entries: [fileAEntry], omittedCount: 2n }],
    [subBEntry.idText, { rawId: subBId, entries: [fileBEntry], omittedCount: 1n }],
  ])

  const committedById = new Map<string, { committed: CommittedDirectory; page: CatalogPage }>()
  for (const [idText, spec] of dirs) {
    const generation = fixedId(3)
    const committed: CommittedDirectory = Object.freeze({
      directoryId: spec.rawId,
      directoryIdText: idText,
      generation,
      generationText: encodeBase64Url(generation),
      pageCount: 1,
      entryCount: spec.entries.length,
      omittedCount: spec.omittedCount,
      terminalCommitment: new Uint8Array(32),
    })
    const page: CatalogPage = Object.freeze({
      shareInstance,
      directoryId: spec.rawId,
      directoryIdText: idText,
      generation,
      generationText: committed.generationText,
      pageIndex: 0,
      terminal: true,
      previousCommitment: new Uint8Array(32),
      entries: Object.freeze([...spec.entries]),
      omittedCount: spec.omittedCount,
      objectCommitment: new Uint8Array(32),
      encodedBytes: 64,
    })
    committedById.set(idText, { committed, page })
  }

  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: encodeBase64Url(rootId),
  }) as unknown as ShareDescriptor

  const catalog = Object.freeze({
    loadDirectory: async (dirId: Uint8Array) => committedById.get(encodeBase64Url(dirId))!.committed,
    page: async (committed: CommittedDirectory) => committedById.get(committed.directoryIdText)!.page,
  }) as unknown as CatalogClient

  const payloads = new Map<string, Uint8Array<ArrayBuffer>>([
    [fileAEntry.idText, payloadA],
    [fileBEntry.idText, payloadB],
  ])
  const revisions: RevisionReader = {
    open: async fileId => {
      const key = encodeBase64Url(fileId)
      const data = payloads.get(key)!
      const rev = fixedId(50)
      return Object.freeze({
        descriptor: Object.freeze({
          shareInstance,
          shareInstanceId: descriptor.shareInstanceId,
          fileId,
          fileIdText: key,
          fileRevision: rev,
          fileRevisionText: encodeBase64Url(rev),
          exactSize: BigInt(data.byteLength),
          geometry: new FileGeometry(BigInt(data.byteLength), 32n),
        }) as FileRevisionDescriptor,
        leaseId: fixedId(90),
        release: async () => undefined,
      })
    },
  }
  const broker: BlockRangeReader = {
    readRange: async function* (revDesc, _leaseId, span) {
      const data = payloads.get(revDesc.fileIdText)!
      yield Object.freeze({ offset: span.start, data: data.subarray(Number(span.start), Number(span.end)) })
    },
  }

  return {
    descriptor, catalog, revisions, broker,
    parentEntry, subAEntry, subBEntry, fileAEntry, fileBEntry, payloadA, payloadB,
  }
}

describe('source omission propagation across transfer, backends, and retained history', () => {
  it('aggregates omitted entries for selected subtrees while excluding unselected siblings and single-file downloads', async () => {
    const share = makeOmittedTreeShare()
    const makeNoopOutput = () => ({
      sessionId: 'noop',
      capabilities: { maxConcurrentFiles: 2, resumable: false, partialOutput: false },
      openFile: async () => ({
        completedRanges: [],
        writeAt: async () => undefined,
        checkpoint: async () => 'skipped' as const,
        commit: async () => undefined,
        pause: async () => undefined,
        abort: async () => undefined,
      }),
    })

    // Recursive whole-share download sums both SubA (2) and SubB (1).
    let lastProgressOmitted = 0n
    const whole = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection: new SelectionPolicy(true).snapshot(),
      revisions: share.revisions,
      broker: share.broker,
      output: makeNoopOutput(),
      rootPath: ['Project'],
      onProgress: progress => { lastProgressOmitted = progress.sourceOmittedEntries },
    }).run()
    expect(whole.status).toBe('completed')
    expect(whole.sourceOmittedEntries).toBe(3n)
    expect(lastProgressOmitted).toBe(3n)

    // Excluding SubB drops SubB's omitted count.
    const onlySubA = new SelectionPolicy(false)
    onlySubA.set(share.subAEntry, [share.descriptor.syntheticRootId, share.parentEntry.idText], true)
    const subAResult = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection: onlySubA.snapshot(),
      revisions: share.revisions,
      broker: share.broker,
      output: makeNoopOutput(),
      rootPath: ['Project', 'SubA'],
    }).run()
    expect(subAResult.status).toBe('completed')
    expect(subAResult.sourceOmittedEntries).toBe(2n)

    // Single-file download inside SubA (which has omitted siblings) must not report source omissions.
    const singleFile = new SelectionPolicy(false)
    singleFile.set(
      share.fileAEntry,
      [share.descriptor.syntheticRootId, share.parentEntry.idText, share.subAEntry.idText],
      true,
    )
    const singleResult = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection: singleFile.snapshot(),
      revisions: share.revisions,
      broker: share.broker,
      output: makeNoopOutput(),
      rootPath: ['Project', 'SubA'],
    }).run()
    expect(singleResult.status).toBe('completed')
    expect(singleResult.sourceOmittedEntries).toBeUndefined()
  })

  it('completes and persists omitted counts across Portable ZIP, OPFS ZIP (preserving local reuse), and FSA pause/resume', async () => {
    const share = makeOmittedTreeShare()
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(share.descriptor.shareInstanceId, share.descriptor.syntheticRootId, selection)
    const key = selectionKey(spec)

    // 1. Portable ZIP still hands off a valid archive and persists sourceOmittedEntries in history.
    const portableStore = new OPFSMemoryStore()
    const { windowPort: portableWin, downloads: portableDownloads } = createMockDownloadWindow()
    const portableSession = openPortableSession({
      task: {
        taskId: 'portable-omitted',
        selection: spec,
        selectionKey: key,
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Project.zip', rootPath: ['Project'] }),
        lifecycle: initialReceiveState('portable-omitted'),
      },
      windowPort: portableWin,
      store: portableStore as unknown as ReceiveStore,
    })
    const portableResult = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection,
      revisions: share.revisions,
      broker: share.broker,
      output: portableSession.transferOutput,
      rootPath: ['Project'],
    }).run()
    assert(portableResult.status === 'completed')
    const portableSettled = await portableSession.settle(portableResult)
    await portableSession.close()

    expect(portableSettled.lifecycle.kind).toBe('download-started')
    expect(portableSettled.sourceOmittedEntries).toBe(3n)
    expect(portableDownloads).toHaveLength(1)
    const portableEntries = await readZipFiles(portableDownloads[0]!.blob)
    expect(portableEntries.get('Project/SubA/a.txt')).toEqual(share.payloadA)
    expect(portableEntries.get('Project/SubB/b.txt')).toEqual(share.payloadB)

    const [portableRetained] = await loadRetainedTasks({
      store: portableStore as unknown as ReceiveStore,
      windowPort: portableWin as unknown as BrowserOutputWindow,
    })
    expect(portableRetained?.sourceOmittedEntries).toBe(3n)

    // 2. OPFS ZIP seals, auto-downloads, preserves sourceOmittedEntries across re-export and reload,
    //    and remains eligible as a local-complete copy so reopening the share avoids redundant redownload.
    const opfsStore = new OPFSMemoryStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort: opfsWin, downloads: opfsDownloads } = createMockDownloadWindow()
    const opfsSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-omitted',
        selection: spec,
        selectionKey: key,
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Project.zip', rootPath: ['Project'] }),
        lifecycle: initialReceiveState('opfs-omitted'),
      },
      store: opfsStore as unknown as ReceiveStore,
      env,
      windowPort: opfsWin,
    })
    const opfsResult = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection,
      revisions: share.revisions,
      broker: share.broker,
      output: opfsSession.transferOutput,
      rootPath: ['Project'],
    }).run()
    assert(opfsResult.status === 'completed')
    const opfsSettled = await opfsSession.settle(opfsResult)
    await opfsSession.close()

    expect(opfsSettled.lifecycle.kind).toBe('download-started')
    expect(opfsSettled.sourceOmittedEntries).toBe(3n)
    expect(opfsDownloads).toHaveLength(1)

    const retainedEnv = {
      store: opfsStore as unknown as ReceiveStore,
      windowPort: opfsWin as unknown as BrowserOutputWindow,
      opfsEnv: env,
    }
    const reexported = await executeRetainedTaskAction(retainedEnv, 'opfs-omitted', 'save')
    expect(reexported.updated?.sourceOmittedEntries).toBe(3n)

    const [opfsRetained] = await loadRetainedTasks(retainedEnv)
    expect(opfsRetained?.sourceOmittedEntries).toBe(3n)
    const match = resolveWorkspaceRetainedMatch({
      tasks: opfsRetained === undefined ? [] : [opfsRetained],
      shareInstance: share.descriptor.shareInstanceId,
      selectionKey: key,
      admission: {
        currentShareInstance: share.descriptor.shareInstanceId,
        hasActiveDownload: false,
        hasPendingAction: false,
        connectionBlock: null,
      },
    })
    expect(match.kind).toBe('local-complete')

    // 3. FSA keeps the larger prior omitted bound across an earlier-interrupted resume and settles cleanly.
    const fsaStore = new FSAMemoryStore()
    const parentHandle = new FakeDirectoryHandle('Downloads', fsaStore.events)
    const fsaSession = await openNewFSASession({
      task: {
        taskId: 'fsa-omitted',
        selection: spec,
        selectionKey: key,
        artifact: Object.freeze({ kind: 'directory-tree', name: 'Project', rootPath: ['Project'] }),
        lifecycle: initialReceiveState('fsa-omitted'),
      },
      parentHandle: parentHandle as unknown as FileSystemDirectoryHandle,
      store: fsaStore as unknown as ReceiveStore,
    })
    const firstPause = await fsaSession.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 1n,
      completedBytes: BigInt(share.payloadA.byteLength),
      discoveredFiles: 1n,
      discoveredBytes: BigInt(share.payloadA.byteLength),
      discoveryComplete: false,
      failedFiles: 0n,
      failedDirectories: 0n,
      sourceOmittedEntries: 2n,
    })
    await fsaSession.close()
    expect(firstPause.sourceOmittedEntries).toBe(2n)

    const reopenedEarly = await reopenFSASession({
      task: firstPause,
      store: fsaStore as unknown as ReceiveStore,
      allowPermissionPrompt: true,
    })
    const earlierPause = await reopenedEarly.settle({
      status: 'paused',
      pauseReason: 'interrupted',
      completedFiles: 1n,
      completedBytes: BigInt(share.payloadA.byteLength),
      discoveredFiles: 1n,
      discoveredBytes: BigInt(share.payloadA.byteLength),
      discoveryComplete: false,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await reopenedEarly.close()
    expect(earlierPause.sourceOmittedEntries).toBe(2n)

    const reopenedFinal = await reopenFSASession({
      task: earlierPause,
      store: fsaStore as unknown as ReceiveStore,
      allowPermissionPrompt: true,
    })
    const finalResult = await new TransferJob({
      descriptor: share.descriptor,
      catalog: share.catalog,
      selection,
      revisions: share.revisions,
      broker: share.broker,
      output: reopenedFinal.transferOutput,
      rootPath: ['Project'],
    }).run()
    assert(finalResult.status === 'completed')
    const fsaSettled = await reopenedFinal.settle(finalResult)
    await reopenedFinal.close()

    expect(fsaSettled.lifecycle.kind).toBe('saved')
    expect(fsaSettled.sourceOmittedEntries).toBe(3n)

    const [fsaRetained] = await loadRetainedTasks({
      store: fsaStore as unknown as ReceiveStore,
      windowPort: opfsWin as unknown as BrowserOutputWindow,
    })
    expect(fsaRetained?.sourceOmittedEntries).toBe(3n)

    // 4. Presentation facts surface warning tone + count without breaking completeness, attention, or live ETA.
    const fsaPresented = presentTask(retainedTaskFacts(fsaRetained!))
    const cleanFsaPresented = presentTask(retainedTaskFacts({ ...fsaRetained!, sourceOmittedEntries: 0n }))
    expect(fsaPresented.stage).toBe('saved')
    expect(fsaPresented.completeness).toBe('complete')
    expect(fsaPresented.attention).toBe(false)
    expect(fsaPresented.tone).toBe('warning')
    expect(fsaPresented.sourceOmittedEntries).toBe(3n)
    expect(fsaPresented.headline).not.toBe(cleanFsaPresented.headline)
    expect(fsaPresented.details.length).toBeGreaterThan(0)
    expect(cleanFsaPresented.details).toHaveLength(0)

    const portablePresented = presentTask(retainedTaskFacts(portableRetained!))
    expect(portablePresented.stage).toBe('handed-to-browser')
    expect(portablePresented.completeness).toBe('complete')
    expect(portablePresented.attention).toBe(false)
    expect(portablePresented.tone).toBe('warning')
    expect(portablePresented.sourceOmittedEntries).toBe(3n)

    const liveDownloading = activeTaskFacts({
      output: {
        ...EMPTY_OUTPUT_PRESENTATION,
        backend: 'fsa',
        lifecycle: { ...initialReceiveState('live-1'), kind: 'downloading', phase: 'receiving' },
      },
      progress: {
        ...EMPTY_PROGRESS,
        transferJobId: 'job-1',
        discovery: 'complete',
        discoveredFiles: 2,
        discoveredBytes: 1_000n,
        materializedBytes: 400n,
        sourceOmittedEntries: 2n,
      },
    })
    const livePresented = presentTask(liveDownloading!)
    expect(livePresented.sourceOmittedEntries).toBe(2n)
    expect(livePresented.tone).toBe('neutral')
    expect(livePresented.progress?.remainingBytes).toBe(600n)
  })
})
