import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  INDEXEDDB_OUTPUT_STORE_SCHEMAS,
  OUTPUT_DATABASE_VERSION,
  INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE,
  openIndexedDbCheckpointDatabase,
  requestResult,
  transactionCompletion,
} from '../../src/output/browser/indexeddb-database'
import { IndexedDbFileCheckpointRepository } from '../../src/output/browser/indexeddb-repository'
import { IndexedDbSemanticWriteStage } from '../../src/output/browser/indexeddb/materialization-ledger-transactions'
import { createMaterializationLedgerBinding } from '../../src/output/materialization-ledger/codec'
import {
  createFinalizedFileMaterializationRecords,
  createMaterializationDirectoryAdmittedEntry,
} from '../../src/output/materialization-ledger/journal'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MaterializationLedgerSealPurpose,
} from '../../src/output/materialization-ledger/model'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  deriveCheckpointLineageID,
  newFileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import { durableCheckpointNamespaceIdentity } from '../../src/output/persistence/namespace'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { VerifiedFinalOutputFile } from '../../src/transfer/output/session'

const RESET_SENTINEL_BYTES = Uint8Array.of(91, 92, 93, 94)
const INCOMPATIBLE_DATABASE_VERSION = OUTPUT_DATABASE_VERSION - 1
const INCOMPATIBLE_STORE_NAMES = ['unknown-output-format', 'receive-operation-v2-records'] as const
const CONCURRENT_FILE_COUNT = 8
const PAGING_DIRECTORY_COUNT = 130

export interface IndexedDbOutputRepositoryProbeResult {
  readonly concurrentFinals: number
  readonly idempotentRetry: string
  readonly indexedClassification: string
  readonly operationWideGetAllCalls: number
  readonly firstPageEntries: number
  readonly secondPageEntries: number
  readonly stableDirectoryRetry: string
  readonly sealPages: string
  readonly candidateSealRejected: boolean
  readonly pathConflictRejected: boolean
  readonly retiredRows: number
}

export interface IndexedDbOutputResetProbeResult {
  readonly oldRowsRemaining: number
  readonly storeCount: number
  readonly exactIndexesPresent: boolean
  readonly publishedSentinelBytes: readonly number[]
  readonly blockedUpgradeRejected: boolean
  readonly currentRowsPreserved: boolean
}

export interface IndexedDbFinalRejectionProbeResult {
  readonly candidateRejected: boolean
  readonly stalePredecessorRejected: boolean
  readonly foreignHandleRejected: boolean
}

export interface IndexedDbRestartProbeResult {
  readonly first: string
  readonly ambiguousRetry: string
  readonly staleRejected: boolean
  readonly foreignHandleRejected: boolean
  readonly fileBytes: readonly number[]
}

