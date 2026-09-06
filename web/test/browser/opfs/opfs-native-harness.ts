export { probeNativeObjectSupport } from '../../../src/output/origin-private/native-object/support'
import { openNativeObject } from '../../../src/output/origin-private/native-object/client'
import { ObjectCheckpointCoordinator } from '../../../src/output/origin-private/native-object/coordinator'
import { IndexedDbTaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/indexeddb-store'
import type { TaskObjectRef } from '../../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/store'
import { ProgressiveZipArchive } from '../../../src/output/progressive-zip/archive'

const OBJECT_NAME = 'archive.bin'
const METADATA_COMMIT_FAILURE = 'injected metadata commit failure'
const Z_BYTES = Uint8Array.of(0, 1, 2, 3, 4)
const A_BYTES = Uint8Array.of(9, 8, 7)

export interface NativeArchiveFixture {
  readonly directoryName: string
  readonly databaseName: string
  readonly object: TaskObjectRef
}

async function fileHandle(fixture: NativeArchiveFixture): Promise<FileSystemFileHandle> {
  const root = await navigator.storage.getDirectory()
  const directory = await root.getDirectoryHandle(fixture.directoryName, { create: true })
  return directory.getFileHandle(OBJECT_NAME, { create: true })
}

async function openArchive(fixture: NativeArchiveFixture) {
  const handle = await fileHandle(fixture)
  const store = await IndexedDbTaskCheckpointStore.open(fixture.object, fixture.databaseName)
  const io = await openNativeObject(handle)
  const traces: string[] = []
  const coordinator = new ObjectCheckpointCoordinator({
    io, ...fixture.object, trace: event => traces.push(event.stage),
  })
  let rejectCommit = false
  const faultableStore: TaskCheckpointStore = {
    readCheckpoint: () => store.readCheckpoint(),
    readEntry: entryId => store.readEntry(entryId),
    readPath: path => store.readPath(path),
    readDirectoryPin: directoryId => store.readDirectoryPin(directoryId),
    readEntries: input => store.readEntries(input),
    commit: input => rejectCommit
      ? Promise.reject(new Error(METADATA_COMMIT_FAILURE))
      : store.commit(input),
    close: () => store.close(),
  }
  const growth: string[] = []
  const archive = await ProgressiveZipArchive.open({
    store: faultableStore, coordinator, object: fixture.object, selectedPaths: [[]],
    // Quota contention has its own authority tests; this records the exact
    // logical growth requested by the real archive, including unfilled gaps.
    capacity: {
      async reserveGrowth(input) {
        if (input.targetLength > input.currentLength) {
          growth.push((input.targetLength - input.currentLength).toString())
        }
        return { async settle() {}, async release() {} }
      },
    },
  })
  return { fixture, handle, store, archive, traces, growth,
    failNextCommit: () => { rejectCommit = true } }
}

type ArchiveSession = Awaited<ReturnType<typeof openArchive>>
let session: ArchiveSession | undefined

export async function createNativeArchiveCut(key: string) {
  const fixture: NativeArchiveFixture = {
    directoryName: `windshare-native-${key}`,
    databaseName: `windshare-native-${key}`,
    object: { operationId: key, objectId: OBJECT_NAME, kind: 'zip-archive', handleId: OBJECT_NAME },
  }
  session = await openArchive(fixture)
  const { archive, store } = session
  const source = { shareInstance: 'share', directoryId: 'directory', generation: 'generation', sourcePath: [] }
  const first = await archive.admitFile({
    entryId: 'z', path: ['z.bin'], source,
    revision: { fileId: 'z', fileRevision: 'z-revision', exactSize: BigInt(Z_BYTES.length) },
  })
  await Promise.all([
    archive.writeRange('z', 3n, Z_BYTES.subarray(3)),
    archive.writeRange('z', 0n, Z_BYTES.subarray(0, 2)),
  ])
  await archive.checkpoint('browser-progress-before-discovery')
  const beforeLaterDiscovery = await store.readEntry('z')
  await archive.admitFile({
    entryId: 'a', path: ['a.bin'], source,
    revision: { fileId: 'a', fileRevision: 'a-revision', exactSize: BigInt(A_BYTES.length) },
  })
  await archive.admitDirectory({ entryId: 'empty', path: ['empty'], source })
  await archive.writeRange('a', 0n, A_BYTES)
  const checkpoint = await archive.checkpoint('browser-parallel-cut')
  return {
    fixture,
    rangesBeforeLaterDiscovery: beforeLaterDiscovery!.ranges.map(range => `${range.start}:${range.end}`),
    discoveryComplete: checkpoint.discoveryComplete,
    generation: checkpoint.generation.toString(),
    firstPayloadOffset: first.zipLayout!.payloadOffset.toString(),
    growth: session.growth,
  }
}

export async function competingNativeWriter(fixture: NativeArchiveFixture): Promise<string> {
  try {
    const writer = await openNativeObject(await fileHandle(fixture))
    await writer.close()
    return 'unexpectedly-opened'
  } catch (error) {
    return error instanceof Error ? error.name : String(error)
  }
}

export async function failNativeArchiveMetadataCommit() {
  const current = requireSession()
  await current.archive.writeRange('z', 2n, Uint8Array.of(99))
  current.failNextCommit()
  let failure = ''
  try { await current.archive.checkpoint('browser-flush-before-metadata-failure') }
  catch (error) { failure = error instanceof Error ? error.message : String(error) }
  const committed = await current.store.readEntry('z')
  const physical = new Uint8Array(await (await current.handle.getFile()).arrayBuffer())
  const checkpoint = await current.store.readCheckpoint()
  current.store.close()
  session = undefined
  return {
    failure,
    ranges: committed!.ranges.map(range => `${range.start}:${range.end}`),
    physicallyFlushedUncommittedByte: physical[Number(committed!.zipLayout!.payloadOffset) + 2],
    generation: checkpoint!.generation.toString(),
    traces: current.traces,
  }
}

export async function resumeNativeArchive(fixture: NativeArchiveFixture) {
  session = await openArchive(fixture)
  const { archive, store } = session
  const entry = await archive.entry('z')
  const recoveredRanges = entry.ranges.map(range => `${range.start}:${range.end}`)
  // Retrying a committed range must neither overwrite its bytes nor count its CRC twice.
  await archive.writeRange('z', 0n, Uint8Array.of(88, 88))
  await archive.writeRange('z', 2n, Z_BYTES.subarray(2, 3))
  await archive.markDiscoveryComplete()
  const checkpoint = await store.readCheckpoint()
  await archive.close()
  store.close()
  session = undefined
  return { recoveredRanges, discoveryComplete: checkpoint!.discoveryComplete,
    artifactState: checkpoint!.artifactState }
}

export async function prepareOfflineNativeFinalization(fixture: NativeArchiveFixture) {
  session = await openArchive(fixture)
  return session.archive.state.artifactState
}

export async function finalizeNativeArchiveOffline() {
  const current = requireSession()
  const result = await current.archive.finalize()
  const file = await current.handle.getFile()
  const bytes = [...new Uint8Array(await file.arrayBuffer())]
  const root = await navigator.storage.getDirectory()
  const directory = await root.getDirectoryHandle(current.fixture.directoryName)
  const names: string[] = []
  for await (const name of (directory as FileSystemDirectoryHandle & {
    keys(): AsyncIterableIterator<string>
  }).keys()) names.push(name)
  current.store.close()
  session = undefined
  await root.removeEntry(current.fixture.directoryName, { recursive: true })
  await deleteDatabase(current.fixture.databaseName)
  return { artifactState: result.artifactState, bytes, objectNames: names,
    exactBytes: result.sealedLength!.toString(), traces: current.traces }
}

function requireSession(): ArchiveSession {
  if (session === undefined) throw new Error('Native archive fixture is not open')
  return session
}

async function deleteDatabase(name: string): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(name)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('Native fixture database still open'))
  })
}
