import { describe, expect, it } from 'vitest'

import type { DirectoryWork, PendingFile } from '../../src/transfer/job/contract'
import type { TransferJobLimits } from '../../src/transfer/job/limits'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'
import {
  newFileQueue,
  runDiscoveryWorkers,
  runPreparedFileWorkers,
} from '../../src/transfer/job/workers'
import {
  superviseWorkerFamily,
  type WorkerFamilyConsequenceFailure,
} from '../../src/transfer/worker-family/supervisor'
import { fileEntry, identity, identityText } from './v2-job-fixture'

const TEST_PENDING_FILE_METADATA_BYTES = 1024n * 1024n
const TEST_LIMITS: TransferJobLimits = Object.freeze({
  concurrentFiles: 2,
  concurrentDirectories: 2,
  pendingFiles: 4,
  pendingFileMetadataBytes: TEST_PENDING_FILE_METADATA_BYTES,
  catalogNodeClaims: 16,
  directoryAdmissions: 16,
})

describe('worker family supervision', () => {
  it('latches the initiating object, aborts aliased queues once, and drains original promises', async () => {
    const producer = deferred()
    const initiatingWorker = deferred()
    const laggingWorker = deferred()
    const aborted = deferred()
    const initiatingFailure = new Error('initiating worker failure')
    const laterFailure = new Error('lagging worker failure')
    const abortReasons: unknown[] = []
    const queueAbortReasons: unknown[] = []
    const consequences: WorkerFamilyConsequenceFailure[] = []
    let closeCalls = 0
    let settled = false
    const queue = {
      close: () => { closeCalls += 1 },
      abort: (reason: unknown) => { queueAbortReasons.push(reason) },
    }

    const running = superviseWorkerFamily({
      producer: producer.promise,
      workers: [initiatingWorker.promise, laggingWorker.promise],
      queues: [queue, queue],
      abort: (failure) => {
        abortReasons.push(failure)
        producer.reject(failure)
        aborted.resolve()
      },
      observeConsequenceFailure: failure => consequences.push(failure),
    })
    const settlementObserved = running.then(
      () => { settled = true },
      () => { settled = true },
    )

    initiatingWorker.reject(initiatingFailure)
    await aborted.promise

    expect(settled).toBe(false)
    expect(closeCalls).toBe(0)
    expect(abortReasons).toEqual([initiatingFailure])
    expect(queueAbortReasons).toEqual([initiatingFailure])

    laggingWorker.reject(laterFailure)
    await expect(running).rejects.toBe(initiatingFailure)
    await settlementObserved

    expect(consequences).toContainEqual(expect.objectContaining({
      initiatingFailure,
      failure: laterFailure,
      source: { kind: 'worker', workerIndex: 1 },
    }))
  })

  it('closes each aliased queue once but resolves only after every original worker', async () => {
    const producer = deferred()
    const firstWorker = deferred()
    const secondWorker = deferred()
    const queueClosed = deferred()
    let closeCalls = 0
    let abortCalls = 0
    let settled = false
    const queue = {
      close: () => {
        closeCalls += 1
        queueClosed.resolve()
      },
      abort: () => { abortCalls += 1 },
    }
    const running = superviseWorkerFamily({
      producer: producer.promise,
      workers: [firstWorker.promise, secondWorker.promise],
      queues: [queue, queue],
      abort: () => { abortCalls += 1 },
    })
    const settlementObserved = running.then(() => { settled = true })

    producer.resolve()
    await queueClosed.promise
    firstWorker.resolve()
    await Promise.resolve()

    expect(settled).toBe(false)
    expect(closeCalls).toBe(1)
    expect(abortCalls).toBe(0)

    secondWorker.resolve()
    await expect(running).resolves.toBeUndefined()
    await settlementObserved
  })

  it('drains prepared workers before exposing their initiating failure', async () => {
    const controller = new AbortController()
    const firstStarted = deferred()
    const secondStarted = deferred()
    const releaseFirstFailure = deferred()
    const releaseSecondWorker = deferred()
    const aborted = deferred()
    const initiatingFailure = new Error('prepared worker failed')
    const events: string[] = []
    let abortCalls = 0
    let settled = false
    const first = pendingFile(11, 'first.bin')
    const second = pendingFile(12, 'second.bin')

    const running = runPreparedFileWorkers({
      files: [first, second],
      limits: TEST_LIMITS,
      signal: controller.signal,
      abort: (failure) => {
        abortCalls += 1
        if (!controller.signal.aborted) controller.abort(failure)
        aborted.resolve()
      },
      transferFile: async (file) => {
        if (file.entry.idText === first.entry.idText) {
          firstStarted.resolve()
          await releaseFirstFailure.promise
          throw initiatingFailure
        }
        secondStarted.resolve()
        await releaseSecondWorker.promise
        events.push('second-worker-drained')
      },
      recordFileFailure: () => { throw new Error('terminal failures cannot be isolated') },
    })
    const settlementObserved = running.then(
      () => { settled = true },
      () => { settled = true },
    )

    await Promise.all([firstStarted.promise, secondStarted.promise])
    releaseFirstFailure.resolve()
    await aborted.promise

    expect(settled).toBe(false)
    expect(events).toEqual([])

    releaseSecondWorker.resolve()
    await expect(running).rejects.toBe(initiatingFailure)
    await settlementObserved
    expect(abortCalls).toBe(1)
    expect(events).toEqual(['second-worker-drained'])
  })

  it('drains discovery producers and file workers before exposing failure', async () => {
    const controller = new AbortController()
    const fileStarted = deferred()
    const childStarted = deferred()
    const releaseFileFailure = deferred()
    const releaseRootProducer = deferred()
    const releaseChildProducer = deferred()
    const aborted = deferred()
    const initiatingFailure = new Error('discovery file worker failed')
    const events: string[] = []
    const root = directoryWork(2, [])
    const child = directoryWork(3, ['child'])
    const file = pendingFile(13, 'payload.bin')
    const directFiles = newFileQueue(TEST_LIMITS)
    let abortCalls = 0
    let settled = false

    const running = runDiscoveryWorkers({
      root,
      directFiles,
      limits: TEST_LIMITS,
      signal: controller.signal,
      abort: (failure) => {
        abortCalls += 1
        if (!controller.signal.aborted) controller.abort(failure)
        aborted.resolve()
      },
      claimRoot: () => undefined,
      discoverDirectory: async function* (work, files) {
        if (work.cursor.idText === root.cursor.idText) {
          await files.push(file, controller.signal)
          yield child
          await releaseRootProducer.promise
          events.push('root-producer-drained')
          return
        }
        childStarted.resolve()
        await releaseChildProducer.promise
        events.push('child-producer-drained')
      },
      recordDirectoryFailure: () => undefined,
      transferFile: async () => {
        fileStarted.resolve()
        await releaseFileFailure.promise
        throw initiatingFailure
      },
      recordFileFailure: () => { throw new Error('terminal failures cannot be isolated') },
    })
    const settlementObserved = running.then(
      () => { settled = true },
      () => { settled = true },
    )

    await Promise.all([fileStarted.promise, childStarted.promise])
    releaseFileFailure.resolve()
    await aborted.promise

    expect(settled).toBe(false)
    expect(events).toEqual([])

    releaseRootProducer.resolve()
    releaseChildProducer.resolve()
    await expect(running).rejects.toBe(initiatingFailure)
    await settlementObserved
    expect(abortCalls).toBe(1)
    expect(events).toEqual(expect.arrayContaining([
      'root-producer-drained',
      'child-producer-drained',
    ]))
  })
})