export async function probeIndexedDbOutputRepository(
  databaseName: string,
): Promise<IndexedDbOutputRepositoryProbeResult> {
  const binding = bindingFor(10)
  const ledgerBinding = await createMaterializationLedgerBinding(ledgerBindingInput(binding))
  const root = await navigator.storage.getDirectory()
  const handles: FileSystemFileHandle[] = []
  let repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
  try {
    const files = await Promise.all(Array.from({ length: CONCURRENT_FILE_COUNT }, async (_, index) => {
      const handle = await root.getFileHandle(`idb-output-${databaseName}-${index}`, { create: true })
      handles.push(handle)
      return preparedFile(index + 20, handle, binding.operationId, binding.authorityRef)
    }))
    const installs = await repository.installInitialClaims(files.map(file => file.candidate))
    if (installs.some(result => result.kind !== 'installed')) {
      throw new Error('initial semantic checkpoint batch did not install')
    }
    await Promise.all(files.map(file => repository.commitCreatedFile(file.created)))
    const receipts = await Promise.all(files.map(file => repository.commitFinalFile(file.finalCommit)))
    const retry = await repository.commitFinalFile(files[0]!.finalCommit)

    const originalGetAll = IDBIndex.prototype.getAll
    let operationWideGetAllCalls = 0
    IDBIndex.prototype.getAll = function (query?: IDBValidKey | IDBKeyRange | null, count?: number) {
      if (typeof query === 'string' && query === binding.operationId) operationWideGetAllCalls += 1
      return originalGetAll.call(this, query, count)
    }
    let classification: string
    try {
      const decisions = await repository.classifyLineages(files.map(file => file.lookup))
      classification = decisions.every(decision => decision.kind === 'exact') ? 'exact' : 'unexpected'
    } finally {
      IDBIndex.prototype.getAll = originalGetAll
    }

    const directoryEntries = await Promise.all(Array.from(
      { length: PAGING_DIRECTORY_COUNT },
      (_, index) => createMaterializationDirectoryAdmittedEntry(ledgerBinding, {
        relativePath: snapshotMaterializationRootRelativePath([`directory-${index}`]),
        directoryId: identity(16, (index + 1) % 250 + 1),
        generation: identity(16, (index + 2) % 250 + 1),
        ownedObjectId: identity(32, (index + 3) % 250 + 1),
        parent: rootDirectoryCoordinates(),
      }),
    ))
    for (const entry of directoryEntries) {
      await repository.appendDirectoryAdmission(ledgerBinding, entry)
    }
    repository.close()
    repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
    const stableDirectoryRetry = await repository.appendDirectoryAdmission(
      ledgerBinding,
      directoryEntries[0]!,
    )
    const firstPage = await repository.scanMaterializationLedgerEntries(ledgerBinding, {
      limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    })
    if (firstPage.continuation === undefined) throw new Error('real IndexedDB page did not continue')
    const secondPage = await repository.scanMaterializationLedgerEntries(ledgerBinding, {
      after: firstPage.continuation,
      limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    })
    const seal = await repository.sealMaterializationLedger({
      binding: ledgerBinding,
      sealSequence: 1n,
      purpose: MaterializationLedgerSealPurpose.ResumableSnapshot,
    })

    const pending = await preparedFile(90, handles[0]!, binding.operationId, binding.authorityRef)
    await repository.createInitialCheckpoint(pending.candidate)
    const candidateSealRejected = await repository.sealMaterializationLedger({
      binding: ledgerBinding,
      sealSequence: 2n,
      purpose: MaterializationLedgerSealPurpose.Terminal,
    }).then(() => false, () => true)
    const conflictingDirectory = await createMaterializationDirectoryAdmittedEntry(ledgerBinding, {
      relativePath: files[0]!.finalCommit.records.ledgerEntry.relativePath,
      directoryId: identity(16, 230),
      generation: identity(16, 231),
      ownedObjectId: identity(32, 232),
      parent: rootDirectoryCoordinates(),
    })
    const pathConflictRejected = await repository.appendDirectoryAdmission(
      ledgerBinding,
      conflictingDirectory,
    ).then(() => false, () => true)

    let retiredRows = 0
    for (;;) {
      const retired = await repository.retireMaterializationLedgerBatch(
        ledgerBinding,
        MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
      )
      retiredRows += retired.deletedRows
      if (retired.state === 'complete') break
    }
    return {
      concurrentFinals: receipts.filter(receipt => receipt.classification === 'insert').length,
      idempotentRetry: retry.classification,
      indexedClassification: classification,
      operationWideGetAllCalls,
      firstPageEntries: firstPage.entries.length,
      secondPageEntries: secondPage.entries.length,
      stableDirectoryRetry,
      sealPages: seal.pageCount.toString(),
      candidateSealRejected,
      pathConflictRejected,
      retiredRows,
    }
  } finally {
    repository.close()
    for (let index = 0; index < handles.length; index += 1) {
      await root.removeEntry(`idb-output-${databaseName}-${index}`).catch(() => undefined)
    }
    await deleteDatabase(databaseName)
  }
}

export async function probeIndexedDbFinalFaultCuts(prefix: string): Promise<readonly string[]> {
  const stages = [
    IndexedDbSemanticWriteStage.FinalCheckpoint,
    IndexedDbSemanticWriteStage.FinalProof,
    IndexedDbSemanticWriteStage.FinalLedgerEntry,
  ] as const
  const root = await navigator.storage.getDirectory()
  const results: string[] = []
  for (let index = 0; index < stages.length; index += 1) {
    const stage = stages[index]!
    const name = `${prefix}-${index}`
    const binding = bindingFor(120 + index)
    const ledgerBinding = await createMaterializationLedgerBinding(ledgerBindingInput(binding))
    const fileName = `fault-${name}`
    const handle = await root.getFileHandle(fileName, { create: true })
    const file = await preparedFile(140 + index, handle, binding.operationId, binding.authorityRef)
    let repository = await IndexedDbFileCheckpointRepository.open(binding, name)
    try {
      await repository.createInitialCheckpoint(file.candidate)
      await repository.commitCreatedFile(file.created)
      repository.close()
      repository = await IndexedDbFileCheckpointRepository.open(binding, name, {
        afterQueuedWrite: queued => {
          if (queued === stage) throw new Error(`fault:${stage}`)
        },
      })
      const rejected = await repository.commitFinalFile(file.finalCommit).then(() => false, () => true)
      repository.close()
      repository = await IndexedDbFileCheckpointRepository.open(binding, name)
      const committed = await repository.readCommitted(file.initialCommitted.recordId)
      const proof = await repository.readMaterializationFinalProof(
        ledgerBinding,
        file.finalCommit.records.finalProof.proofId,
      )
      const page = await repository.scanMaterializationLedgerEntries(ledgerBinding, {
        limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
      })
      results.push(rejected && committed?.checksum === file.initialCommitted.checksum &&
        proof === undefined && page.entries.length === 0 ? stage : 'failed')
    } finally {
      repository.close()
      await root.removeEntry(fileName).catch(() => undefined)
      await deleteDatabase(name)
    }
  }
  return results
}

