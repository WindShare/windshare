import { encodeBase64Url } from '../../../crypto/bytes'
import { sha256 } from '../../../crypto/digest'
import {
  outputRecordKey,
  sameOutputRecord,
  type OutputCheckpointJournal,
  type OutputJournalScan,
  type PersistedOutputRecord,
} from '../../persistence/journal'
import {
  pausedTaskDescriptorKey,
  pausedTaskDescriptorNamespace,
  type PausedTaskDescriptorV1,
} from '../../resume/descriptor'
import {
  observePausedTask,
  type PausedTaskTraceListener,
} from '../../resume/authority'
import { FILE_SYSTEM_ACCESS_BACKEND } from '../../capability/contract'
import { IndexedDbOutputRepository } from '../indexeddb-repository'
import {
  completedFiles,
  readPreparedCapability,
  type IndexedDbResumeStateDiscardMarker,
  type StoredRootCapability,
} from './records'

const MAXIMUM_PINNED_CHECKPOINT_RECORDS = 1_000_000

export interface BrowserResumeStatePin {
  readonly descriptor: PausedTaskDescriptorV1
  readonly capability: StoredRootCapability
  readonly committed: readonly PersistedOutputRecord[]
  readonly candidates: readonly PersistedOutputRecord[]
  readonly inventoryDigest: string
  readonly discardMarker?: IndexedDbResumeStateDiscardMarker
}

export async function prepareResumeStateInventory(
  databaseName: string,
  database: IDBDatabase,
  descriptors: readonly PausedTaskDescriptorV1[],
  onTrace?: PausedTaskTraceListener,
): Promise<readonly BrowserResumeStatePin[]> {
  const pins: BrowserResumeStatePin[] = []
  for (const descriptor of descriptors) {
    const capability = await readPreparedCapability(database, descriptor)
    const repository = await IndexedDbOutputRepository.openExisting(
      databaseName,
      pausedTaskDescriptorNamespace(descriptor),
    )
    try {
      const [committed, candidates, discardMarker] = await Promise.all([
        scanAllRecords((scan) => repository.scanCommitted(scan)),
        scanAllRecords((scan) => repository.scanCandidates(scan)),
        repository.readResumeStateDiscard(),
      ])
      const inventoryDigest = await resumeStateInventoryDigest(committed, candidates)
      pins.push(Object.freeze({
        descriptor,
        capability,
        committed,
        candidates,
        inventoryDigest,
        ...(discardMarker === undefined ? {} : { discardMarker }),
      }))
      observePausedTask(
        onTrace,
        'paused-task-resume-prepared',
        descriptor,
        { decision: 'descriptor-capability-and-journal-pinned' },
      )
    } finally {
      repository.close()
    }
  }
  return Object.freeze(pins)
}

export async function scanAllRecords(
  scanPage: (scan: OutputJournalScan) => ReturnType<OutputCheckpointJournal['scanCommitted']>,
): Promise<readonly PersistedOutputRecord[]> {
  const records: PersistedOutputRecord[] = []
  let cursor: string | undefined
  do {
    const page = await scanPage({
      direction: 'ascending',
      ...(cursor === undefined ? {} : { cursor }),
    })
    records.push(...page.records)
    if (records.length > MAXIMUM_PINNED_CHECKPOINT_RECORDS) {
      throw new DOMException(
        'Paused task checkpoint inventory exceeds its bound',
        'QuotaExceededError',
      )
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)
  return Object.freeze(records)
}

export async function resumeStateInventoryDigest(
  committed: readonly PersistedOutputRecord[],
  candidates: readonly PersistedOutputRecord[],
): Promise<string> {
  const entries = [
    ...committed.map((record) => resumeStateInventoryEntry('committed', record)),
    ...candidates.map((record) => resumeStateInventoryEntry('candidate', record)),
  ]
  const bytes = new TextEncoder().encode(JSON.stringify(entries))
  return encodeBase64Url(await sha256(bytes))
}

function resumeStateInventoryEntry(
  store: 'committed' | 'candidate',
  record: PersistedOutputRecord,
): readonly string[] {
  return Object.freeze([
    store,
    outputRecordKey(record),
    record.recordId,
    record.generation.toString(),
    record.checksum,
  ])
}

export function sameOptionalDiscardMarker(
  left: IndexedDbResumeStateDiscardMarker | undefined,
  right: IndexedDbResumeStateDiscardMarker | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.descriptorKey === right.descriptorKey &&
    left.rootCapabilityRef === right.rootCapabilityRef &&
    left.backend === right.backend &&
    left.inventoryDigest === right.inventoryDigest &&
    left.phase === right.phase
}

export function discardMarkerMatchesPin(
  marker: IndexedDbResumeStateDiscardMarker,
  descriptor: PausedTaskDescriptorV1,
  inventoryDigest: string,
): boolean {
  return marker.descriptorKey === pausedTaskDescriptorKey(descriptor) &&
    marker.rootCapabilityRef === descriptor.rootCapabilityRef &&
    marker.backend === descriptor.intent.output.backend &&
    marker.inventoryDigest === inventoryDigest
}

export function discardMarkerPhaseMatchesTask(
  marker: IndexedDbResumeStateDiscardMarker,
  descriptor: PausedTaskDescriptorV1,
  committed: readonly PersistedOutputRecord[],
): boolean {
  if (descriptor.intent.output.backend === FILE_SYSTEM_ACCESS_BACKEND) {
    return marker.phase === 'retiring'
  }
  const requiresExport = completedFiles(committed).length > 0
  return requiresExport
    ? marker.phase === 'exporting' || marker.phase === 'exported'
    : marker.phase === 'retiring'
}

export function sameRecordInventory(
  current: readonly PersistedOutputRecord[],
  pinned: readonly PersistedOutputRecord[],
): boolean {
  return current.length === pinned.length && current.every((record, index) => {
    const expected = pinned[index]
    return expected !== undefined &&
      outputRecordKey(record) === outputRecordKey(expected) &&
      sameOutputRecord(record, expected)
  })
}
