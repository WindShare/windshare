import { IndexedDbOutputRepository } from '../../src/output/browser/indexeddb-repository'
import { IndexedDbOriginPrivateAdmissionAuthority } from '../../src/output/origin-private/admission-authority'
import { IndexedDbZipCentralDirectorySpool } from '../../src/output/streams/zip-spool'

const ZIP_FLUSH_BYTES = 256 * 1024

export interface IndexedDbFailureBoundaryProbe {
  readonly journalBlocked: string
  readonly journalLateConnectionClosed: boolean
  readonly journalVersionChange: string
  readonly admissionVersionChange: string
  readonly zipBlocked: string
  readonly zipLateConnectionClosed: boolean
  readonly zipVersionChange: string
}

export async function probeIndexedDbFailureBoundaries(): Promise<IndexedDbFailureBoundaryProbe> {
  const journalBlocked = await probeBlockedJournal()
  const journalVersionChange = await probeJournalVersionChange()
  const admissionVersionChange = await probeAdmissionVersionChange()
  const zipBlocked = await probeBlockedZipSpool()
  const zipVersionChange = await probeZipVersionChange()
  return {
    journalBlocked: journalBlocked.rejection,
    journalLateConnectionClosed: journalBlocked.deleted,
    journalVersionChange,
    admissionVersionChange,
    zipBlocked: zipBlocked.rejection,
    zipLateConnectionClosed: zipBlocked.deleted,
    zipVersionChange,
  }
}

async function probeAdmissionVersionChange(): Promise<string> {
  const databaseName = `r8-admission-versionchange-${crypto.randomUUID()}`
  const authority = await IndexedDbOriginPrivateAdmissionAuthority.open(databaseName)
  const record = {
    id: 'obsolete-admission',
    token: 'obsolete-token',
    logicalBytes: 0n,
    additionalBytes: 0n,
    expiresAtMilliseconds: 1_000,
  }
  const limits = {
    jobLimit: 10n,
    processLimit: 10n,
    quota: 10n,
    usage: 0n,
    reserve: 0n,
    nowMilliseconds: 0,
  }
  await authority.claim(record, limits)
  const upgrader = await openRawDatabase(databaseName, 2)
  const rejection = await rejectionName(authority.update(record, limits))
  authority.close()
  upgrader.close()
  if (!await deleteDatabase(databaseName)) throw new Error('Admission versionchange leaked a connection')
  return rejection
}

async function probeBlockedJournal(): Promise<{
  readonly rejection: string
  readonly deleted: boolean
}> {
  const databaseName = `r8-journal-blocked-${crypto.randomUUID()}`
  const blocker = await openRawDatabase(databaseName, 1)
  const rejection = await rejectionName(
    IndexedDbOutputRepository.open(databaseName, 'blocked', 'journal'),
  )
  blocker.close()
  return { rejection, deleted: await deleteDatabase(databaseName) }
}

async function probeJournalVersionChange(): Promise<string> {
  const databaseName = `r8-journal-versionchange-${crypto.randomUUID()}`
  const repository = await IndexedDbOutputRepository.open(databaseName, 'obsolete', 'journal')
  const upgrader = await openRawDatabase(databaseName, 3)
  const rejection = await rejectionName(repository.scanCommitted({ direction: 'ascending' }))
  upgrader.close()
  if (!await deleteDatabase(databaseName)) throw new Error('Journal versionchange leaked a connection')
  return rejection
}

async function probeBlockedZipSpool(): Promise<{
  readonly rejection: string
  readonly deleted: boolean
}> {
  const databaseName = `r8-zip-blocked-${crypto.randomUUID()}`
  const blocker = await openRawDatabase(databaseName, 1)
  const spool = new IndexedDbZipCentralDirectorySpool({ databaseName })
  const rejection = await rejectionName(spool.append(new Uint8Array(ZIP_FLUSH_BYTES)))
  blocker.close()
  return { rejection, deleted: await deleteDatabase(databaseName) }
}

async function probeZipVersionChange(): Promise<string> {
  const databaseName = `r8-zip-versionchange-${crypto.randomUUID()}`
  const spool = new IndexedDbZipCentralDirectorySpool({ databaseName })
  await spool.append(new Uint8Array(ZIP_FLUSH_BYTES))
  const upgrader = await openRawDatabase(databaseName, 4)
  const rejection = await rejectionName(spool.append(Uint8Array.of(1)))
  await spool.clear().catch(() => undefined)
  upgrader.close()
  if (!await deleteDatabase(databaseName)) throw new Error('ZIP versionchange leaked a connection')
  return rejection
}

function openRawDatabase(name: string, version: number): Promise<IDBDatabase> {
  return requestResult(indexedDB.open(name, version))
}

async function rejectionName(operation: Promise<unknown>): Promise<string> {
  try {
    await operation
    return 'resolved'
  } catch (error) {
    if (error instanceof DOMException || error instanceof Error) return error.name
    return String(error)
  }
}

function deleteDatabase(name: string): Promise<boolean> {
  return new Promise<boolean>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.addEventListener('success', () => resolve(true), { once: true })
    request.addEventListener('blocked', () => resolve(false), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
  })
}