export async function probeIndexedDbFinalRejections(
  databaseName: string,
): Promise<IndexedDbFinalRejectionProbeResult> {
  const binding = bindingFor(180)
  const root = await navigator.storage.getDirectory()
  const fileNames = ['candidate', 'stale', 'foreign'].map(label => `${databaseName}-${label}`)
  const handles = await Promise.all(fileNames.map(name => root.getFileHandle(name, { create: true })))
  const [candidate, stale, foreign] = await Promise.all(handles.map((handle, index) =>
    preparedFile(190 + index, handle, binding.operationId, binding.authorityRef)))
  const repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
  try {
    await repository.createInitialCheckpoint(candidate!.candidate)
    const candidateRejected = await repository.commitFinalFile(candidate!.finalCommit)
      .then(() => false, () => true)

    await repository.createInitialCheckpoint(stale!.candidate)
    await repository.commitCreatedFile(stale!.created)
    const staleCheckpoint = newFileCheckpointV2({
      ...stale!.initialCommitted,
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 4n }],
    })
    await repository.commitDurableCut(stale!.initialCommitted, staleCheckpoint)
    const stalePredecessorRejected = await repository.commitFinalFile(stale!.finalCommit)
      .then(() => false, () => true)

    await repository.createInitialCheckpoint(foreign!.candidate)
    await repository.commitCreatedFile(foreign!.created)
    await repository.deleteHandle(foreign!.created.handle.id)
    await repository.putHandle({
      ...foreign!.created.handle,
      id: `${foreign!.created.handle.id}:foreign`,
      ownedObjectId: identity(32, 250),
    })
    const foreignHandleRejected = await repository.commitFinalFile(foreign!.finalCommit)
      .then(() => false, () => true)

    return { candidateRejected, stalePredecessorRejected, foreignHandleRejected }
  } finally {
    repository.close()
    for (const fileName of fileNames) await root.removeEntry(fileName).catch(() => undefined)
    await deleteDatabase(databaseName)
  }
}

export async function probeIndexedDbOwnedFileRestart(
  databaseName: string,
): Promise<IndexedDbRestartProbeResult> {
  const binding = bindingFor(215)
  const root = await navigator.storage.getDirectory()
  const fileNames = [`${databaseName}-restart`, `${databaseName}-foreign`]
  const handles = await Promise.all(fileNames.map(name => root.getFileHandle(name, { create: true })))
  const writer = await handles[0]!.createWritable()
  await writer.write(Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8))
  await writer.close()
  const [restartFile, foreignFile] = await Promise.all(handles.map((handle, index) =>
    preparedFile(220 + index, handle, binding.operationId, binding.authorityRef)))
  let repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
  try {
    const restartPaused = pausedCheckpoint(restartFile!.initialCommitted)
    await repository.createInitialCheckpoint(restartFile!.candidate)
    await repository.commitCreatedFile(restartFile!.created)
    await repository.commitDurableCut(restartFile!.initialCommitted, restartPaused)
    const restartReset = zeroRestartCheckpoint(restartPaused)
    const restartInput = {
      previous: restartPaused,
      reset: restartReset,
      expectedHandle: restartFile!.created.handle,
    }
    const first = await repository.restartOwnedFile(restartInput)
    repository.close()
    repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
    const ambiguousRetry = await repository.restartOwnedFile(restartInput)

    const resumed = newFileCheckpointV2({
      ...restartReset,
      stateGeneration: restartReset.stateGeneration + 1n,
      phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    })
    await repository.resumePausedCheckpoint(restartReset, resumed)
    const newerPaused = newFileCheckpointV2({
      ...resumed,
      stateGeneration: resumed.stateGeneration + 1n,
      checkpointGeneration: resumed.checkpointGeneration + 1n,
      phase: FILE_CHECKPOINT_PHASE_PAUSED,
    })
    await repository.commitDurableCut(resumed, newerPaused)
    const staleRejected = await repository.restartOwnedFile(restartInput)
      .then(() => false, () => true)

    const foreignPaused = pausedCheckpoint(foreignFile!.initialCommitted)
    await repository.createInitialCheckpoint(foreignFile!.candidate)
    await repository.commitCreatedFile(foreignFile!.created)
    await repository.commitDurableCut(foreignFile!.initialCommitted, foreignPaused)
    const foreignHandleRejected = await repository.restartOwnedFile({
      previous: foreignPaused,
      reset: zeroRestartCheckpoint(foreignPaused),
      expectedHandle: {
        ...foreignFile!.created.handle,
        id: `${foreignFile!.created.handle.id}:foreign`,
      },
    }).then(() => false, () => true)

    const file = await handles[0]!.getFile()
    return {
      first,
      ambiguousRetry,
      staleRejected,
      foreignHandleRejected,
      fileBytes: [...new Uint8Array(await file.arrayBuffer())],
    }
  } finally {
    repository.close()
    for (const fileName of fileNames) await root.removeEntry(fileName).catch(() => undefined)
    await deleteDatabase(databaseName)
  }
}