function pendingFile(firstIdentityByte: number, name: string): PendingFile {
  const entry = fileEntry(identity(firstIdentityByte), name, 1n)
  return Object.freeze({
    entry,
    sourceAuthenticationPath: snapshotSourceAuthenticationPath([name]),
    logicalArtifactPath: snapshotLogicalArtifactPath([name]),
    materializationRelativePath: snapshotMaterializationRootRelativePath([name]),
    parent: Object.freeze({
      kind: 'reference',
      directoryId: identityText(2),
      generation: identityText(90),
      sourceAuthenticationPath: snapshotSourceAuthenticationPath([]),
      logicalArtifactPath: snapshotLogicalArtifactPath([]),
    }),
    ready: Promise.resolve(),
  })
}

function directoryWork(firstIdentityByte: number, path: readonly string[]): DirectoryWork {
  const id = identity(firstIdentityByte)
  const idText = identityText(firstIdentityByte)
  return Object.freeze({
    cursor: Object.freeze({
      id,
      idText,
      path: Object.freeze([...path]),
      ancestry: Object.freeze([idText]),
      selected: true,
    }),
    materializeParent: async () => Object.freeze({
      kind: 'reference' as const,
      directoryId: idText,
      generation: identityText(90),
      sourceAuthenticationPath: snapshotSourceAuthenticationPath(path),
      logicalArtifactPath: snapshotLogicalArtifactPath(path),
    }),
  })
}

function deferred(): {
  readonly promise: Promise<void>
  readonly resolve: () => void
  readonly reject: (reason: unknown) => void
} {
  let resolve!: () => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<void>((innerResolve, innerReject) => {
    resolve = innerResolve
    reject = innerReject
  })
  return { promise, resolve, reject }
}
