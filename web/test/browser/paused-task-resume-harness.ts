import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
} from '../../src/output/capability/contract'
import {
  IndexedDbPausedTaskState,
  PausedTaskCapabilityError,
} from '../../src/output/browser/indexeddb-resume-state'
import { IndexedDbBrowserPausedTaskLifecycle } from '../../src/output/browser/paused-task-lifecycle'
import {
  INDEXEDDB_ROOT_CAPABILITY_STORE,
  IndexedDbOutputRepository,
  resolveIndexedDbRootIdentity,
} from '../../src/output/browser/indexeddb-repository'
import {
  acquireFileSystemAccessOutputSession,
} from '../../src/output/file-system-access/session'
import {
  pausedTaskDescriptorKey,
  pausedTaskDescriptorNamespace,
} from '../../src/output/resume/descriptor'
import {
  outputRecordKey,
  type OutputJournalPage,
  type OutputJournalScan,
  type PersistedOutputRecord,
} from '../../src/output/persistence/journal'
import {
  ORIGIN_PRIVATE_EXPORT_COMPLETE,
  openOriginPrivateOutputSession,
  openOriginPrivateStagingNamespace,
  removeOriginPrivateStagingNamespace,
} from '../../src/output/origin-private/session'
import {
  createTransferIntentDraft,
  createTransferRun,
  freezeTransferIntent,
  type TransferIntentDraft,
} from '../../src/transfer/intent'
import { directoryAdmissionScope, type OutputSession } from '../../src/transfer/output-session'
import {
  admittedOutputFile,
  TEST_DIRECTORY_ADMISSION_SCOPE,
  testOutputIdentity,
} from '../output/admission-fixture'

const ACTIVE_SIGNAL = new AbortController().signal
const CURRENT_SHARE = createTransferIntentDraft({
  shareInstance: testOutputIdentity('paused-browser-share'),
  syntheticRoot: TEST_DIRECTORY_ADMISSION_SCOPE.syntheticRoot,
  selection: { mode: 'node-id', defaultSelected: true, rules: [] },
})
const COMPLETED_FILE = Object.freeze({
  source: Object.freeze({
    shareInstance: CURRENT_SHARE.shareInstance,
    fileId: testOutputIdentity('paused-browser-completed-file'),
    fileRevision: testOutputIdentity('paused-browser-completed-revision'),
  }),
  path: Object.freeze(['completed-browser-file.bin']),
  exactSize: 5n,
})
const FILE = Object.freeze({
  source: Object.freeze({
    shareInstance: CURRENT_SHARE.shareInstance,
    fileId: testOutputIdentity('paused-browser-file'),
    fileRevision: testOutputIdentity('paused-browser-revision'),
  }),
  path: Object.freeze(['paused-browser-file.bin']),
  exactSize: 5n,
})

export interface PausedTaskBrowserFixture {
  readonly databaseName: string
  readonly backend: typeof FILE_SYSTEM_ACCESS_BACKEND | typeof ORIGIN_PRIVATE_BACKEND
  readonly rootName?: string
  readonly initialTransferJobId: string
  readonly initialOutputSessionId: string
}

export function createPausedTaskBrowserFixture(
  backend: PausedTaskBrowserFixture['backend'],
): Promise<PausedTaskBrowserFixture> {
  return createTaskBrowserFixture(backend, false)
}

export function createDiscardTaskBrowserFixture(
  backend: PausedTaskBrowserFixture['backend'],
): Promise<PausedTaskBrowserFixture> {
  return createTaskBrowserFixture(backend, true)
}