export async function probeIndexedDbOutputReset(
  databaseName: string,
): Promise<IndexedDbOutputResetProbeResult> {
  const root = await navigator.storage.getDirectory()
  const sentinelName = `reset-${databaseName}`
  const sentinelHandle = await root.getFileHandle(sentinelName, { create: true })
  const writer = await sentinelHandle.createWritable()
  await writer.write(RESET_SENTINEL_BYTES)
  await writer.close()
  try {
    const incompatible = await openDatabase(databaseName, INCOMPATIBLE_DATABASE_VERSION, request => {
      for (const name of INCOMPATIBLE_STORE_NAMES) {
        request.result.createObjectStore(name, { keyPath: 'obsoleteKey' })
      }
    })
    const seed = incompatible.transaction([...INCOMPATIBLE_STORE_NAMES], 'readwrite')
    for (const name of INCOMPATIBLE_STORE_NAMES) {
      seed.objectStore(name).put({ obsoleteKey: name, handle: sentinelHandle })
    }
    await transactionCompletion(seed)
    incompatible.close()

    const upgraded = await openIndexedDbCheckpointDatabase(databaseName)
    const countsTransaction = upgraded.transaction(
      INDEXEDDB_OUTPUT_STORE_SCHEMAS.map(schema => schema.name),
      'readonly',
    )
    const exactIndexesPresent = INDEXEDDB_OUTPUT_STORE_SCHEMAS.every(schema => {
      const store = countsTransaction.objectStore(schema.name)
      return JSON.stringify(store.keyPath) === JSON.stringify(schema.keyPath) &&
        store.indexNames.length === schema.indexes.length && schema.indexes.every(expected => {
          if (!store.indexNames.contains(expected.name)) return false
          const index = store.index(expected.name)
          return JSON.stringify(index.keyPath) === JSON.stringify(expected.keyPath) &&
            index.unique === (expected.unique ?? false) && !index.multiEntry
        })
    })
    const counts = await Promise.all(INDEXEDDB_OUTPUT_STORE_SCHEMAS.map(schema => requestResult(
      countsTransaction.objectStore(schema.name).count(),
    )))
    await transactionCompletion(countsTransaction)
    const legacyStoresRemaining = INCOMPATIBLE_STORE_NAMES.filter(name => name === 'unknown-output-format' &&
      upgraded.objectStoreNames.contains(name)).length
    const storeCount = upgraded.objectStoreNames.length
    const currentWrite = upgraded.transaction(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, 'readwrite')
    currentWrite.objectStore(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE).put({ id: 'current-record' })
    await transactionCompletion(currentWrite)
    upgraded.close()
    const reopened = await openIndexedDbCheckpointDatabase(databaseName)
    const currentRead = reopened.transaction(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE, 'readonly')
    const currentRowsPreserved = await requestResult(
      currentRead.objectStore(INDEXEDDB_FILE_CHECKPOINT_CONTROL_STORE).count(),
    ) === 1
    await transactionCompletion(currentRead)
    reopened.close()

    const blocker = await openDatabase(`${databaseName}-blocked`, INCOMPATIBLE_DATABASE_VERSION,
      request => request.result.createObjectStore('unsupported-format'))
    const blockedUpgradeRejected = await openIndexedDbCheckpointDatabase(`${databaseName}-blocked`)
      .then(database => {
        database.close()
        return false
      }, () => true)
    blocker.close()
    const published = await (await root.getFileHandle(sentinelName)).getFile()
    return {
      oldRowsRemaining: counts.reduce((sum, count) => sum + count, legacyStoresRemaining),
      storeCount,
      exactIndexesPresent,
      publishedSentinelBytes: [...new Uint8Array(await published.arrayBuffer())],
      blockedUpgradeRejected,
      currentRowsPreserved,
    }
  } finally {
    await root.removeEntry(sentinelName).catch(() => undefined)
    await deleteDatabase(databaseName)
    await deleteDatabase(`${databaseName}-blocked`)
  }
}

