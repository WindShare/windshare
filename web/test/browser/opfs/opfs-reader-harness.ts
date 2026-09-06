import { acquireArtifactReader, withArtifactCleanup, type ArtifactReaderLease } from '../../../src/output/origin-private/export-readers'
import { openNativeObject } from '../../../src/output/origin-private/native-object/client'

let reader: ArtifactReaderLease | undefined
let cleanup: Promise<void> | undefined
let cleanupFinished = false

export async function retainArtifactReader(operationId: string): Promise<void> {
  const root = await navigator.storage.getDirectory()
  const handle = await root.getFileHandle(operationId, { create: true })
  const writer = await openNativeObject(handle)
  await writer.writeAt(0n, Uint8Array.of(1, 2, 3))
  await writer.flush()
  await writer.close()
  reader = await acquireArtifactReader(operationId)
}

export function beginArtifactCleanup(operationId: string): void {
  cleanupFinished = false
  cleanup = withArtifactCleanup(operationId, async () => {
    const root = await navigator.storage.getDirectory()
    await root.removeEntry(operationId)
    cleanupFinished = true
  })
}

export async function observeArtifactCleanup(operationId: string) {
  const locks = await navigator.locks.query()
  const root = await navigator.storage.getDirectory()
  const file = await (await root.getFileHandle(operationId)).getFile()
  return {
    pendingCleanup: locks.pending?.some(lock => lock.name === `windshare/artifact-readers/${operationId}`) ?? false,
    cleanupFinished,
    bytes: [...new Uint8Array(await file.arrayBuffer())],
  }
}

export function releaseArtifactReader(): void {
  reader?.release()
  reader = undefined
}

export async function finishArtifactCleanup(operationId: string): Promise<boolean> {
  await cleanup
  const root = await navigator.storage.getDirectory()
  try { await root.getFileHandle(operationId); return false }
  catch (error) {
    if (error instanceof DOMException && error.name === 'NotFoundError') return cleanupFinished
    throw error
  }
}