export async function interruptFsaDiscardAfterOwnedFileRemoval(
  fixture: PausedTaskBrowserFixture,
): Promise<void> {
  if (fixture.backend !== FILE_SYSTEM_ACCESS_BACKEND || fixture.rootName === undefined) {
    throw new TypeError('Interrupted FSA discard fixture requires an FSA root')
  }
  const descriptor = await (async () => {
    const state = await IndexedDbPausedTaskState.open({ databaseName: fixture.databaseName })
    try {
      const inventory = await state.listResumeState()
      try {
        return requiredReference(inventory.tasks).descriptor
      } finally {
        inventory.close()
      }
    } finally {
      state.close()
    }
  })()

  const repository = await IndexedDbOutputRepository.openExisting(
    fixture.databaseName,
    pausedTaskDescriptorNamespace(descriptor),
  )
  try {
    const [committed, candidates] = await Promise.all([
      scanHarnessRecords((scan) => repository.scanCommitted(scan)),
      scanHarnessRecords((scan) => repository.scanCandidates(scan)),
    ])
    await repository.beginResumeStateDiscard({
      descriptorKey: pausedTaskDescriptorKey(descriptor),
      rootCapabilityRef: descriptor.rootCapabilityRef,
      backend: descriptor.intent.output.backend,
      inventoryDigest: await harnessInventoryDigest(committed, candidates),
      phase: 'retiring',
    })
    const root = await (await originPrivateRoot()).getDirectoryHandle(fixture.rootName)
    await root.removeEntry(FILE.path[0] ?? '')
  } finally {
    repository.close()
  }
}

async function createTaskBrowserFixture(
  backend: PausedTaskBrowserFixture['backend'],
  includeCompletedFile: boolean,
): Promise<PausedTaskBrowserFixture> {
  const databaseName = `paused-task-${crypto.randomUUID()}`
  const originRoot = await originPrivateRoot()
  const rootName = backend === FILE_SYSTEM_ACCESS_BACKEND
    ? `paused-fsa-${crypto.randomUUID()}`
    : undefined
  const root = rootName === undefined
    ? originRoot
    : await originRoot.getDirectoryHandle(rootName, { create: true })
  const rootIdentity = await resolveIndexedDbRootIdentity({
    databaseName,
    backend,
    root,
  })
  const intent = await freezeTransferIntent(CURRENT_SHARE, {
    target: encodeBase64Url(rootIdentity),
    targetKind: 2,
    backend,
    format: backend === FILE_SYSTEM_ACCESS_BACKEND ? 'directory' : 'zip',
  })
  const initialRun = createTransferRun()
  const session = backend === FILE_SYSTEM_ACCESS_BACKEND
    ? await acquireFileSystemAccessOutputSession(root, {
        databaseName,
        outputSessionId: initialRun.outputSessionId,
        directoryAdmissionScope: directoryAdmissionScope(intent),
        transferIntentDigest: intent.digest,
        rootIdentity: intent.output.target,
      })
    : await openOriginPrivateOutputSession({
        databaseName,
        outputSessionId: initialRun.outputSessionId,
        directoryAdmissionScope: directoryAdmissionScope(intent),
        transferIntentDigest: intent.digest,
        rootIdentity: intent.output.target,
        storage: {
          getDirectory: async () => root,
          estimate: () => navigator.storage.estimate(),
        },
        exporter: {
          export: async () => ORIGIN_PRIVATE_EXPORT_COMPLETE,
          abort: async () => undefined,
        },
      })

  if (includeCompletedFile) {
    const completed = await beginTestFile(session, COMPLETED_FILE)
    await completed.transaction.writeRange(0n, Uint8Array.of(9, 8, 7, 6, 5), ACTIVE_SIGNAL)
    await completed.transaction.checkpoint(ACTIVE_SIGNAL)
    await completed.transaction.commit(ACTIVE_SIGNAL)
  }
  const begun = await beginTestFile(session, FILE)
  await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3), ACTIVE_SIGNAL)
  await begun.transaction.checkpoint(ACTIVE_SIGNAL)
  const lifecycle = new IndexedDbBrowserPausedTaskLifecycle(
    () => IndexedDbPausedTaskState.open({ databaseName }),
  )
  const tracked = await lifecycle.track(intent, backend === FILE_SYSTEM_ACCESS_BACKEND
    ? {
        kind: 'PersistentDirectory',
        root,
        rootIdentity,
        targetKind: 2,
        backend,
        format: 'directory',
      }
    : {
        kind: 'OriginPrivateStaging',
        root,
        output: new WritableStream<Uint8Array>(),
        rootIdentity,
        targetKind: 2,
        backend,
        format: 'zip',
      }, session)
  await tracked.pauseJob(new DOMException('Browser fixture paused', 'AbortError'))
  return {
    databaseName,
    backend,
    ...(rootName === undefined ? {} : { rootName }),
    initialTransferJobId: initialRun.transferJobId,
    initialOutputSessionId: initialRun.outputSessionId,
  }
}

