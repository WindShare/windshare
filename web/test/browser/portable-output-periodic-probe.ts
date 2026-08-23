import { encodeBase64Url } from '../../src/crypto/bytes'
import { StreamingZipArchiveWriter } from '../../src/output/streams/streaming-zip'
import { IndexedDbZipCentralDirectorySpool } from '../../src/output/streams/zip-spool'
import { ZipLayoutLedgerV1 } from '../../src/output/zip-layout/layout'

const MILLION_MEMBER_COUNT = 1_000_000
const RESULT_ROOT_NAME = 'result'
const MEMBER_NAME_WIDTH = (MILLION_MEMBER_COUNT - 2).toString(36).length
const DIGEST_BYTES = 32
const RECEIVE_INTENT_DIGEST_SEED = 1
const ARTIFACT_DIGEST_SEED = 2
const DISCOVERY_LEDGER_DIGEST_SEED = 3
const PROGRESS_ENTRY_INTERVAL = 100_000
const PROGRESS_EVENT_KIND = 'weekly-million-member-zip-progress'

export interface MillionMemberZipProbe {
  readonly memberCount: number
  readonly outputBytes: number
  readonly outputWrites: number
  readonly maximumWriteBytes: number
  readonly outputWritesBeforeClose: number
  readonly entryStreamBytes: number
  readonly closed: boolean
  readonly beforeClose: readonly number[]
  readonly afterClose: readonly number[]
}

export async function probeMillionMemberZipWriter(): Promise<MillionMemberZipProbe> {
  const startedAtMilliseconds = performance.now()
  const databaseName = `million-writer-${crypto.randomUUID()}`
  let outputBytes = 0
  let outputWrites = 0
  let maximumWriteBytes = 0
  let closed = false
  const output = new WritableStream<Uint8Array>({
    write(chunk) {
      outputBytes += chunk.byteLength
      outputWrites += 1
      maximumWriteBytes = Math.max(maximumWriteBytes, chunk.byteLength)
    },
    close() { closed = true },
  })
  const ledger = new ZipLayoutLedgerV1(
    probeDigest(RECEIVE_INTENT_DIGEST_SEED),
    probeDigest(ARTIFACT_DIGEST_SEED),
  )
  const archive = new StreamingZipArchiveWriter(
    output,
    new IndexedDbZipCentralDirectorySpool({ databaseName }),
    { kind: 'progressive', ledger },
  )
  const root = ledger.append({ kind: 'directory', path: [RESULT_ROOT_NAME] })
  await archive.addDirectory(root)
  for (let index = 0; index < MILLION_MEMBER_COUNT - 1; index += 1) {
    const name = `f${index.toString(36).padStart(MEMBER_NAME_WIDTH, '0')}`
    const plan = ledger.append({
      kind: 'file',
      path: [RESULT_ROOT_NAME, name],
      exactSize: 0n,
    })
    const member = await archive.beginFile(plan)
    await member.close()
    const completedEntries = index + 2
    if (completedEntries % PROGRESS_ENTRY_INTERVAL === 0) {
      reportProgress('entries', completedEntries, startedAtMilliseconds)
    }
  }
  reportProgress('discovery-complete', MILLION_MEMBER_COUNT, startedAtMilliseconds)
  ledger.completeDiscovery(probeDigest(DISCOVERY_LEDGER_DIGEST_SEED))
  const sealedPlan = await ledger.seal()
  reportProgress('layout-sealed', MILLION_MEMBER_COUNT, startedAtMilliseconds)
  const beforeClose = await countStores(databaseName)
  const outputWritesBeforeClose = outputWrites
  await archive.close(sealedPlan, new AbortController().signal)
  reportProgress('archive-closed', MILLION_MEMBER_COUNT, startedAtMilliseconds)
  const afterClose = await countStores(databaseName)
  await deleteDatabase(databaseName)
  return {
    memberCount: Number(sealedPlan.entryCount),
    outputBytes,
    outputWrites,
    maximumWriteBytes,
    outputWritesBeforeClose,
    entryStreamBytes: Number(sealedPlan.centralDirectoryOffset),
    closed,
    beforeClose,
    afterClose,
  }
}

function reportProgress(
  phase: 'entries' | 'discovery-complete' | 'layout-sealed' | 'archive-closed',
  entryCount: number,
  startedAtMilliseconds: number,
): void {
  console.info(JSON.stringify({
    kind: PROGRESS_EVENT_KIND,
    phase,
    entryCount,
    elapsedMilliseconds: Math.round(performance.now() - startedAtMilliseconds),
  }))
}

function probeDigest(seed: number): string {
  const bytes = new Uint8Array(DIGEST_BYTES)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}

async function countStores(name: string): Promise<readonly number[]> {
  const database = await openDatabase(name)
  const transaction = database.transaction(
    ['central-directory-chunks', 'central-directory-namespaces'],
    'readonly',
  )
  const counts = await Promise.all([
    requestCount(transaction.objectStore('central-directory-chunks')),
    requestCount(transaction.objectStore('central-directory-namespaces')),
  ])
  await transactionDone(transaction)
  database.close()
  return counts
}

function openDatabase(name: string): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(name)
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function requestCount(store: IDBObjectStore): Promise<number> {
  return new Promise((resolve, reject) => {
    const request = store.count()
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function transactionDone(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve(), { once: true })
    transaction.addEventListener('error', () => reject(transaction.error), { once: true })
    transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
  })
}

function deleteDatabase(name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('blocked', () => {
      reject(new Error('Million-member spool left an IndexedDB connection open'))
    }, { once: true })
  })
}
