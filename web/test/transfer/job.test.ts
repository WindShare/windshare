import { describe, expect, it } from 'vitest'

import {
  directoryId,
  fileId,
  scanAttemptId,
  structuralCatalogNamePolicy,
  type CatalogDirectoryGeneration,
  type CatalogDirectoryNode,
  type CatalogFileNode,
  type DirectoryId,
  type FileId,
} from '../../src/catalog/model'
import { ProgressiveCatalogTree } from '../../src/catalog/tree'
import { byteRange } from '../../src/content/geometry'
import {
  TransferJob,
  MAXIMUM_CONCURRENT_TRANSFER_FILES,
  type DirectoryDiscoveryResult,
  type DirectoryDiscoverySource,
  type FileTransferService,
  type PreparedFileTransfer,
} from '../../src/transfer/job'
import type { JobOutcome } from '../../src/transfer/outcome'
import {
  type BeginOutputFileResult,
  type FileAbortDisposition,
  type OutputCapabilities,
  type OutputDirectory,
  type OutputFile,
  type OutputFileOwnership,
  type OutputFileTransaction,
  type OutputSession,
  type OutputSessionIdentity,
  type OutputSourceIdentity,
  VerifiedDurableRanges,
  outputCapabilities,
  outputSessionIdentity,
} from '../../src/transfer/output-session'
import { SelectionRules } from '../../src/transfer/selection-rules'

