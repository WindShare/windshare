import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  CHECKPOINT_DATABASE_VERSION,
  INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
  INDEXEDDB_V6_STORE_SCHEMAS,
  transactionCompletion,
} from '../../src/output/browser/indexeddb-database'
import {
  IndexedDbFileCheckpointRepository,
  IndexedDbReceiveOperationRepository,
} from '../../src/output/browser/indexeddb-repository'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  deriveCheckpointLineageID,
  encodeFileCheckpointV2,
  newFileCheckpointV2,
  type FileCheckpointSpec,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  type DurableCheckpointNamespaceIdentity,
} from '../../src/output/persistence/namespace'

const LEGACY_PAUSED_STORE = 'paused-task-descriptor-v1'

export interface IndexedDbV6Probe {
  readonly blockedUpgrade: string
  readonly blockedRequestClosedLate: boolean
  readonly versionChange: string
  readonly schemaVersion: number
  readonly v6StoresPresent: boolean
  readonly legacyStoreRetainedForCleanup: boolean
  readonly legacyRowsVisibleToV6: boolean
}

export interface IndexedDbCheckpointLineageProbe {
  readonly putCandidateSurfacePresent: boolean
  readonly unbackedUpdateRejection: string
  readonly unbackedUpdateCandidateRows: number
  readonly updateConcurrencyOutcomes: readonly string[]
  readonly updateConcurrencyCandidateRows: number
  readonly concurrentKinds: readonly string[]
  readonly concurrentObjectConverged: boolean
  readonly candidateRowsBeforeResolution: number
  readonly candidateBeforeObjectDecision: string
  readonly candidateRowsAfterResolution: number
  readonly resolutionReplayDecision: string
  readonly revisionDecision: string
  readonly ownershipDecision: string
  readonly invalidDecision: string
  readonly crossLineageOwnershipDecision: string
  readonly unresolvedCandidateRejection: string
  readonly resolvedRange: string
}

export async function probeIndexedDbV6Replacement(): Promise<IndexedDbV6Probe> {
  const blocked = await probeBlockedUpgrade()
  const versionChange = await probeVersionChange()
  const replacement = await probeReplacement()
  return Object.freeze({
    blockedUpgrade: blocked.rejection,
    blockedRequestClosedLate: blocked.deleted,
    versionChange,
    ...replacement,
  })
}

