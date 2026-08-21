import {
  V2_MAXIMUM_PENDING_DIRECTORIES,
  type DirectoryWork,
  type PendingFile,
} from './contract'
import { runV2DirectoryTransferWorker } from './directory-transfer'
import { isV2FileScopedTransferFailure } from './failures'
import type { TransferJobLimits } from './limits'
import { AsyncBoundedQueue, pendingFileMetadataBytes } from './scheduler'
import {
  superviseWorkerFamily,
  type WorkerFamilyConsequenceFailure,
} from '../worker-family/supervisor'

export function newFileQueue(limits: TransferJobLimits): AsyncBoundedQueue<PendingFile> {
  return new AsyncBoundedQueue<PendingFile>(
    limits.pendingFiles,
    limits.pendingFileMetadataBytes,
    pendingFileMetadataBytes,
  )
}

export async function runDiscoveryWorkers(input: {
  readonly root: DirectoryWork
  readonly directFiles?: AsyncBoundedQueue<PendingFile>
  readonly limits: TransferJobLimits
  readonly signal: AbortSignal
  readonly abort: (error: unknown) => void
  readonly claimRoot: (root: DirectoryWork) => void
  readonly discoverDirectory: (
    work: DirectoryWork,
    files: AsyncBoundedQueue<PendingFile>,
  ) => AsyncGenerator<DirectoryWork, void>
  readonly recordDirectoryFailure: (directoryIdentity: string, error: unknown) => void
  readonly transferFile: (file: PendingFile) => Promise<void>
  readonly recordFileFailure: (file: PendingFile, error: unknown) => void
  readonly observeConsequenceFailure?: (failure: WorkerFamilyConsequenceFailure) => void
}): Promise<void> {
  const directoryQueue = new AsyncBoundedQueue<DirectoryWork>(
    V2_MAXIMUM_PENDING_DIRECTORIES,
    BigInt(V2_MAXIMUM_PENDING_DIRECTORIES),
    () => 1n,
    true,
  )
  const sinkFiles = input.directFiles ?? newFileQueue(input.limits)
  input.claimRoot(input.root)
  await directoryQueue.push(input.root, input.signal)

  const directoryWorkers = Array.from({ length: input.limits.concurrentDirectories }, () =>
    runV2DirectoryTransferWorker(directoryQueue, sinkFiles, {
      signal: input.signal,
      discoverDirectory: input.discoverDirectory,
      isolateDirectory: input.recordDirectoryFailure,
    }),
  )
  const directFiles = input.directFiles
  const fileWorkers = directFiles === undefined
    ? []
    : Array.from({ length: input.limits.concurrentFiles }, () => runFileWorker({
        queue: directFiles,
        signal: input.signal,
        transferFile: input.transferFile,
        recordFileFailure: input.recordFileFailure,
      }))
  // Directory workers collectively own production. Waiting for all of them here
  // makes queue closure a success-only transition while the supervisor still
  // observes and drains every original promise independently.
  const producer = Promise.allSettled(directoryWorkers).then(() => undefined)
  await superviseWorkerFamily({
    producer,
    workers: [...directoryWorkers, ...fileWorkers],
    // Direct transfers expose the same file queue through producer and consumer
    // roles; the supervisor collapses that alias before any terminal action.
    queues: [
      directoryQueue,
      sinkFiles,
      ...(directFiles === undefined ? [] : [directFiles]),
    ],
    abort: input.abort,
    ...(input.observeConsequenceFailure === undefined
      ? {}
      : { observeConsequenceFailure: input.observeConsequenceFailure }),
  })
}

export async function runPreparedFileWorkers(input: {
  readonly files: readonly PendingFile[]
  readonly limits: TransferJobLimits
  readonly signal: AbortSignal
  readonly abort: (error: unknown) => void
  readonly transferFile: (file: PendingFile) => Promise<void>
  readonly recordFileFailure: (file: PendingFile, error: unknown) => void
  readonly observeConsequenceFailure?: (failure: WorkerFamilyConsequenceFailure) => void
}): Promise<void> {
  const queue = newFileQueue(input.limits)
  const workers = Array.from({ length: input.limits.concurrentFiles }, () => runFileWorker({
    queue,
    signal: input.signal,
    transferFile: input.transferFile,
    recordFileFailure: input.recordFileFailure,
  }))
  const producer = (async () => {
    for (const file of input.files) await queue.push(file, input.signal)
  })()
  await superviseWorkerFamily({
    producer,
    workers,
    queues: [queue],
    abort: input.abort,
    ...(input.observeConsequenceFailure === undefined
      ? {}
      : { observeConsequenceFailure: input.observeConsequenceFailure }),
  })
}

async function runFileWorker(input: {
  readonly queue: AsyncBoundedQueue<PendingFile>
  readonly signal: AbortSignal
  readonly transferFile: (file: PendingFile) => Promise<void>
  readonly recordFileFailure: (file: PendingFile, error: unknown) => void
}): Promise<void> {
  while (true) {
    const file = await input.queue.pop(input.signal)
    if (file === undefined) return
    try {
      await file.ready
      input.signal.throwIfAborted()
      await input.transferFile(file)
    } catch (error) {
      if (input.signal.aborted) throw error
      if (isV2FileScopedTransferFailure(error)) {
        input.recordFileFailure(file, error)
        continue
      }
      throw error
    }
  }
}