async function preparedFile(
  seed: number,
  handle: FileSystemFileHandle,
  operationId: string,
  authorityRef: string,
) {
  const spec = {
    operationId,
    receiveIntentDigest: identity(32, 11),
    materializationBindingDigest: identity(32, 12),
    fileId: identity(16, seed),
    fileRevision: identity(16, seed + 1),
    canonicalPath: [`file-${seed}.bin`],
    exactSize: 8n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef,
    ownedObjectId: identity(32, seed + 2),
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
  } as const
  const candidate = newFileCheckpointV2({ ...spec, commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE })
  const initialCommitted = newFileCheckpointV2({
    ...spec,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const finalCheckpoint = newFileCheckpointV2({
    ...spec,
    checkpointGeneration: 1n,
    verifiedRanges: [{ start: 0n, end: 8n }],
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const binding = bindingForValues(operationId, authorityRef)
  const ledgerBinding = await createMaterializationLedgerBinding(ledgerBindingInput(binding))
  const finalOutput = new VerifiedFinalOutputFile({
    backend: 'browser-fsa',
    outputSessionId: `session-${seed}`,
    canonicalPath: snapshotMaterializationRootRelativePath(finalCheckpoint.canonicalPath),
    ownedFileIdentity: finalCheckpoint.ownedObjectId,
  }, {
    shareInstance: identity(16, 13),
    fileId: finalCheckpoint.fileId,
    fileRevision: finalCheckpoint.fileRevision,
  }, finalCheckpoint.exactSize)
  return {
    candidate,
    initialCommitted,
    created: {
      candidate,
      committed: initialCommitted,
      handle: {
        id: `probe-handle:${operationId}:${candidate.ownedObjectId}`,
        operationId,
        kind: 1,
        authorityRef,
        ownedObjectId: candidate.ownedObjectId,
        handle,
      },
    },
    lookup: {
      lineageId: deriveCheckpointLineageID(finalCheckpoint),
      fileId: finalCheckpoint.fileId,
      canonicalPath: finalCheckpoint.canonicalPath,
      fileRevision: finalCheckpoint.fileRevision,
      exactSize: finalCheckpoint.exactSize,
    },
    finalCommit: {
      binding: ledgerBinding,
      expectedCommittedCheckpoint: initialCommitted,
      records: await createFinalizedFileMaterializationRecords({
        binding: ledgerBinding,
        finalOutput,
        finalCheckpoint,
      }),
      expectedPersistedOwnedFileIdentity: finalCheckpoint.ownedObjectId,
    },
  }
}

function pausedCheckpoint(initial: ReturnType<typeof newFileCheckpointV2>) {
  return newFileCheckpointV2({
    ...initial,
    stateGeneration: initial.stateGeneration + 1n,
    checkpointGeneration: initial.checkpointGeneration + 1n,
    verifiedRanges: [{ start: 0n, end: 4n }],
    phase: FILE_CHECKPOINT_PHASE_PAUSED,
  })
}

function zeroRestartCheckpoint(paused: ReturnType<typeof newFileCheckpointV2>) {
  return newFileCheckpointV2({
    ...paused,
    stateGeneration: paused.stateGeneration + 1n,
    checkpointGeneration: paused.checkpointGeneration + 1n,
    verifiedRanges: [],
  })
}

function bindingFor(seed: number) {
  return bindingForValues(identity(16, seed), identity(32, 14))
}

function rootDirectoryCoordinates() {
  return {
    relativePath: snapshotMaterializationRootRelativePath([]),
    directoryId: identity(16, 240),
    generation: identity(16, 241),
    ownedObjectId: identity(32, 242),
  }
}

function bindingForValues(operationId: string, authorityRef: string) {
  return durableCheckpointNamespaceIdentity({
    operationId,
    receiveIntentDigest: identity(32, 11),
    materializationBindingDigest: identity(32, 12),
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef,
  })
}

function ledgerBindingInput(binding: ReturnType<typeof bindingForValues>) {
  return {
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    authorityRef: binding.authorityRef,
  }
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}

function openDatabase(
  name: string,
  version: number,
  upgrade: (request: IDBOpenDBRequest) => void,
): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(name, version)
    request.addEventListener('upgradeneeded', () => upgrade(request), { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('blocked', () => resolve(), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}
