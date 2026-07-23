import { StreamingZipArchiveWriter } from '../../src/output/streams/streaming-zip'
import { IndexedDbZipCentralDirectorySpool } from '../../src/output/streams/zip-spool'

const MILLION_MEMBER_COUNT = 1_000_000

export interface MillionMemberZipProbe {
  readonly memberCount: number
  readonly outputBytes: number
  readonly outputWrites: number
  readonly maximumWriteBytes: number
  readonly closed: boolean
  readonly beforeClose: readonly number[]
  readonly afterClose: readonly number[]
}

export async function probeMillionMemberZipWriter(): Promise<MillionMemberZipProbe> {
  const databaseName = `r8-million-writer-${crypto.randomUUID()}`
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
  const archive = new StreamingZipArchiveWriter(
    output,
    new IndexedDbZipCentralDirectorySpool({ databaseName }),
  )
  for (let index = 0; index < MILLION_MEMBER_COUNT; index += 1) {
    const member = await archive.beginFile({
      path: [`f${index.toString(36)}`],
      exactSize: 0n,
    })
    await member.close()
  }
  const beforeClose = await countStores(databaseName)
  await archive.close(new AbortController().signal)
  const afterClose = await countStores(databaseName)
  await deleteDatabase(databaseName)
  return {
    memberCount: MILLION_MEMBER_COUNT,
    outputBytes,
    outputWrites,
    maximumWriteBytes,
    closed,
    beforeClose,
    afterClose,
  }
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