describe('incremental TransferJob', () => {
  it('rejects concurrency above the frozen output/lease admission bound', () => {
    const root = directoryId('bounded-root')
    expect(() => new TransferJob(
      treeWithRoot(root, []),
      new SelectionRules(true),
      new FakeDiscovery(new Map()),
      new FakeFiles(),
      new FakeOutput(true),
      {
        shareInstance: 'share',
        maximumConcurrentFiles: MAXIMUM_CONCURRENT_TRANSFER_FILES + 1,
      },
    )).toThrow(/between 1 and 32/u)
  })

  it('starts a committed directory file before an unrelated scan finishes', async () => {
    const root = directoryId('root')
    const first = directoryId('first')
    const blocked = directoryId('blocked')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: first, name: 'first' },
      { kind: 'directory', id: blocked, name: 'blocked' },
    ])
    const gate = deferred<CatalogDirectoryGeneration>()
    const fileStarted = deferred<void>()
    const discovery = new FakeDiscovery(new Map<DirectoryId, DiscoveryResult>([
      [first, {
        directoryId: first,
        generation: 'first',
        children: [{ kind: 'file', id: file, name: 'now.bin', expectedSize: 4n }],
      }],
      [blocked, gate.promise],
    ]))
    const files = new FakeFiles(new Map([[file, {
      transfer: async (transaction, durable) => {
        fileStarted.resolve()
        expect(durable.ranges).toEqual([{ start: 0n, end: 2n }])
        await transaction.writeRange(2n, Uint8Array.of(3, 4))
        expect((await transaction.checkpoint()).ranges).toEqual([{ start: 0n, end: 4n }])
      },
    }]]))
    const output = new FakeOutput(true, new Map([[file, [byteRange(0n, 2n)]]]))
    const measures: string[] = []
    const running = new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      files,
      output,
      {
        shareInstance: 'share',
        maximumConcurrentFiles: 2,
        onMeasure: (measure) => measures.push(measure.sizeClass),
      },
    ).run()

    await fileStarted.promise
    expect(discovery.calls).toEqual([first, blocked])
    expect(output.finished).toBeUndefined()

    gate.resolve({ directoryId: blocked, generation: 'blocked', children: [] })
    const result = await running
    expect(result.outcome.status).toBe('Succeeded')
    expect(result.measure).toMatchObject({
      discoveredFiles: 1,
      discoveredBytes: 4n,
      discovery: 'complete',
      sizeClass: 'small',
    })
    expect(measures).toEqual(['unknown', 'small'])
    expect(output.committedPaths).toEqual(['first/now.bin'])
    expect(output.finalizedDirectories).toHaveLength(2)
    expect(output.finalizedDirectories).toEqual(expect.arrayContaining(['first', 'blocked']))
  })

  it('records a directory failure, skips its unknown subtree, and continues siblings', async () => {
    const root = directoryId('root')
    const failed = directoryId('failed')
    const healthy = directoryId('healthy')
    const file = fileId('healthy-file')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: failed, name: 'failed' },
      { kind: 'directory', id: healthy, name: 'healthy' },
    ])
    const discovery = new FakeDiscovery(new Map<DirectoryId, DiscoveryResult>([
      [failed, new Error('scan failed')],
      [healthy, {
        directoryId: healthy,
        generation: 'healthy',
        children: [{ kind: 'file', id: file, name: 'ok', expectedSize: 1n }],
      }],
    ]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failures).toHaveLength(1)
    expect(result.outcome.failures[0]).toMatchObject({
      kind: 'directory',
      directoryId: failed,
    })
    expect(result.measure).toMatchObject({ discovery: 'failed', sizeClass: 'unknown' })
    expect(output.committedPaths).toEqual(['healthy/ok'])
    expect(tree.directoryState(failed).status).toBe('failed')
  })

  it('aborts on an untyped catalog-client failure instead of misreporting a directory error', async () => {
    const root = directoryId('root')
    const directory = directoryId('directory')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: directory, name: 'directory' },
    ])
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(result.outcome.failures).toEqual([])
    expect(output.abortJobCalls).toBe(1)
    expect(tree.directoryState(directory).status).toBe('undiscovered')
  })

  it('reuses an existing terminal directory failure without silently rescanning it', async () => {
    const root = directoryId('root')
    const failed = directoryId('failed')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: failed, name: 'failed' },
    ])
    const failure = {
      attemptId: scanAttemptId('failed-attempt'),
      kind: 'permanent' as const,
      message: 'access denied',
    }
    tree.failDirectory(failed, failure)
    const discovery = new FakeDiscovery(new Map([[failed, new Error('must not rescan')]]))

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      new FakeFiles(),
      new FakeOutput(true),
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome).toMatchObject({
      status: 'CompletedWithErrors',
      failures: [{ kind: 'directory', directoryId: failed, reason: failure }],
    })
    expect(discovery.calls).toEqual([])
  })

  it('rejects a generation spliced from another requested directory atomically', async () => {
    const root = directoryId('root')
    const requested = directoryId('requested')
    const other = directoryId('other')
    const injected = fileId('injected')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: requested, name: 'requested' },
      { kind: 'directory', id: other, name: 'other' },
    ])
    const discovery = new FakeDiscovery(new Map([[requested, {
      directoryId: other,
      generation: 'other-generation',
      children: [{ kind: 'file', id: injected, name: 'injected', expectedSize: 1n }],
    }]]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(tree.node(injected)).toBeUndefined()
    expect(tree.directoryState(requested).status).toBe('undiscovered')
    expect(tree.directoryState(other).status).toBe('undiscovered')
    expect(output.abortJobCalls).toBe(1)
  })

  it('cancels an in-progress scan and releases its loading projection', async () => {
    const root = directoryId('root')
    const directory = directoryId('directory')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: directory, name: 'directory' },
    ])
    const started = deferred<void>()
    const discovery: DirectoryDiscoverySource = {
      async listChildren(_directory, signal) {
        started.resolve()
        return await new Promise<DirectoryDiscoveryResult>((_resolve, reject) => {
          const abort = () => reject(signal.reason)
          signal.addEventListener('abort', abort, { once: true })
        })
      },
    }
    const controller = new AbortController()
    const output = new FakeOutput(true)
    const running = new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run(controller.signal)

    await started.promise
    controller.abort(new Error('cancelled'))
    const result = await running

    expect(result.outcome.status).toBe('Aborted')
    expect(tree.directoryState(directory).status).toBe('undiscovered')
    expect(output.abortJobCalls).toBe(1)
  })

  it('keeps cancellation live while output finalization is blocked', async () => {
    const root = directoryId('finalize-root')
    const output = new BlockingFinishOutput()
    const controller = new AbortController()
    const running = new TransferJob(
      treeWithRoot(root, []),
      new SelectionRules(true),
      new FakeDiscovery(new Map()),
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run(controller.signal)

    await output.finishStarted.promise
    controller.abort(new DOMException('cancelled during export', 'AbortError'))
    const result = await running

    expect(result.outcome.status).toBe('Aborted')
    expect(output.finished).toBeUndefined()
    expect(output.abortJobCalls).toBe(1)
  })
})