export async function resumePausedTaskBrowserFixture(
  fixture: PausedTaskBrowserFixture,
): Promise<{
  readonly descriptorCount: number
  readonly ranges: readonly string[]
  readonly freshTransferJobId: boolean
  readonly freshOutputSessionId: boolean
  readonly permissionStartedSynchronously: boolean
  readonly finalOutputStartedSynchronously: boolean
}> {
  let permissionCalls = 0
  let finalOutputCalls = 0
  const state = await IndexedDbPausedTaskState.open({
    databaseName: fixture.databaseName,
    permission: {
      requestWritePermission: async () => {
        permissionCalls += 1
        return 'granted'
      },
    },
  })
  try {
    const inventory = await state.listResumeState()
    const reference = requiredReference(inventory.tasks)
    const operation = state.resume(reference, {
      currentShare: CURRENT_SHARE,
      acquireOriginPrivateOutput: async () => {
        finalOutputCalls += 1
        return new WritableStream<Uint8Array>()
      },
    })
    const permissionStartedSynchronously = fixture.backend !== FILE_SYSTEM_ACCESS_BACKEND ||
      permissionCalls === 1
    const finalOutputStartedSynchronously = fixture.backend !== ORIGIN_PRIVATE_BACKEND ||
      finalOutputCalls === 1
    const resumed = await operation
    const begun = await beginTestFile(resumed.session)
    const ranges = begun.durableRanges.ranges.map((range) => `${range.start}:${range.end}`)
    await resumed.session.pauseJob(new DOMException('Browser fixture cleanup', 'AbortError'))
    return {
      descriptorCount: inventory.tasks.length,
      ranges,
      freshTransferJobId: resumed.run.transferJobId !== fixture.initialTransferJobId,
      freshOutputSessionId: resumed.run.outputSessionId !== fixture.initialOutputSessionId,
      permissionStartedSynchronously,
      finalOutputStartedSynchronously,
    }
  } finally {
    state.close()
    await cleanupFixture(fixture)
  }
}

