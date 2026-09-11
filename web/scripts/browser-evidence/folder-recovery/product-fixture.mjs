import { encodeBase64Url } from '../../../src/crypto/bytes.ts'
import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository.ts'
import { reopenFileSystemAccessOutput } from '../../../src/output/file-system-access/session.ts'
import { bindTask, resultRootArtifact } from '../../../test/browser/fsa-namespace-atomicity-harness.ts'
import { openCase } from './browser-storage.mjs'

export function identity(seed) { return encodeBase64Url(new Uint8Array(16).fill(seed)) }

async function manifestDatabase(caseId) {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(`${caseId}-fixture`, 1)
    request.onupgradeneeded = () => request.result.createObjectStore('fixture')
    request.onerror = () => reject(request.error)
    request.onsuccess = () => resolve(request.result)
  })
}

export async function saveFixture(caseId, value) {
  const db = await manifestDatabase(caseId)
  try {
    await new Promise((resolve, reject) => {
      const tx = db.transaction('fixture', 'readwrite')
      tx.objectStore('fixture').put(value, 'current')
      tx.oncomplete = resolve
      tx.onerror = () => reject(tx.error)
      tx.onabort = () => reject(tx.error ?? new Error('Fixture persistence aborted'))
    })
  } finally { db.close() }
}

export async function loadFixture(caseId) {
  const db = await manifestDatabase(caseId)
  try {
    return await new Promise((resolve, reject) => {
      const request = db.transaction('fixture').objectStore('fixture').get('current')
      request.onsuccess = () => resolve(request.result)
      request.onerror = () => reject(request.error)
    })
  } finally { db.close() }
}

export async function openProductTarget(input, reopen) {
  const tree = await openCase(input.caseId, !reopen)
  const fixture = reopen ? await loadFixture(input.caseId) : {
    databaseName: `${input.caseId}-output`, parentName: input.caseId, input,
  }
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const target = reopen
    ? await reopenFileSystemAccessOutput({ intent: fixture.intent, operationRepository: repository, databaseName: fixture.databaseName })
    : await bindTask(fixture, tree.target, repository, await resultRootArtifact(), 10)
  const saved = { ...fixture, intent: target.intent }
  await saveFixture(input.caseId, saved)
  const payloadTarget = await tree.target.getDirectoryHandle(target.reservation.physicalName)
  return { tree, fixture: saved, repository, target, payloadTarget }
}

export async function deleteFixtureDatabase(name) {
  await new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = resolve
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error(`Evidence database still open: ${name}`))
  })
}
