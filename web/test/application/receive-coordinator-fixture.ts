import type { BrowseDirectory } from '../../src/application/browser-share/gateway'
import type { ReceiveRuntimeSnapshot } from '../../src/application/receive/contracts'
import type { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'

const SNAPSHOT_WAIT_TIMEOUT_MS = 2_000

export function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

/**
 * Event-driven snapshot wait backed by `ReceiveCoordinator.subscribe()`, avoiding
 * default 50ms `vi.waitFor` timer polling in fast unit tests.
 */
export function waitForSnapshot(
  coordinator: ReceiveCoordinator,
  predicate: (snapshot: ReceiveRuntimeSnapshot) => boolean,
): Promise<ReceiveRuntimeSnapshot> {
  const current = coordinator.getSnapshot()
  if (predicate(current)) return Promise.resolve(current)
  const timeoutError = new Error('Timed out waiting for ReceiveCoordinator snapshot')
  return new Promise<ReceiveRuntimeSnapshot>((resolve, reject) => {
    const timer = setTimeout(() => {
      unsubscribe()
      const last = coordinator.getSnapshot()
      timeoutError.message = `Timed out waiting for ReceiveCoordinator snapshot (lifecycle=${last.lifecycle?.kind ?? 'null'}, activeTaskId=${last.activeTaskId}, activeSingleFileId=${last.activeSingleFileId}, activation=${last.activation?.kind ?? 'null'}, pending=${last.retained.pending?.action ?? 'null'}, offers=${last.offers?.kind ?? 'null'})`
      reject(timeoutError)
    }, SNAPSHOT_WAIT_TIMEOUT_MS)
    const unsubscribe = coordinator.subscribe(() => {
      const next = coordinator.getSnapshot()
      if (!predicate(next)) return
      clearTimeout(timer)
      unsubscribe()
      resolve(next)
    })
  })
}

export function makeFileEntry(
  seed: number,
  name: string,
  expectedSize: bigint,
): Extract<CatalogEntry, { kind: 'file' }> {
  const id = new Uint8Array(16).fill(seed)
  return Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize })
}

export function makeSubDirectory(rootId: string, dirSeed: number, name: string): BrowseDirectory {
  const id = new Uint8Array(16).fill(dirSeed)
  const idText = encodeBase64Url(id)
  return Object.freeze({
    id,
    idText,
    path: Object.freeze([name]),
    ancestry: Object.freeze([rootId, idText]),
  })
}

export function singleFileSelectionKey(
  shareInstance: string,
  syntheticRoot: string,
  entry: Extract<CatalogEntry, { kind: 'file' }>,
  directory: BrowseDirectory,
) {
  const policy = new SelectionPolicy(false)
  policy.set(entry, directory.ancestry, true)
  const spec = selectionSpecFromPolicy(shareInstance, syntheticRoot, policy.snapshot())
  return { spec, key: selectionKey(spec) }
}

export async function commitMockOutputFile(
  output: TransferOutputSession,
  file: Readonly<{ readonly idText: string; readonly name: string }>,
): Promise<void> {
  const writer = await output.openFile({
    fileId: file.idText,
    path: [file.name],
    sourcePath: [file.name],
    revision: 'mock-rev',
    exactSize: 0n,
  }, new AbortController().signal)
  await writer.commit([])
}

export function makeJobResult(status: 'completed' | 'paused' = 'completed', size = 16n) {
  return status === 'paused'
    ? {
        status: 'paused' as const,
        pauseReason: 'user' as const,
        completedFiles: 0n,
        completedBytes: 0n,
        discoveredFiles: 1n,
        discoveredBytes: size,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      }
    : {
        status: 'completed' as const,
        completedFiles: 1n,
        completedBytes: size,
        discoveredFiles: 1n,
        discoveredBytes: size,
        discoveryComplete: true,
        failedFiles: 0n,
        failedDirectories: 0n,
      }
}