export async function discardPausedTaskBrowserFixture(
  fixture: PausedTaskBrowserFixture,
): Promise<{
  readonly kind: string
  readonly preservedCompletedFiles: number
  readonly exportedPartialZip: boolean
  readonly descriptorCount: number
  readonly completedBytes: readonly number[]
  readonly incompleteRemoved: boolean
  readonly zipEntries: readonly string[]
  readonly zipMagic: readonly number[]
  readonly stagingRemoved: boolean
  readonly permissionStartedSynchronously: boolean
  readonly partialOutputStartedSynchronously: boolean
}> {
  let permissionCalls = 0
  let partialOutputCalls = 0
  const zipChunks: Uint8Array[] = []
  const state = await IndexedDbPausedTaskState.open({
    databaseName: fixture.databaseName,
    permission: {
      requestWritePermission: async () => {
        permissionCalls += 1
        return 'granted'
      },
    },
  })
  try {
    const inventory = await state.listResumeState()
    const reference = requiredReference(inventory.tasks)
    const operation = state.discard(reference, {
      currentShare: CURRENT_SHARE,
      acquireOriginPrivateOutput: async () => {
        partialOutputCalls += 1
        return new WritableStream<Uint8Array>({
          write: (chunk) => { zipChunks.push(chunk.slice()) },
        })
      },
    })
    const permissionStartedSynchronously = fixture.backend !== FILE_SYSTEM_ACCESS_BACKEND ||
      permissionCalls === 1
    const partialOutputStartedSynchronously = fixture.backend !== ORIGIN_PRIVATE_BACKEND ||
      partialOutputCalls === 1
    const result = await operation
    if (result.kind !== 'Discarded') {
      throw new Error(`Discard fixture unexpectedly settled as ${result.kind}`)
    }
    const afterDiscard = await state.listResumeState()
    const descriptorCount = afterDiscard.tasks.length
    afterDiscard.close()

    let completedBytes: readonly number[] = []
    let incompleteRemoved = false
    let zipEntries: readonly string[] = []
    let zipMagic: readonly number[] = []
    let stagingRemoved = false
    if (fixture.backend === FILE_SYSTEM_ACCESS_BACKEND) {
      if (fixture.rootName === undefined) throw new Error('FSA discard fixture lost its root name')
      const originRoot = await originPrivateRoot()
      const root = await originRoot.getDirectoryHandle(fixture.rootName)
      const completed = await (await root.getFileHandle(COMPLETED_FILE.path[0] ?? '')).getFile()
      completedBytes = [...new Uint8Array(await completed.arrayBuffer())]
      incompleteRemoved = await entryMissing(root, FILE.path[0] ?? '')
    } else {
      const bytes = concatenateBytes(zipChunks)
      zipMagic = [...bytes.slice(0, 2)]
      const reader = new ZipReader(new Uint8ArrayReader(bytes))
      try {
        const entries = await reader.getEntries()
        zipEntries = entries.map((entry) => entry.filename)
        const completed = entries.find((entry) => entry.filename === COMPLETED_FILE.path[0])
        if (completed === undefined || !('getData' in completed)) {
          throw new Error('Partial ZIP lost its completed member')
        }
        completedBytes = [...await completed.getData(new Uint8ArrayWriter())]
      } finally {
        await reader.close()
      }
      try {
        await openOriginPrivateStagingNamespace(
          await originPrivateRoot(),
          reference.descriptor.intent.digest,
        )
      } catch (error) {
        stagingRemoved = errorName(error) === 'NotFoundError'
      }
      incompleteRemoved = stagingRemoved
    }
    return {
      kind: result.kind,
      preservedCompletedFiles: result.preservedCompletedFiles,
      exportedPartialZip: result.exportedPartialZip,
      descriptorCount,
      completedBytes,
      incompleteRemoved,
      zipEntries,
      zipMagic,
      stagingRemoved,
      permissionStartedSynchronously,
      partialOutputStartedSynchronously,
    }
  } finally {
    state.close()
    await cleanupFixture(fixture)
  }
}

export async function probeOriginPrivateDiscardExportFailure(): Promise<{
  readonly firstKind: string
  readonly firstReason: string
  readonly retryKind: string
  readonly retryReason: string
  readonly outputCalls: number
  readonly descriptorCount: number
  readonly stagingRetained: boolean
}> {
  const fixture = await createDiscardTaskBrowserFixture(ORIGIN_PRIVATE_BACKEND)
  let outputCalls = 0
  let intentDigest: string | undefined
  const state = await IndexedDbPausedTaskState.open({ databaseName: fixture.databaseName })
  try {
    const firstInventory = await state.listResumeState()
    const firstReference = requiredReference(firstInventory.tasks)
    intentDigest = firstReference.descriptor.intent.digest
    const first = await state.discard(firstReference, {
      currentShare: CURRENT_SHARE,
      acquireOriginPrivateOutput: async () => {
        outputCalls += 1
        return new WritableStream<Uint8Array>({
          write: () => { throw new Error('Injected partial ZIP publication failure') },
        })
      },
    })
    firstInventory.close()

    const retryInventory = await state.listResumeState()
    const retry = await state.discard(requiredReference(retryInventory.tasks), {
      currentShare: CURRENT_SHARE,
      acquireOriginPrivateOutput: async () => {
        outputCalls += 1
        return new WritableStream<Uint8Array>()
      },
    })
    retryInventory.close()
    const stagingRetained = await openOriginPrivateStagingNamespace(
      await originPrivateRoot(),
      intentDigest,
    ).then(() => true, () => false)
    return {
      firstKind: first.kind,
      firstReason: 'reason' in first ? first.reason : '',
      retryKind: retry.kind,
      retryReason: 'reason' in retry ? retry.reason : '',
      outputCalls,
      descriptorCount: (await state.list()).length,
      stagingRetained,
    }
  } finally {
    state.close()
    if (intentDigest !== undefined) {
      const namespace = await openOriginPrivateStagingNamespace(
        await originPrivateRoot(),
        intentDigest,
      ).catch(() => undefined)
      if (namespace !== undefined) await removeOriginPrivateStagingNamespace(namespace)
    }
    await cleanupFixture(fixture)
  }
}