describe('TransferJob output transactions', () => {
  it('isolates a failed file when the backend promises file transactions', async () => {
    const root = directoryId('root')
    const bad = fileId('bad')
    const good = fileId('good')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: bad, name: 'bad', expectedSize: 1n },
      { kind: 'file', id: good, name: 'good', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[bad, {
      transfer: async () => { throw new Error('file drifted') },
    }]]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failures).toHaveLength(1)
    expect(output.abortedFiles).toEqual(['bad'])
    expect(output.committedPaths).toEqual(['good'])
    expect(output.abortJobCalls).toBe(0)
  })

  it('rejects a huge write offset before it can escape the file transaction', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[file, {
      transfer: async (transaction) => {
        await transaction.writeRange(1n << 80n, Uint8Array.of(1))
      },
    }]]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.abortedFiles).toEqual(['file'])
    expect(output.committedPaths).toEqual([])
  })

  it('refuses durable ranges bound to another source revision', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const staleSource: OutputSourceIdentity = {
      shareInstance: 'share',
      fileId: file,
      fileRevision: 'stale-revision',
    }
    const output = new FakeOutput(true, new Map(), new Map([[file, staleSource]]))

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.abortedFiles).toEqual(['file'])
    expect(output.committedPaths).toEqual([])
  })

  it('binds an opened revision to the transfer job share instance', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[file, {
      source: {
        shareInstance: 'another-share',
        fileId: file,
        fileRevision: 'revision',
      },
    }]]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.begunFiles).toEqual([])
    expect(files.released).toEqual([file])
  })

  it('rejects a checkpoint that changes source revision after BeginFile validation', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const output = new CheckpointMismatchOutput()

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.abortedFiles).toEqual(['file'])
    expect(output.committedPaths).toEqual([])
  })

  it('rejects a checkpoint rebound to another output session', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const output = new CheckpointMismatchOutput('ownership')

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.abortedFiles).toEqual(['file'])
    expect(output.committedPaths).toEqual([])
  })

  it('aborts globally when a backend cannot roll back a failed file', async () => {
    const root = directoryId('root')
    const file = fileId('file')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: file, name: 'file', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[file, {
      transfer: async () => { throw new Error('transfer failed') },
    }]]))
    const output = new FakeOutput(
      true,
      new Map(),
      new Map(),
      new Error('rollback failed'),
    )

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(result.outcome.failures).toHaveLength(1)
    expect(output.abortJobCalls).toBe(1)
    expect(output.finished).toBeUndefined()
  })

  it('skips an unopened file even when started stream output cannot be rolled back', async () => {
    const root = directoryId('root')
    const missing = fileId('missing')
    const available = fileId('available')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: missing, name: 'missing', expectedSize: 1n },
      { kind: 'file', id: available, name: 'available', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[missing, {
      openError: new Error('source disappeared'),
    }]]))
    const output = new FakeOutput(false)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.committedPaths).toEqual(['available'])
    expect(output.abortJobCalls).toBe(0)
  })

  it('continues a stream job when its transaction fails before emitting bytes', async () => {
    const root = directoryId('root')
    const failed = fileId('failed')
    const available = fileId('available')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: failed, name: 'failed', expectedSize: 1n },
      { kind: 'file', id: available, name: 'available', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[failed, {
      transfer: async () => { throw new Error('failed before output') },
    }]]))
    const output = new FakeOutput(false)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(output.abortedFiles).toEqual(['failed'])
    expect(output.committedPaths).toEqual(['available'])
  })

  it('aborts the job when a streaming backend cannot isolate one file failure', async () => {
    const root = directoryId('root')
    const bad = fileId('bad')
    const later = fileId('later')
    const tree = treeWithRoot(root, [
      { kind: 'file', id: bad, name: 'bad', expectedSize: 1n },
      { kind: 'file', id: later, name: 'later', expectedSize: 1n },
    ])
    const files = new FakeFiles(new Map([[bad, {
      transfer: async (transaction) => {
        await transaction.writeRange(0n, Uint8Array.of(1))
        throw new Error('stream cannot continue')
      },
    }]]))
    const output = new FakeOutput(false)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      new FakeDiscovery(),
      files,
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(output.abortJobCalls).toBe(1)
    expect(output.finished).toBeUndefined()
    expect(output.abortedFiles).toContain('bad')
  })

  it('does not discover an unselected directory whose unknown descendants inherit false', async () => {
    const root = directoryId('root')
    const skipped = directoryId('skipped')
    const tree = treeWithRoot(root, [
      { kind: 'directory', id: skipped, name: 'skipped' },
    ])
    const discovery = new FakeDiscovery(new Map([[skipped, new Error('must not scan')]]))
    const output = new FakeOutput(true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(false),
      discovery,
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Succeeded')
    expect(discovery.calls).toEqual([])
    expect(output.ensuredDirectories).toEqual([])
  })

  it('preserves authenticated modification times at the output boundary', async () => {
    const root = directoryId('root')
    const directory = directoryId('directory')
    const file = fileId('file')
    const tree = treeWithRoot(root, [{
      kind: 'directory',
      id: directory,
      name: 'directory',
      modifiedTime: { milliseconds: 100n, precisionMilliseconds: 1_000n },
    }])
    const discovery = new FakeDiscovery(new Map<DirectoryId, DiscoveryResult>([[directory, {
      directoryId: directory,
      generation: 'directory',
      children: [{
        kind: 'file',
        id: file,
        name: 'file',
        expectedSize: 0n,
        modifiedTime: { milliseconds: 200n, precisionMilliseconds: 1_000n },
      }],
    }]]))
    const output = new FakeOutput(true, new Map(), new Map(), undefined, true)

    const result = await new TransferJob(
      tree,
      new SelectionRules(true),
      discovery,
      new FakeFiles(),
      output,
      { shareInstance: 'share', maximumConcurrentFiles: 1 },
    ).run()

    expect(result.outcome.status).toBe('Succeeded')
    expect(output.ensuredDirectoryValues[0]?.modifiedTimeMilliseconds).toBe(100n)
    expect(output.begunFiles[0]?.modifiedTimeMilliseconds).toBe(200n)
  })
})

