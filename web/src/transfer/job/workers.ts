import {
  V2_MAXIMUM_PENDING_DIRECTORIES,
  type DirectoryWork,
  type PendingFile,
} from './contract'
import { runV2DirectoryTransferWorker } from './directory-transfer'
import { DiscoveryQueue, type DiscoverySchedulingObservation } from '../discovery/queue'
import { runV2DirectoryReplayWorker, type V2DirectoryReplay } from '../discovery/v2-directory-replay'
import { isV2FileScopedTransferFailure } from './failures'
import type { TransferExecutionLimits, TransferJobLimits } from './limits'
import {
  AsyncBoundedQueue,
  OutputResourceBudget,
  pendingFileMetadataBytes,
} from './scheduler'
import {
  superviseWorkerFamily,
  type WorkerFamilyConsequenceFailure,
} from '../worker-family/supervisor'
import {
  createPerformanceFilePipelineObservation,
  type PerformanceFilePipelineObservation,
} from '../../output/diagnostics/performance-runtime-observations'
import type { PerformanceSummaryObservations } from '../../output/diagnostics/performance-summary'

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
  readonly execution?: TransferExecutionLimits
  readonly resources?: OutputResourceBudget
  readonly performance?: PerformanceSummaryObservations
  readonly signal: AbortSignal
  readonly abort: (error: unknown) => void
  readonly claimRoot: (root: DirectoryWork) => void
  readonly discoverDirectory: (
    work: DirectoryWork,
    replays: AsyncBoundedQueue<V2DirectoryReplay>,
  ) => AsyncGenerator<DirectoryWork, void>
  readonly observeDiscovery?: (event: DiscoverySchedulingObservation) => void
  readonly discoveryComplete: () => void
  readonly recordDiscoveryFailure: (directoryIdentity: string, error: unknown) => void
  readonly recordDirectoryFailure: (directoryIdentity: string, error: unknown) => void
  readonly transferFile: (
    file: PendingFile,
    pipeline?: PerformanceFilePipelineObservation,
  ) => Promise<void>
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

  const replays = new DiscoveryQueue<V2DirectoryReplay>({
    queue: 'generations',
    maximumItems: input.limits.pendingGenerations,
    maximumMetadataBytes: input.limits.pendingGenerationMetadataBytes,
    weight: replay => replay.metadataBytes,
    ...(input.observeDiscovery === undefined ? {} : { observe: input.observeDiscovery }),
  })
  const directoryWorkers = Array.from({ length: input.limits.concurrentDirectories }, () =>
    runV2DirectoryTransferWorker(directoryQueue, replays, {
      signal: input.signal,
      discoverDirectory: input.discoverDirectory,
      isolateDirectory: input.recordDiscoveryFailure,
    }),
  )
  const replayWorkers = Array.from({ length: input.limits.concurrentDirectories }, () =>
    runV2DirectoryReplayWorker({
      queue: replays,
      files: sinkFiles,
      signal: input.signal,
      isolateDirectory: input.recordDirectoryFailure,
    }),
  )
  const discovery = Promise.all(directoryWorkers).then(() => {
    input.signal.throwIfAborted()
    input.discoveryComplete()
    replays.close()
  })
  const directFiles = input.directFiles
  let fileWorkers: Promise<void>[] = []
  if (directFiles !== undefined) {
    const execution = input.execution
    const resources = input.resources
    if (execution === undefined || resources === undefined) {
      throw new TypeError('direct discovery workers require bound output execution resources')
    }
    fileWorkers = Array.from({ length: execution.concurrentFiles }, () => runFileWorker({
      queue: directFiles,
      resources,
      ...(input.performance === undefined ? {} : { performance: input.performance }),
      signal: input.signal,
      transferFile: input.transferFile,
      recordFileFailure: input.recordFileFailure,
    }))
  }
  // Discovery closes its denominator independently. Only replay completion closes
  // file production; the supervisor still drains every original worker on failure.
  const producer = Promise.all(replayWorkers).then(() => undefined)
  await superviseWorkerFamily({
    producer,
    workers: [...directoryWorkers, discovery, ...replayWorkers, ...fileWorkers],
    // Direct transfers expose the same file queue through producer and consumer
    // roles; the supervisor collapses that alias before any terminal action.
    queues: [
      directoryQueue,
      replays,
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
  readonly execution: TransferExecutionLimits
  readonly resources: OutputResourceBudget
  readonly signal: AbortSignal
  readonly abort: (error: unknown) => void
  readonly performance?: PerformanceSummaryObservations
  readonly transferFile: (
    file: PendingFile,
    pipeline?: PerformanceFilePipelineObservation,
  ) => Promise<void>
  readonly recordFileFailure: (file: PendingFile, error: unknown) => void
  readonly observeConsequenceFailure?: (failure: WorkerFamilyConsequenceFailure) => void
}): Promise<void> {
  const queue = newFileQueue(input.limits)
  const workers = Array.from({ length: input.execution.concurrentFiles }, () => runFileWorker({
    queue,
    resources: input.resources,
    ...(input.performance === undefined ? {} : { performance: input.performance }),
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
  readonly resources: OutputResourceBudget
  readonly performance?: PerformanceSummaryObservations
  readonly signal: AbortSignal
  readonly transferFile: (
    file: PendingFile,
    pipeline?: PerformanceFilePipelineObservation,
  ) => Promise<void>
  readonly recordFileFailure: (file: PendingFile, error: unknown) => void
}): Promise<void> {
  const pipeline = createPerformanceFilePipelineObservation(input.performance)
  try {
    while (true) {
      const file = await input.queue.pop(input.signal)
      if (file === undefined) return
      try {
        await file.ready
        input.signal.throwIfAborted()
        const lease = await input.resources.acquireFile(input.signal)
        pipeline?.transition('initial_lineage')
        try {
          await input.transferFile(file, pipeline)
        } finally {
          pipeline?.transition('idle_no_ready_file')
          lease.release()
        }
      } catch (error) {
        if (input.signal.aborted) throw error
        if (isV2FileScopedTransferFailure(error)) {
          input.recordFileFailure(file, error)
          continue
        }
        throw error
      }
    }
  } finally {
    pipeline?.close()
  }
}
