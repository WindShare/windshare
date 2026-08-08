import { byteRange } from '../../src/content/geometry'
import type { OutputSession } from '../../src/transfer/output-session'
import type { CheckpointCrashPhase } from '../../src/output/persistent-tree/contracts'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  jobOutcome,
} from '../../src/transfer/outcome'
import {
  ORIGIN_PRIVATE_EXPORT_COMPLETE,
  openOriginPrivateOutputSession,
} from '../../src/output/origin-private/session'
import {
  acquireFileSystemAccessOutputSession,
  FILE_SYSTEM_ACCESS_BACKEND,
} from '../../src/output/file-system-access/session'
import { IndexedDbOutputRepository } from '../../src/output/browser/indexeddb-repository'
import {
  admittedOutputFile,
  TEST_DIRECTORY_ADMISSION_SCOPE,
  testOutputIdentity,
} from '../output/admission-fixture'

const FILE = Object.freeze({
  source: Object.freeze({
    shareInstance: testOutputIdentity('browser-share'),
    fileId: testOutputIdentity('browser-file'),
    fileRevision: testOutputIdentity('browser-revision'),
  }),
  path: Object.freeze(['browser-file.bin']),
  exactSize: 5n,
})
const ACTIVE_SIGNAL = new AbortController().signal
const TEST_DURABLE_BINDING = Object.freeze({
  directoryAdmissionScope: TEST_DIRECTORY_ADMISSION_SCOPE,
  transferIntentDigest: TEST_DIRECTORY_ADMISSION_SCOPE.transferIntentDigest,
  rootIdentity: TEST_DIRECTORY_ADMISSION_SCOPE.transferIntentDigest,
})
const TEST_DURABLE_NAMESPACE = Object.freeze({
  backend: FILE_SYSTEM_ACCESS_BACKEND,
  transferIntentDigest: TEST_DURABLE_BINDING.transferIntentDigest,
  rootIdentity: TEST_DURABLE_BINDING.rootIdentity,
})

const exporter = Object.freeze({
  export: async () => ORIGIN_PRIVATE_EXPORT_COMPLETE,
})

const heldSessions = new Map<string, Awaited<ReturnType<typeof openOriginPrivateOutputSession>>>()

export async function createCheckpoint(outputSessionId: string): Promise<readonly string[]> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter,
    retainAfterExport: true,
  })
  const begun = await beginTestFile(session)
  await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3), ACTIVE_SIGNAL)
  const durable = await begun.transaction.checkpoint(ACTIVE_SIGNAL)
  return durable.ranges.map((range) => `${range.start}:${range.end}`)
}

export async function reopenCheckpoint(outputSessionId: string): Promise<{
  readonly ranges: readonly string[]
  readonly coversPrefix: boolean
  readonly durability: string
}> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter,
    retainAfterExport: true,
  })
  const begun = await beginTestFile(session)
  const result = {
    ranges: begun.durableRanges.ranges.map((range) => `${range.start}:${range.end}`),
    coversPrefix: begun.durableRanges.covers(byteRange(0n, 3n)),
    durability: session.capabilities.durability,
  }
  await session.pauseJob(new Error('recovery probe complete'))
  return result
}

export async function createCrashCut(
  outputSessionId: string,
  phase: CheckpointCrashPhase,
): Promise<boolean> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter,
    retainAfterExport: true,
    crashHook: (current) => {
      if (current === phase) throw new Error(`simulated crash after ${phase}`)
    },
  })
  const begun = await beginTestFile(session)
  try {
    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3), ACTIVE_SIGNAL)
    if (phase !== 'DataWritten') await begun.transaction.checkpoint(ACTIVE_SIGNAL)
  } catch (error) {
    return error instanceof Error && error.message === `simulated crash after ${phase}`
  }
  return false
}

export async function holdOutputSession(outputSessionId: string): Promise<void> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter,
    retainAfterExport: true,
  })
  heldSessions.set(outputSessionId, session)
}

export async function competingSessionError(outputSessionId: string): Promise<string | undefined> {
  try {
    await openOriginPrivateOutputSession({
      outputSessionId,
      ...TEST_DURABLE_BINDING,
      exporter,
      retainAfterExport: true,
    })
    return undefined
  } catch (error) {
    return error instanceof DOMException ? error.name : 'Error'
  }
}

export async function releaseOutputSession(outputSessionId: string): Promise<void> {
  const session = heldSessions.get(outputSessionId)
  if (session === undefined) return
  heldSessions.delete(outputSessionId)
  await session.pauseJob(new Error('release browser lease'))
}

export async function createPersistentHandleCheckpoint(
  outputSessionId: string,
): Promise<readonly string[]> {
  const originRoot = await originPrivateRoot()
  const outputRoot = await originRoot.getDirectoryHandle(handleRootName(outputSessionId), {
    create: true,
  })
  const session = await acquireFileSystemAccessOutputSession(outputRoot, {
    outputSessionId,
    ...TEST_DURABLE_BINDING,
  })
  const begun = await beginTestFile(session)
  await begun.transaction.writeRange(0n, Uint8Array.of(4, 5, 6), ACTIVE_SIGNAL)
  return (await begun.transaction.checkpoint(ACTIVE_SIGNAL)).ranges
    .map((range) => `${range.start}:${range.end}`)
}