export async function probeIndexedDbCheckpointLineage(): Promise<IndexedDbCheckpointLineageProbe> {
  const databaseName = `w3c-checkpoint-lineage-${crypto.randomUUID()}`
  const binding = checkpointBinding()
  const first = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
  const second = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
  const left = checkpoint(binding, {
    fileId: identity(16, 0x11),
    fileRevision: identity(16, 0x21),
    canonicalPath: ['concurrent.bin'],
    ownedObjectId: identity(32, 0x31),
  })
  const right = checkpoint(binding, {
    fileId: left.fileId,
    fileRevision: left.fileRevision,
    canonicalPath: left.canonicalPath,
    ownedObjectId: identity(32, 0x32),
  })

  try {
    const putCandidateSurfacePresent = 'putCandidate' in first
    const unbackedInitial = checkpoint(binding, {
      fileId: identity(16, 0x18),
      fileRevision: identity(16, 0x29),
      canonicalPath: ['unbacked.bin'],
      ownedObjectId: identity(32, 0x39),
    })
    const unbackedCommitted = checkpointTransition(unbackedInitial, {
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
    const unbackedUpdate = checkpointTransition(unbackedCommitted, {
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 1n }],
      commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    })
    const unbackedUpdateRejection = await rejectionName(
      first.stageCheckpointUpdate(unbackedCommitted, unbackedUpdate),
    )
    const unbackedUpdateCandidateRows = (await first.scanCandidates({
      direction: 'ascending',
      fileId: unbackedUpdate.fileId,
    })).records.length

    const concurrent = await Promise.all([
      first.createInitialCheckpoint(left),
      second.createInitialCheckpoint(right),
    ])
    const concurrentKinds = Object.freeze(concurrent.map((result) => result.kind).sort())
    const concurrentRecords = concurrent.map((result) => {
      if (result.kind !== 'installed' && result.kind !== 'exact') {
        throw new Error(`concurrent initial claim returned ${result.kind}`)
      }
      return result.record
    })
    const concurrentObjectConverged = concurrentRecords[0]!.ownedObjectId ===
      concurrentRecords[1]!.ownedObjectId
    first.close()
    second.close()

    const repository = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
    try {
      const candidateRowsBeforeResolution = (await repository.scanCandidates({
        direction: 'ascending',
        fileId: left.fileId,
      })).records.length
      const candidateBeforeObject = await repository.lookupLineage(lineageRequest(binding, left))
      if (candidateBeforeObject.kind !== 'exact') {
        throw new Error('candidate-only reservation did not retain exact lineage authority')
      }
      const selectedCandidate = candidateBeforeObject.record
      const selectedCommitted = checkpointTransition(selectedCandidate, {
        commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
      })
      await repository.commitCheckpointCandidate(selectedCandidate, selectedCommitted)
      await repository.commitCheckpointCandidate(selectedCandidate, selectedCommitted)
      const candidateRowsAfterResolution = (await repository.scanCandidates({
        direction: 'ascending',
        fileId: left.fileId,
      })).records.length
      const resolutionReplay = await repository.lookupLineage(lineageRequest(binding, left))

      const updatePredecessor = await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x19),
        fileRevision: identity(16, 0x2a),
        canonicalPath: ['update-concurrency.bin'],
        ownedObjectId: identity(32, 0x3a),
      })
      const updateLeft = checkpointTransition(updatePredecessor, {
        checkpointGeneration: 1n,
        verifiedRanges: [{ start: 0n, end: 1n }],
        commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
      })
      const updateRight = checkpointTransition(updatePredecessor, {
        checkpointGeneration: 1n,
        verifiedRanges: [{ start: 0n, end: 2n }],
        commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
      })
      const updateContender = await IndexedDbFileCheckpointRepository.open(binding, databaseName)
      let updateConcurrencyOutcomes: readonly string[]
      try {
        const updateConcurrency = await Promise.allSettled([
          repository.stageCheckpointUpdate(updatePredecessor, updateLeft),
          updateContender.stageCheckpointUpdate(updatePredecessor, updateRight),
        ])
        updateConcurrencyOutcomes = Object.freeze(updateConcurrency.map((result) =>
          result.status === 'fulfilled' ? 'resolved' : errorName(result.reason)).sort())
      } finally {
        updateContender.close()
      }
      const stagedUpdateRows = (await repository.scanCandidates({
        direction: 'ascending',
        fileId: updatePredecessor.fileId,
      })).records
      const updateConcurrencyCandidateRows = stagedUpdateRows.length
      const stagedUpdate = stagedUpdateRows[0]
      if (stagedUpdate === undefined) {
        throw new Error('concurrent update staging did not retain its selected candidate')
      }
      await repository.commitCheckpointCandidate(stagedUpdate, checkpointTransition(stagedUpdate, {
        commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
      }))

      const revisionRecord = await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x12),
        fileRevision: identity(16, 0x22),
        canonicalPath: ['revision.bin'],
        ownedObjectId: identity(32, 0x33),
      })
      const revisionDecision = await repository.lookupLineage(lineageRequest(
        binding,
        revisionRecord,
        { fileRevision: identity(16, 0x23) },
      ))

      const ownershipSpec = {
        fileId: identity(16, 0x13),
        fileRevision: identity(16, 0x24),
        canonicalPath: ['ownership.bin'],
        exactSize: 8n,
      } as const
      const ownershipLeft = await seedCommitted(databaseName, binding, {
        ...ownershipSpec,
        ownedObjectId: identity(32, 0x34),
      })
      await seedCommitted(databaseName, binding, {
        ...ownershipSpec,
        ownedObjectId: identity(32, 0x35),
      })
      const ownershipDecision = await repository.lookupLineage(
        lineageRequest(binding, ownershipLeft),
      )

      const invalidRecord = await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x14),
        fileRevision: identity(16, 0x25),
        canonicalPath: ['invalid.bin'],
        exactSize: 9n,
        ownedObjectId: identity(32, 0x36),
      })
      const invalidDecision = await repository.lookupLineage(lineageRequest(
        binding,
        invalidRecord,
        { exactSize: 10n },
      ))

      const sharedObject = identity(32, 0x37)
      const crossLineageRecord = await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x15),
        fileRevision: identity(16, 0x26),
        canonicalPath: ['cross-one.bin'],
        ownedObjectId: sharedObject,
      })
      await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x16),
        fileRevision: identity(16, 0x27),
        canonicalPath: ['cross-two.bin'],
        ownedObjectId: sharedObject,
      })
      const crossLineageOwnershipDecision = await repository.lookupLineage(
        lineageRequest(binding, crossLineageRecord),
      )

      const unresolvedCommitted = await seedCommitted(databaseName, binding, {
        fileId: identity(16, 0x17),
        fileRevision: identity(16, 0x28),
        canonicalPath: ['unresolved.bin'],
        exactSize: 8n,
        ownedObjectId: identity(32, 0x38),
      })
      const unresolvedCandidate = checkpointTransition(unresolvedCommitted, {
        checkpointGeneration: 1n,
        verifiedRanges: [{ start: 0n, end: 1n }],
        commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
      })
      await repository.stageCheckpointUpdate(unresolvedCommitted, unresolvedCandidate)
      const unresolvedCandidateRejection = await rejectionName(
        repository.lookupLineage(lineageRequest(binding, unresolvedCandidate)),
      )
      const resolved = checkpointTransition(unresolvedCandidate, {
        commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
      })
      await repository.resolveCandidate(unresolvedCandidate, {
        kind: 'verified',
        committed: resolved,
      })
      const resolvedDecision = await repository.lookupLineage(
        lineageRequest(binding, unresolvedCandidate),
      )
      if (resolvedDecision.kind !== 'exact') {
        throw new Error('resolved checkpoint did not recover exact lineage authority')
      }

      return Object.freeze({
        putCandidateSurfacePresent,
        unbackedUpdateRejection,
        unbackedUpdateCandidateRows,
        updateConcurrencyOutcomes,
        updateConcurrencyCandidateRows,
        concurrentKinds,
        concurrentObjectConverged,
        candidateRowsBeforeResolution,
        candidateBeforeObjectDecision: candidateBeforeObject.kind,
        candidateRowsAfterResolution,
        resolutionReplayDecision: resolutionReplay.kind,
        revisionDecision: revisionDecision.kind,
        ownershipDecision: ownershipDecision.kind,
        invalidDecision: invalidDecision.kind,
        crossLineageOwnershipDecision: crossLineageOwnershipDecision.kind,
        unresolvedCandidateRejection,
        resolvedRange: resolvedDecision.record.verifiedRanges
          .map((range) => `${range.start}:${range.end}`).join(','),
      })
    } finally {
      repository.close()
    }
  } finally {
    first.close()
    second.close()
    await deleteDatabase(databaseName)
  }
}