export async function probePausedTaskPermissionAndShareAuthority(): Promise<{
  readonly deniedFailure: string
  readonly deniedRunCreations: number
  readonly mismatchName: string
  readonly mismatchPermissionCalls: number
}> {
  const fixture = await createPausedTaskBrowserFixture(FILE_SYSTEM_ACCESS_BACKEND)
  let deniedRunCreations = 0
  const denied = await IndexedDbPausedTaskState.open({
    databaseName: fixture.databaseName,
    permission: {
      requestWritePermission: async () => 'denied',
    },
    createRun: () => {
      deniedRunCreations += 1
      return createTransferRun()
    },
  })
  let deniedFailure = 'resolved'
  try {
    const inventory = await denied.listResumeState()
    await denied.resume(requiredReference(inventory.tasks), { currentShare: CURRENT_SHARE })
  } catch (error) {
    deniedFailure = error instanceof PausedTaskCapabilityError ? error.failure : errorName(error)
  } finally {
    denied.close()
  }

  let mismatchPermissionCalls = 0
  const mismatch = await IndexedDbPausedTaskState.open({
    databaseName: fixture.databaseName,
    permission: {
      requestWritePermission: async () => {
        mismatchPermissionCalls += 1
        return 'granted'
      },
    },
  })
  let mismatchName = 'resolved'
  try {
    const inventory = await mismatch.listResumeState()
    await mismatch.resume(requiredReference(inventory.tasks), {
      currentShare: changedShareAuthority(),
    })
  } catch (error) {
    mismatchName = errorName(error)
  } finally {
    mismatch.close()
    await cleanupFixture(fixture)
  }
  return {
    deniedFailure,
    deniedRunCreations,
    mismatchName,
    mismatchPermissionCalls,
  }
}

export async function probePausedTaskStaleCapability(): Promise<string> {
  const fixture = await createPausedTaskBrowserFixture(FILE_SYSTEM_ACCESS_BACKEND)
  const state = await IndexedDbPausedTaskState.open({
    databaseName: fixture.databaseName,
    permission: {
      requestWritePermission: async () => 'granted',
    },
  })
  try {
    const inventory = await state.listResumeState()
    const reference = requiredReference(inventory.tasks)
    const descriptor = reference.descriptor
    const originRoot = await originPrivateRoot()
    const replacementName = `paused-replacement-${crypto.randomUUID()}`
    const replacement = await originRoot.getDirectoryHandle(replacementName, { create: true })
    await replaceCapabilityHandle(
      fixture.databaseName,
      descriptor.rootCapabilityRef,
      replacement,
    )
    try {
      await state.resume(reference, { currentShare: CURRENT_SHARE })
      return 'resolved'
    } catch (error) {
      return error instanceof PausedTaskCapabilityError ? error.failure : errorName(error)
    } finally {
      await originRoot.removeEntry(replacementName, { recursive: true })
    }
  } finally {
    state.close()
    await cleanupFixture(fixture)
  }
}