export async function reopenPersistentHandleCheckpoint(
  outputSessionId: string,
): Promise<readonly string[]> {
  const originRoot = await originPrivateRoot()
  const outputRoot = await originRoot.getDirectoryHandle(handleRootName(outputSessionId))
  const session = await acquireFileSystemAccessOutputSession(outputRoot, {
    outputSessionId: `${outputSessionId}-resumed`,
    ...TEST_DURABLE_BINDING,
  })
  const begun = await beginTestFile(session)
  const ranges = begun.durableRanges.ranges.map((range) => `${range.start}:${range.end}`)
  await session.pauseJob(new Error('recovery probe complete'))
  const repository = await IndexedDbOutputRepository.openExisting(
    'windshare-output-checkpoints',
    TEST_DURABLE_NAMESPACE,
  )
  await repository.deleteSessionData()
  repository.close()
  await originRoot.removeEntry(handleRootName(outputSessionId), { recursive: true })
  return ranges
}

export async function completePersistentHandleOutput(
  outputSessionId: string,
): Promise<{ readonly bytes: readonly number[]; readonly metadataRetired: boolean }> {
  const originRoot = await originPrivateRoot()
  const rootName = handleRootName(outputSessionId)
  const outputRoot = await outputStep(
    'create output root',
    () => originRoot.getDirectoryHandle(rootName, { create: true }),
  )
  const session = await outputStep(
    'acquire output session',
    () => acquireFileSystemAccessOutputSession(outputRoot, {
      outputSessionId,
      ...TEST_DURABLE_BINDING,
    }),
  )
  const begun = await outputStep('begin output file', () => beginTestFile(session))
  await outputStep(
    'write output file',
    () => begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5), ACTIVE_SIGNAL),
  )
  await outputStep('commit output file', () => begun.transaction.commit(ACTIVE_SIGNAL))
  await outputStep('finish output session', () => session.completeJob(
    jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
    ACTIVE_SIGNAL,
  ))
  let file: File
  try {
    file = await (await outputRoot.getFileHandle(FILE.path[0] ?? '')).getFile()
  } catch (error) {
    throw new Error('Completed persistent output file is missing', { cause: error })
  }
  const repository = await IndexedDbOutputRepository.openExisting(
    'windshare-output-checkpoints',
    TEST_DURABLE_NAMESPACE,
  )
  const [committed, candidates] = await Promise.all([
    repository.scanCommitted({ direction: 'ascending' }),
    repository.scanCandidates({ direction: 'ascending' }),
  ])
  const metadataRetired = committed.records.length === 0 && candidates.records.length === 0
  repository.close()
  const bytes = [...new Uint8Array(await file.arrayBuffer())]
  try {
    await originRoot.removeEntry(rootName, { recursive: true })
  } catch (error) {
    throw new Error('Completed persistent output root cleanup failed', { cause: error })
  }
  return {
    bytes,
    metadataRetired,
  }
}

export async function completeOriginPrivateOutput(
  outputSessionId: string,
): Promise<{ readonly exported: readonly number[]; readonly reopenedRanges: readonly string[] }> {
  let exported: readonly number[] = []
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter: {
      export: async (snapshot) => {
        let staged
        for await (const file of snapshot.files()) {
          staged = file
          break
        }
        if (staged === undefined) throw new Error('Committed staged file is missing')
        exported = [...new Uint8Array(await (await staged.read()).arrayBuffer())]
        return ORIGIN_PRIVATE_EXPORT_COMPLETE
      },
    },
  })
  const begun = await beginTestFile(session)
  await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5), ACTIVE_SIGNAL)
  await begun.transaction.commit(ACTIVE_SIGNAL)
  await session.completeJob(
    jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
    ACTIVE_SIGNAL,
  )

  const reopened = await openOriginPrivateOutputSession({
    outputSessionId,
    ...TEST_DURABLE_BINDING,
    exporter,
    retainAfterExport: true,
  })
  const fresh = await beginTestFile(reopened)
  const reopenedRanges = fresh.durableRanges.ranges
    .map((range) => `${range.start}:${range.end}`)
  await reopened.pauseJob(new Error('completion probe complete'))
  return { exported, reopenedRanges }
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

async function beginTestFile(session: OutputSession) {
  return session.beginFile(await admittedOutputFile(session, FILE), ACTIVE_SIGNAL)
}

function handleRootName(outputSessionId: string): string {
  return `durable-handle-${outputSessionId}`
}

async function outputStep<T>(label: string, operation: () => Promise<T>): Promise<T> {
  try {
    return await operation()
  } catch (error) {
    throw new Error(`Durable recovery step failed: ${label}`, { cause: error })
  }
}