type RootChild = CatalogDirectoryGeneration['children'][number]

function treeWithRoot(
  root: DirectoryId,
  children: readonly RootChild[],
): ProgressiveCatalogTree {
  const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
  tree.publishDirectory({ directoryId: root, generation: 'root', children })
  return tree
}

class FakeDiscovery implements DirectoryDiscoverySource {
  readonly calls: DirectoryId[] = []
  readonly #results: ReadonlyMap<
    DirectoryId,
    CatalogDirectoryGeneration | Promise<CatalogDirectoryGeneration> | Error
  >

  constructor(
    results: ReadonlyMap<
      DirectoryId,
      CatalogDirectoryGeneration | Promise<CatalogDirectoryGeneration> | Error
    > = new Map(),
  ) {
    this.#results = results
  }

  async listChildren(
    directory: CatalogDirectoryNode,
  ): Promise<DirectoryDiscoveryResult> {
    this.calls.push(directory.id)
    const result = this.#results.get(directory.id)
    if (result instanceof Error) {
      return {
        status: 'failed',
        failure: {
          attemptId: scanAttemptId(`attempt-${directory.id}`),
          kind: 'permanent',
          message: result.message,
        },
      }
    }
    if (result === undefined) {
      throw new Error(`unexpected directory ${directory.id}`)
    }
    return { status: 'ready', generation: await result }
  }
}