async function probeBlockedUpgrade(): Promise<{
  readonly rejection: string
  readonly deleted: boolean
}> {
  const databaseName = `w3c-v6-blocked-${crypto.randomUUID()}`
  const blocker = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION - 1, (database) => {
    database.createObjectStore(LEGACY_PAUSED_STORE, { keyPath: 'id' })
  })
  const rejection = await rejectionName(IndexedDbReceiveOperationRepository.open(databaseName))
  blocker.close()
  return Object.freeze({ rejection, deleted: await deleteDatabase(databaseName) })
}

async function probeVersionChange(): Promise<string> {
  const databaseName = `w3c-v6-versionchange-${crypto.randomUUID()}`
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const upgrader = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION + 1)
  const rejection = await rejectionName(repository.listRecords(identity(16, 0x31)))
  repository.close()
  upgrader.close()
  await deleteDatabase(databaseName)
  return rejection
}

async function probeReplacement(): Promise<Pick<
  IndexedDbV6Probe,
  | 'schemaVersion'
  | 'v6StoresPresent'
  | 'legacyStoreRetainedForCleanup'
  | 'legacyRowsVisibleToV6'
>> {
  const databaseName = `w3c-v6-replacement-${crypto.randomUUID()}`
  const operationId = identity(16, 0x41)
  const legacy = await openRawDatabase(
    databaseName,
    CHECKPOINT_DATABASE_VERSION - 1,
    (database, transaction) => {
      const store = database.createObjectStore(LEGACY_PAUSED_STORE, { keyPath: 'id' })
      transaction.addEventListener('complete', () => undefined, { once: true })
      store.put({ id: 'obsolete-pause', operationId })
    },
  )
  legacy.close()
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const raw = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION)
  const names = new Set(Array.from(raw.objectStoreNames))
  const result = Object.freeze({
    schemaVersion: raw.version,
    v6StoresPresent: INDEXEDDB_V6_STORE_SCHEMAS.every((schema) => names.has(schema.name)),
    legacyStoreRetainedForCleanup: names.has(LEGACY_PAUSED_STORE),
    legacyRowsVisibleToV6: (await repository.listRecords(operationId)).length !== 0,
  })
  raw.close()
  repository.close()
  await deleteDatabase(databaseName)
  return result
}

