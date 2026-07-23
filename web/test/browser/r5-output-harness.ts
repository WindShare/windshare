import { byteRange } from '../../src/content/geometry'
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
  discardFileSystemAccessOutputSession,
  prepareFileSystemAccessReauthorization,
} from '../../src/output/file-system-access/session'

const FILE = Object.freeze({
  source: Object.freeze({
    shareInstance: 'browser-share',
    fileId: 'browser-file',
    fileRevision: 'browser-revision',
  }),
  path: Object.freeze(['browser-file.bin']),
  exactSize: 5n,
})
const ACTIVE_SIGNAL = new AbortController().signal

const exporter = Object.freeze({
  export: async () => ORIGIN_PRIVATE_EXPORT_COMPLETE,
})

const heldSessions = new Map<string, Awaited<ReturnType<typeof openOriginPrivateOutputSession>>>()

export async function createCheckpoint(outputSessionId: string): Promise<readonly string[]> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    exporter,
    retainAfterExport: true,
  })
  const begun = await session.beginFile(FILE)
  await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
  const durable = await begun.transaction.checkpoint()
  return durable.ranges.map((range) => `${range.start}:${range.end}`)
}

export async function reopenCheckpoint(outputSessionId: string): Promise<{
  readonly ranges: readonly string[]
  readonly coversPrefix: boolean
  readonly durability: string
}> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    exporter,
    retainAfterExport: true,
  })
  const begun = await session.beginFile(FILE)
  const result = {
    ranges: begun.durableRanges.ranges.map((range) => `${range.start}:${range.end}`),
    coversPrefix: begun.durableRanges.covers(byteRange(0n, 3n)),
    durability: session.capabilities.durability,
  }
  await session.abortJob()
  return result
}

export async function createCrashCut(
  outputSessionId: string,
  phase: CheckpointCrashPhase,
): Promise<boolean> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    exporter,
    retainAfterExport: true,
    crashHook: (current) => {
      if (current === phase) throw new Error(`simulated crash after ${phase}`)
    },
  })
  const begun = await session.beginFile(FILE)
  try {
    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    if (phase !== 'DataWritten') await begun.transaction.checkpoint()
  } catch (error) {
    return error instanceof Error && error.message === `simulated crash after ${phase}`
  }
  return false
}

export async function holdOutputSession(outputSessionId: string): Promise<void> {
  const session = await openOriginPrivateOutputSession({
    outputSessionId,
    exporter,
    retainAfterExport: true,
  })
  heldSessions.set(outputSessionId, session)
}

export async function competingSessionError(outputSessionId: string): Promise<string | undefined> {
  try {
    await openOriginPrivateOutputSession({
      outputSessionId,
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
  await session.abortJob()
}

export async function createPersistentHandleCheckpoint(
  outputSessionId: string,
): Promise<readonly string[]> {
  const originRoot = await originPrivateRoot()
  const outputRoot = await originRoot.getDirectoryHandle(handleRootName(outputSessionId), {
    create: true,
  })
  const session = await acquireFileSystemAccessOutputSession(outputRoot, { outputSessionId })
  const begun = await session.beginFile(FILE)
  await begun.transaction.writeRange(0n, Uint8Array.of(4, 5, 6))
  return (await begun.transaction.checkpoint()).ranges
    .map((range) => `${range.start}:${range.end}`)
}

export async function reopenPersistentHandleCheckpoint(
  outputSessionId: string,
): Promise<readonly string[]> {
  const prepared = await prepareFileSystemAccessReauthorization({ outputSessionId })
  const session = await prepared.authorize()
  const begun = await session.beginFile(FILE)
  const ranges = begun.durableRanges.ranges.map((range) => `${range.start}:${range.end}`)
  await session.abortJob()
  await discardFileSystemAccessOutputSession({ outputSessionId })
  const originRoot = await originPrivateRoot()
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
    () => acquireFileSystemAccessOutputSession(outputRoot, { outputSessionId }),
  )
  const begun = await outputStep('begin output file', () => session.beginFile(FILE))
  await outputStep(
    'write output file',
    () => begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5)),
  )
  await outputStep('commit output file', () => begun.transaction.commit())
  await outputStep('finish output session', () => session.finishJob(
    jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
    ACTIVE_SIGNAL,
  ))
  let file: File
  try {
    file = await (await outputRoot.getFileHandle(FILE.path[0] ?? '')).getFile()
  } catch (error) {
    throw new Error('Completed persistent output file is missing', { cause: error })
  }
  let metadataRetired = false
  try {
    await prepareFileSystemAccessReauthorization({ outputSessionId })
  } catch (error) {
    metadataRetired = error instanceof DOMException && error.name === 'NotFoundError'
  }
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
  const begun = await session.beginFile(FILE)
  await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5))
  await begun.transaction.commit()
  await session.finishJob(
    jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY),
    ACTIVE_SIGNAL,
  )

  const reopened = await openOriginPrivateOutputSession({
    outputSessionId,
    exporter,
    retainAfterExport: true,
  })
  const fresh = await reopened.beginFile(FILE)
  const reopenedRanges = fresh.durableRanges.ranges
    .map((range) => `${range.start}:${range.end}`)
  await reopened.abortJob()
  return { exported, reopenedRanges }
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

function handleRootName(outputSessionId: string): string {
  return `r5-handle-${outputSessionId}`
}

async function outputStep<T>(label: string, operation: () => Promise<T>): Promise<T> {
  try {
    return await operation()
  } catch (error) {
    throw new Error(`R5 output step failed: ${label}`, { cause: error })
  }
}