type DiscoveryResult = CatalogDirectoryGeneration | Promise<CatalogDirectoryGeneration> | Error

interface FileBehavior {
  readonly transfer?: PreparedFileTransfer['transfer']
  readonly openError?: unknown
  readonly source?: OutputSourceIdentity
}

class FakeFiles implements FileTransferService {
  readonly opened: FileId[] = []
  readonly released: FileId[] = []
  readonly #behaviors: ReadonlyMap<FileId, FileBehavior>

  constructor(behaviors: ReadonlyMap<FileId, FileBehavior> = new Map()) {
    this.#behaviors = behaviors
  }

  async open(file: CatalogFileNode): Promise<PreparedFileTransfer> {
    this.opened.push(file.id)
    const behavior = this.#behaviors.get(file.id)
    if (behavior?.openError !== undefined) {
      throw behavior.openError
    }
    return {
      source: behavior?.source ?? {
        shareInstance: 'share',
        fileId: file.id,
        fileRevision: `revision-${file.id}`,
      },
      exactSize: file.expectedSize,
      transfer: behavior?.transfer ?? (async (transaction, durable) => {
        const whole = byteRange(0n, file.expectedSize)
        if (!durable.covers(whole) && file.expectedSize > 0n) {
          await transaction.writeRange(0n, new Uint8Array(Number(file.expectedSize)))
          await transaction.checkpoint()
        }
      }),
      release: async () => {
        this.released.push(file.id)
      },
    }
  }
}

class FakeOutput implements OutputSession {
  readonly identity: OutputSessionIdentity = outputSessionIdentity({
    backend: 'fake',
    outputSessionId: 'output-session',
  })
  readonly capabilities: OutputCapabilities
  readonly ensuredDirectories: string[] = []
  readonly ensuredDirectoryValues: OutputDirectory[] = []
  readonly finalizedDirectories: string[] = []
  readonly begunFiles: OutputFile[] = []
  readonly committedPaths: string[] = []
  readonly abortedFiles: string[] = []
  readonly #initial: ReadonlyMap<FileId, readonly { start: bigint; end: bigint }[]>
  readonly #sourceOverrides: ReadonlyMap<FileId, OutputSourceIdentity>
  readonly #transactionAbortFailure: unknown | undefined
  finished: JobOutcome | undefined
  abortJobCalls = 0

  constructor(
    fileFailureIsolation: boolean,
    initial: ReadonlyMap<FileId, readonly { start: bigint; end: bigint }[]> = new Map(),
    sourceOverrides: ReadonlyMap<FileId, OutputSourceIdentity> = new Map(),
    transactionAbortFailure?: unknown,
    modificationTime = false,
  ) {
    this.capabilities = outputCapabilities({
      durability: fileFailureIsolation ? 'ProcessRestart' : 'None',
      randomWrite: fileFailureIsolation,
      fileFailureIsolation,
      modificationTime,
    })
    this.#initial = initial
    this.#sourceOverrides = sourceOverrides
    this.#transactionAbortFailure = transactionAbortFailure
  }

  async ensureDirectory(directory: OutputDirectory): Promise<void> {
    this.ensuredDirectories.push(directory.path.join('/'))
    this.ensuredDirectoryValues.push(directory)
  }

  async finalizeDirectory(directory: OutputDirectory): Promise<void> {
    this.finalizedDirectories.push(directory.path.join('/'))
  }