function openRawDatabase(
  name: string,
  version: number,
  upgrade?: (database: IDBDatabase, transaction: IDBTransaction) => void,
): Promise<IDBDatabase> {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(name, version)
    request.addEventListener('upgradeneeded', () => {
      const transaction = request.transaction
      if (transaction === null) throw new Error('IndexedDB upgrade lacks a transaction')
      upgrade?.(request.result, transaction)
    }, { once: true })
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

async function rejectionName(operation: Promise<unknown>): Promise<string> {
  try {
    await operation
    return 'resolved'
  } catch (error) {
    return errorName(error)
  }
}

function errorName(error: unknown): string {
  if (error instanceof DOMException || error instanceof Error) return error.name
  return String(error)
}

function deleteDatabase(name: string): Promise<boolean> {
  return new Promise<boolean>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(true), { once: true })
    request.addEventListener('blocked', () => resolve(false), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function checkpointBinding(): DurableCheckpointNamespaceIdentity {
  return durableCheckpointNamespaceIdentity({
    operationId: identity(16, 0x41),
    receiveIntentDigest: identity(32, 0x42),
    materializationBindingDigest: identity(32, 0x43),
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 0x44),
  })
}

function checkpoint(
  binding: DurableCheckpointNamespaceIdentity,
  input: Readonly<{
    fileId: string
    fileRevision: string
    canonicalPath: readonly string[]
    ownedObjectId: string
    exactSize?: bigint
  }>,
): FileCheckpointV2 {
  return newFileCheckpointV2({
    ...binding,
    fileId: input.fileId,
    fileRevision: input.fileRevision,
    canonicalPath: input.canonicalPath,
    exactSize: input.exactSize ?? 8n,
    ownedObjectId: input.ownedObjectId,
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
  })
}

function checkpointTransition(
  record: FileCheckpointV2,
  overrides: Partial<FileCheckpointSpec>,
): FileCheckpointV2 {
  return newFileCheckpointV2({
    ...record,
    ...overrides,
  })
}

async function seedCommitted(
  databaseName: string,
  binding: DurableCheckpointNamespaceIdentity,
  input: Parameters<typeof checkpoint>[1],
): Promise<FileCheckpointV2> {
  const candidate = checkpoint(binding, input)
  const committed = checkpointTransition(candidate, {
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  const database = await openRawDatabase(databaseName, CHECKPOINT_DATABASE_VERSION)
  try {
    const transaction = database.transaction(
      INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE,
      'readwrite',
    )
    transaction.objectStore(INDEXEDDB_FILE_CHECKPOINT_COMMITTED_STORE).put({
      id: committed.recordId,
      operationId: committed.operationId,
      fileId: committed.fileId,
      envelope: encodeFileCheckpointV2(committed),
    })
    await transactionCompletion(transaction)
  } finally {
    database.close()
  }
  return committed
}

function lineageRequest(
  binding: DurableCheckpointNamespaceIdentity,
  record: FileCheckpointV2,
  overrides: Readonly<{ fileRevision?: string; exactSize?: bigint }> = {},
) {
  return Object.freeze({
    lineageId: deriveCheckpointLineageID({
      ...binding,
      fileId: record.fileId,
      canonicalPath: record.canonicalPath,
    }),
    fileId: record.fileId,
    canonicalPath: record.canonicalPath,
    fileRevision: overrides.fileRevision ?? record.fileRevision,
    exactSize: overrides.exactSize ?? record.exactSize,
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