async function beginTestFile(
  session: OutputSession,
  file: Parameters<typeof admittedOutputFile>[1] = FILE,
) {
  return session.beginFile(await admittedOutputFile(session, file), ACTIVE_SIGNAL)
}

function requiredReference(
  references: Awaited<ReturnType<IndexedDbPausedTaskState['listResumeState']>>['tasks'],
) {
  const reference = references[0]
  if (reference === undefined) throw new Error('Paused task reference is unavailable')
  return reference
}

function changedShareAuthority(): TransferIntentDraft {
  return createTransferIntentDraft({
    shareInstance: CURRENT_SHARE.shareInstance,
    syntheticRoot: CURRENT_SHARE.syntheticRoot,
    selection: { mode: 'node-id', defaultSelected: false, rules: [] },
  })
}

async function replaceCapabilityHandle(
  databaseName: string,
  capabilityRef: string,
  replacement: FileSystemDirectoryHandle,
): Promise<void> {
  const database = await openDatabase(databaseName)
  try {
    const transaction = database.transaction(INDEXEDDB_ROOT_CAPABILITY_STORE, 'readwrite')
    const store = transaction.objectStore(INDEXEDDB_ROOT_CAPABILITY_STORE)
    const current = await requestResult<Record<string, unknown> | undefined>(store.get(capabilityRef))
    if (current === undefined) throw new Error('Root capability record is unavailable')
    store.put({ ...current, handle: replacement })
    await transactionCompletion(transaction)
  } finally {
    database.close()
  }
}

async function scanHarnessRecords(
  scanPage: (scan: OutputJournalScan) => Promise<OutputJournalPage>,
): Promise<readonly PersistedOutputRecord[]> {
  const records: PersistedOutputRecord[] = []
  let cursor: string | undefined
  do {
    const page = await scanPage({
      direction: 'ascending',
      ...(cursor === undefined ? {} : { cursor }),
    })
    records.push(...page.records)
    cursor = page.nextCursor
  } while (cursor !== undefined)
  return records
}

async function harnessInventoryDigest(
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
): Promise<string> {
  const entry = (store: string, record: PersistedOutputRecord): readonly string[] => [
    store,
    outputRecordKey(record),
    record.recordId,
    record.generation.toString(),
    record.checksum,
  ]
  const entries = [
    ...committed.map((record) => entry('committed', record)),
    ...candidates.map((record) => entry('candidate', record)),
  ]
  return encodeBase64Url(await sha256(new TextEncoder().encode(JSON.stringify(entries))))
}

async function cleanupFixture(fixture: PausedTaskBrowserFixture): Promise<void> {
  await deleteDatabase(fixture.databaseName)
  if (fixture.rootName === undefined) return
  const root = await originPrivateRoot()
  await root.removeEntry(fixture.rootName, { recursive: true }).catch(() => undefined)
}

async function entryMissing(
  root: FileSystemDirectoryHandle,
  name: string,
): Promise<boolean> {
  try {
    await root.getFileHandle(name)
    return false
  } catch (error) {
    if (errorName(error) === 'NotFoundError') return true
    throw error
  }
}

function concatenateBytes(chunks: readonly Uint8Array[]): Uint8Array {
  const length = chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
  const result = new Uint8Array(length)
  let offset = 0
  for (const chunk of chunks) {
    result.set(chunk, offset)
    offset += chunk.byteLength
  }
  return result
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

function openDatabase(name: string): Promise<IDBDatabase> {
  return requestResult(indexedDB.open(name))
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('blocked', () => reject(
      new DOMException('Paused-task test database deletion was blocked', 'InvalidStateError'),
    ), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionCompletion(transaction: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
  })
}

function errorName(error: unknown): string {
  return error instanceof Error || error instanceof DOMException ? error.name : String(error)
}