  async beginFile(file: OutputFile): Promise<BeginOutputFileResult> {
    this.begunFiles.push(file)
    const fileId = file.source.fileId as FileId
    const initial = this.#initial.get(fileId) ?? []
    const transaction = new FakeTransaction(
      file,
      this,
      initial,
      this.#transactionAbortFailure,
    )
    return {
      transaction,
      durableRanges: new VerifiedDurableRanges(
        outputOwnership(this, file),
        this.#sourceOverrides.get(fileId) ?? file.source,
        file.exactSize,
        initial,
      ),
    }
  }

  async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    this.finished = outcome
  }

  async abortJob(): Promise<void> {
    this.abortJobCalls += 1
  }
}

class BlockingFinishOutput extends FakeOutput {
  readonly finishStarted = deferred<void>()

  constructor() {
    super(true)
  }

  override async finishJob(outcome: JobOutcome, signal: AbortSignal): Promise<void> {
    this.finishStarted.resolve()
    if (signal.aborted) throw signal.reason
    await new Promise<void>((_resolve, reject) => {
      signal.addEventListener('abort', () => reject(signal.reason), { once: true })
    })
    this.finished = outcome
  }
}

class CheckpointMismatchOutput extends FakeOutput {
  readonly #mismatch: 'source' | 'ownership'

  constructor(mismatch: 'source' | 'ownership' = 'source') {
    super(true)
    this.#mismatch = mismatch
  }

  override async beginFile(file: OutputFile): Promise<BeginOutputFileResult> {
    const begun = await super.beginFile(file)
    const transaction: OutputFileTransaction = {
      writeRange: (offset, data) => begun.transaction.writeRange(offset, data),
      checkpoint: async () => new VerifiedDurableRanges(
        this.#mismatch === 'ownership'
          ? { ...begun.durableRanges.ownership, outputSessionId: 'wrong-session' }
          : begun.durableRanges.ownership,
        this.#mismatch === 'source'
          ? { ...file.source, fileRevision: 'wrong-revision' }
          : file.source,
        file.exactSize,
        [],
      ),
      commit: () => begun.transaction.commit(),
      abort: (reason) => begun.transaction.abort(reason),
    }
    return { transaction, durableRanges: begun.durableRanges }
  }
}

class FakeTransaction implements OutputFileTransaction {
  readonly #file: OutputFile
  readonly #output: FakeOutput
  readonly #written: Array<{ start: bigint; end: bigint }> = []
  readonly #initial: readonly { start: bigint; end: bigint }[]
  readonly #abortFailure: unknown | undefined

  constructor(
    file: OutputFile,
    output: FakeOutput,
    initial: readonly { start: bigint; end: bigint }[],
    abortFailure?: unknown,
  ) {
    this.#file = file
    this.#output = output
    this.#initial = initial
    this.#abortFailure = abortFailure
  }

  async writeRange(offset: bigint, data: Uint8Array): Promise<void> {
    this.#written.push({ start: offset, end: offset + BigInt(data.byteLength) })
  }

  async checkpoint(): Promise<VerifiedDurableRanges> {
    const initial = new VerifiedDurableRanges(
      outputOwnership(this.#output, this.#file),
      this.#file.source,
      this.#file.exactSize,
      [...this.#initial, ...this.#written].map(({ start, end }) => byteRange(start, end)),
    )
    return initial
  }

  async commit(): Promise<void> {
    this.#output.committedPaths.push(this.#file.path.join('/'))
  }

  async abort(): Promise<FileAbortDisposition> {
    this.#output.abortedFiles.push(this.#file.path.join('/'))
    if (this.#abortFailure !== undefined) {
      throw this.#abortFailure
    }
    return this.#output.capabilities.fileFailureIsolation || this.#written.length === 0
      ? 'FileIsolated'
      : 'JobOutputCompromised'
  }
}

function outputOwnership(output: FakeOutput, file: OutputFile): OutputFileOwnership {
  return {
    ...output.identity,
    canonicalPath: file.path,
    ownedFileIdentity: `owned-${file.source.fileId}`,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((innerResolve) => {
    resolve = innerResolve
  })
  return { promise, resolve }
}
