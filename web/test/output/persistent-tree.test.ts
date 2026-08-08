import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome } from '../../src/transfer/outcome'
import {
  OutputDirectoryMutationError,
  OutputSessionCompromisedError,
  type OutputFile,
} from '../../src/transfer/output-session'
import {
  BoundaryFaultError,
  CheckpointFaultCode,
  FaultScope,
  SourceFaultCode,
  authorizeFileRetirement,
  sourceFault,
} from '../../src/transfer/fault'
import type { CheckpointCrashPhase } from '../../src/output/persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'
import {
  admitOutputDirectory,
  admittedOutputDirectory,
  admittedOutputFile,
  TEST_DIRECTORY_ADMISSION_SCOPE,
  testOutputIdentity,
  testOutputModifiedTime,
} from './admission-fixture'

const ACTIVE_SIGNAL = new AbortController().signal
const SUCCESS_OUTCOME = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)

const IDENTITY = Object.freeze({ backend: 'memory-tree', outputSessionId: 'session' })

describe('persistent tree output session', () => {
  it('publishes checkpoints only after data and journal durability in the required order', async () => {
    const events: string[] = []
    const tree = new MemoryOutputTree(events)
    const journal = new MemoryOutputJournal(events)
    const session = await open(tree, journal, (phase) => { events.push(`cut:${phase}`) })
    const begun = await beginAdmittedFile(session, outputFile('file', 'revision', 3n))
    events.length = 0

    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3), ACTIVE_SIGNAL)
    const durable = await begun.transaction.checkpoint(ACTIVE_SIGNAL)

    expect(durable.ranges).toEqual([byteRange(0n, 3n)])
    expect(events).toEqual([
      'data-write',
      'cut:DataWritten',
      'data-flush',
      'cut:DataFlushed',
      'journal-write',
      'cut:JournalWritten',
      'journal-flush',
      'cut:JournalFlushed',
      'journal-commit',
      'cut:CheckpointCommitted',
      'journal-reopen',
      'cut:CheckpointVerified',
    ])
  })

  for (const phase of [
    'DataWritten',
    'DataFlushed',
    'JournalWritten',
    'JournalFlushed',
    'CheckpointCommitted',
    'CheckpointVerified',
  ] as const) {
    it(`recovers conservatively after a ${phase} crash cut`, async () => {
      const tree = new MemoryOutputTree()
      const journal = new MemoryOutputJournal()
      const session = await open(tree, journal, (current) => {
        if (current === phase) throw new SimulatedCrash(phase)
      })
      const file = outputFile('file', 'revision', 4n)
      const begun = await beginAdmittedFile(session, file)

      if (phase === 'DataWritten') {
        await expect(begun.transaction.writeRange(0n, Uint8Array.of(1, 2), ACTIVE_SIGNAL))
          .rejects.toBeInstanceOf(SimulatedCrash)
      } else {
        await begun.transaction.writeRange(0n, Uint8Array.of(1, 2), ACTIVE_SIGNAL)
        await expect(begun.transaction.checkpoint(ACTIVE_SIGNAL)).rejects.toBeInstanceOf(SimulatedCrash)
      }
      tree.crash()
      journal.crash()

      const recovered = await open(tree, journal)
      const reopened = await beginAdmittedFile(recovered, file)
      expect(reopened.durableRanges.ranges).toEqual(
        phase === 'CheckpointCommitted' || phase === 'CheckpointVerified'
          ? [byteRange(0n, 2n)]
          : [],
      )
    })
  }

  it('requires reauthorization before exposing persisted ranges', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 1n)
    const first = await open(tree, journal)
    const begun = await beginAdmittedFile(first, file)
    await begun.transaction.writeRange(0n, Uint8Array.of(7), ACTIVE_SIGNAL)
    await begun.transaction.checkpoint(ACTIVE_SIGNAL)
    tree.authorizationError = new DOMException('denied', 'NotAllowedError')

    await expect(open(tree, journal)).rejects.toMatchObject({
      kind: 'authorization',
    })
    tree.authorizationError = undefined
    const recovered = await open(tree, journal)
    expect((await beginAdmittedFile(recovered, file)).durableRanges.covers(byteRange(0n, 1n))).toBe(true)
  })

  it('suspends live handles without deleting verified restart ranges', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 2n)
    const first = await open(tree, journal)
    const begun = await beginAdmittedFile(first, file)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
    await begun.transaction.checkpoint(ACTIVE_SIGNAL)

    await first.pauseJob(new Error('test pause'))
    const recovered = await open(tree, journal)
    expect((await beginAdmittedFile(recovered, file)).durableRanges.ranges).toEqual([byteRange(0n, 1n)])
  })

  it('rejects a revision mismatch without deleting its file or checkpoint', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const first = await open(tree, journal)
    await checkpoint(first, outputFile('a', 'revision-a', 1n), 1)
    await checkpoint(first, outputFile('b', 'revision-b', 1n), 2)
    const oldA = tree.fileIdentity(['a'])

    const recovered = await open(tree, journal)
    const changed = await beginAdmittedFile(recovered, outputFile('a', 'revision-a2', 1n))
      .then(() => undefined, (error: unknown) => error)
    const unchanged = await beginAdmittedFile(recovered, outputFile('b', 'revision-b', 1n))

    expectCheckpointAttention(changed)
    expect(tree.fileIdentity(['a'])).toBe(oldA)
    expect(journal.hasCommitted('file:a')).toBe(true)
    expect(unchanged.durableRanges.ranges).toEqual([byteRange(0n, 1n)])
  })

  it('binds a checkpoint to exact durable identity while allowing a fresh runtime session', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 1n)
    await checkpoint(await open(tree, journal), file, 1)

    const freshRun = await PersistentTreeOutputSession.open({
      identity: { backend: 'memory-tree', outputSessionId: 'another-session' },
      directoryAdmissionScope: TEST_DIRECTORY_ADMISSION_SCOPE,
      tree,
      journal,
    })
    expect((await beginAdmittedFile(freshRun, file)).durableRanges.ranges).toEqual([
      byteRange(0n, 1n),
    ])

    const recovered = await open(tree, journal)
    const resized = await beginAdmittedFile(recovered, { ...file, exactSize: 2n })
      .then(() => undefined, (error: unknown) => error)
    expectCheckpointAttention(resized)
    expect(journal.hasCommitted('file:file')).toBe(true)
    expect(tree.has(file.path)).toBe(true)
  })

  it('never treats a same-path replacement as journal-owned output', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 1n)
    await checkpoint(await open(tree, journal), file, 4)
    const replacement = Uint8Array.of(99)
    tree.replaceFile(file.path, replacement)
    const replacementIdentity = tree.fileIdentity(file.path)
    const recovered = await open(tree, journal)

    await expect(beginAdmittedFile(recovered, file)).rejects.toMatchObject({
      fault: {
        domain: 'checkpoint',
        scope: FaultScope.OutputPause,
        code: CheckpointFaultCode.OwnershipMismatch,
      },
    })
    expect(tree.has(file.path)).toBe(true)
    expect(tree.fileIdentity(file.path)).toBe(replacementIdentity)
    expect(journal.hasCommitted('file:file')).toBe(true)
  })

  it('rejects a forged durable range whose journal checksum no longer matches', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 2n)
    await checkpoint(await open(tree, journal), file, 1)
    journal.corruptCommitted('file:file', (record) => ({
      ...record,
      durableRanges: [byteRange(0n, 2n)],
    }) as typeof record)

    await expect(open(tree, journal)).rejects.toMatchObject({ kind: 'journal-binding' })
  })

  it('bounds open transactions and rejects files without an admitted parent', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await PersistentTreeOutputSession.open({
      identity: IDENTITY,
      directoryAdmissionScope: TEST_DIRECTORY_ADMISSION_SCOPE,
      tree,
      journal,
      maximumOpenFiles: 1,
    })
    await expect(session.beginFile({
      ...outputFile('nested', 'revision-nested', 0n),
      path: ['parent', 'nested'],
    }, ACTIVE_SIGNAL)).rejects.toThrow(/missing, forged, or mismatched/u)

    const active = await beginAdmittedFile(session, outputFile('active', 'revision-active', 0n))
    await expect(beginAdmittedFile(session, outputFile('blocked', 'revision-blocked', 0n)))
      .rejects.toMatchObject({ kind: 'resource-limit' })
    await expect(active.transaction.retire(new Error('release slot')))
      .rejects.toThrow(/allowlisted authorization/u)
    await active.transaction.retire(permanentRetirement())
    await admitOutputDirectory(session, { path: ['parent'] })
    const nested = await beginAdmittedFile(session, {
      ...outputFile('nested', 'revision-nested', 0n),
      path: ['parent', 'nested'],
    })
    await nested.transaction.commit(ACTIVE_SIGNAL)
  })

  it('preserves quota-failed output because transient errors have no retirement authority', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const failed = await beginAdmittedFile(session, outputFile('failed', 'revision-failed', 1n))
    tree.writeError = new DOMException('quota', 'QuotaExceededError')
    await expect(failed.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL))
      .rejects.toMatchObject({ name: 'QuotaExceededError' })
    await expect(failed.transaction.retire(new Error('quota')))
      .rejects.toThrow(/allowlisted authorization/u)
    await session.pauseJob(new Error('quota pause'))
    expect(tree.has(['failed'])).toBe(true)
    expect(journal.hasCommitted('file:failed')).toBe(true)
  })

  it('materializes empty files and directories while applying only authorized owned mtimes', async () => {
    const tree = new MemoryOutputTree()
    tree.seedDirectory(['existing'])
    const session = await open(tree, new MemoryOutputJournal())
    const emptyDirectory = await admittedOutputDirectory(
      session,
      { path: ['empty'], modifiedTime: testOutputModifiedTime(10n) },
    )
    await session.finalizeDirectory(
      emptyDirectory,
      ACTIVE_SIGNAL,
    )
    const existingDirectory = await admittedOutputDirectory(
      session,
      { path: ['existing'], modifiedTime: testOutputModifiedTime(20n) },
    )
    await session.finalizeDirectory(
      existingDirectory,
      ACTIVE_SIGNAL,
    )
    const empty = await beginAdmittedFile(session, {
      ...outputFile('empty-file', 'revision-empty', 0n),
      modifiedTime: testOutputModifiedTime(30n),
    })
    await empty.transaction.commit(ACTIVE_SIGNAL)

    expect(tree.has(['empty'])).toBe(true)
    expect(tree.has(['empty-file'])).toBe(true)
    expect(tree.directoryModificationTimes.get('empty')).toBe(10n)
    expect(tree.directoryModificationTimes.has('existing')).toBe(false)
    expect(tree.fileModificationTimes.get('empty-file')).toBe(30n)
  })

  it('closes and removes a newly created file when cancellation wins acquisition', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const file = await admittedOutputFile(session, outputFile('cancelled', 'revision', 1n))
    const controller = new AbortController()
    const cancelled = new DOMException('cancel after create', 'AbortError')
    tree.afterCreateFile = () => controller.abort(cancelled)

    await expect(session.beginFile(file, controller.signal)).rejects.toBe(cancelled)

    expect(tree.activeFileHandles).toBe(0)
    expect(tree.has(file.path)).toBe(false)
    expect(journal.hasCommitted('file:cancelled')).toBe(false)
  })

  it('closes a reopened handle when size validation fails without deleting its retry record', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('retry', 'revision', 1n)
    const first = await open(tree, journal)
    await checkpoint(first, file, 1)
    await first.pauseJob(new Error('test pause'))
    const recovered = await open(tree, journal)
    const sizeFailure = new Error('size query failed')
    tree.sizeError = sizeFailure

    await expect(beginAdmittedFile(recovered, file)).rejects.toBe(sizeFailure)

    expect(tree.activeFileHandles).toBe(0)
    expect(journal.hasCommitted('file:retry')).toBe(true)
  })

  it('rolls back a child directory cancelled immediately after materialization', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    const controller = new AbortController()
    const cancelled = new DOMException('cancel directory acquisition', 'AbortError')
    tree.afterEnsureDirectory = () => controller.abort(cancelled)

    await expect(session.admitDirectory(directoryRequest(['cancelled'], root), controller.signal)).rejects.toBe(cancelled)

    expect(tree.has(['cancelled'])).toBe(false)
    expect(journal.hasCommitted('directory:cancelled')).toBe(false)
  })

  it('rolls back a child directory when atomic journal publication fails', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    const publicationFailure = new Error('journal commit failed')
    journal.commitError = publicationFailure

    await expect(session.admitDirectory(directoryRequest(['unpublished'], root), ACTIVE_SIGNAL))
      .rejects.toBe(publicationFailure)

    expect(tree.has(['unpublished'])).toBe(false)
    expect(journal.hasCommitted('directory:unpublished')).toBe(false)
  })

  it('retains a committed directory record when cleanup cannot prove physical removal', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    tree.validateDirectoryError = new Error('directory verification failed')
    tree.removeDirectoryError = new Error('directory removal failed')

    const failure = await session.admitDirectory(directoryRequest(['retained'], root), ACTIVE_SIGNAL)
      .then(() => undefined, (error: unknown) => error)

    expect(failure).toBeInstanceOf(OutputDirectoryMutationError)
    expect(failure).toMatchObject({ sessionCompromised: true })
    expect(tree.has(['retained'])).toBe(true)
    expect(journal.hasCommitted('directory:retained')).toBe(true)
  })

  it('retains a committed file record when acquisition cleanup cannot remove the file', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const file = await admittedOutputFile(session, outputFile('retained', 'revision', 1n))
    tree.openFileError = new Error('file verification failed')
    tree.removeFileError = new Error('file removal failed')

    const failure = await session.beginFile(file, ACTIVE_SIGNAL)
      .then(() => undefined, (error: unknown) => error)

    expect(failure).toBeInstanceOf(OutputSessionCompromisedError)
    expect(tree.activeFileHandles).toBe(0)
    expect(tree.has(file.path)).toBe(true)
    expect(journal.hasCommitted('file:retained')).toBe(true)
  })

  it('rejects paths that could escape the selected capability root', async () => {
    const session = await open(new MemoryOutputTree(), new MemoryOutputJournal())
    await expect(session.beginFile({
      ...outputFile('safe', 'revision', 0n),
      path: ['..', 'escape'],
    }, ACTIVE_SIGNAL)).rejects.toThrow('frozen path policy')
  })
})

describe('persistent tree resume retirement authority', () => {
  it.each([
    ['below the durable high-water mark', Uint8Array.of(1)],
    ['above the expected size', Uint8Array.of(1, 2, 3, 4)],
  ] as const)('preserves a same-identity file whose live size is %s', async (_name, liveBytes) => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('sized', 'revision', 3n)
    const first = await open(tree, journal)
    const begun = await beginAdmittedFile(first, file)
    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2), ACTIVE_SIGNAL)
    await begun.transaction.checkpoint(ACTIVE_SIGNAL)
    await first.pauseJob(new Error('restart'))
    const ownedIdentity = tree.fileIdentity(file.path)
    tree.resizeOwnedFile(file.path, liveBytes)

    const recovered = await open(tree, journal)
    const failure = await beginAdmittedFile(recovered, file)
      .then(() => undefined, (error: unknown) => error)

    expectCheckpointAttention(failure)
    expect(tree.fileIdentity(file.path)).toBe(ownedIdentity)
    expect(tree.has(file.path)).toBe(true)
    expect(journal.hasCommitted('file:sized')).toBe(true)
    await expect(recovered.pauseJob(failure)).resolves.toMatchObject({ kind: 'NeedsAttention' })
  })

  it('preserves published output through mismatch and typed source retirement', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('published', 'revision', 1n)
    const first = await open(tree, journal)
    const begun = await beginAdmittedFile(first, file)
    await begun.transaction.writeRange(0n, Uint8Array.of(7), ACTIVE_SIGNAL)
    await begun.transaction.checkpoint(ACTIVE_SIGNAL)
    await begun.transaction.commit(ACTIVE_SIGNAL)
    const publishedIdentity = tree.fileIdentity(file.path)

    const mismatched = await open(tree, journal)
    const mismatch = await beginAdmittedFile(
      mismatched,
      outputFile('published', 'another-revision', 1n),
    ).then(() => undefined, (error: unknown) => error)
    expectCheckpointAttention(mismatch)
    expect(tree.fileIdentity(file.path)).toBe(publishedIdentity)
    expect(journal.hasCommitted('file:published')).toBe(true)

    const recovered = await open(tree, journal)
    const reopened = await beginAdmittedFile(recovered, file)
    await expect(reopened.transaction.retire(permanentRetirement())).resolves.toBe('FileIsolated')
    expect(tree.fileIdentity(file.path)).toBe(publishedIdentity)
    expect(journal.hasCommitted('file:published')).toBe(true)
  })

  it('refuses to delete a same-path replacement during authorized retirement', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('replacement', 'revision', 1n)
    const session = await open(tree, journal)
    const begun = await beginAdmittedFile(session, file)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
    await begun.transaction.checkpoint(ACTIVE_SIGNAL)
    tree.replaceFile(file.path, Uint8Array.of(9))
    const replacementIdentity = tree.fileIdentity(file.path)

    await expect(begun.transaction.retire(permanentRetirement()))
      .rejects.toThrow('Could not isolate failed persistent output file')
    expect(tree.fileIdentity(file.path)).toBe(replacementIdentity)
    expect(journal.hasCommitted('file:replacement')).toBe(true)
  })

  it('seals directory authority while a reserved BeginFile drains to an active transaction', async () => {
    const tree = new MemoryOutputTree()
    const createStarted = deferred<void>()
    const releaseCreate = deferred<void>()
    tree.beforeCreateFile = async () => {
      createStarted.resolve()
      await releaseCreate.promise
    }
    const session = await open(tree, new MemoryOutputJournal())
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    const file = { ...outputFile('reserved', 'revision', 0n), parentAdmission: root }

    const beginning = session.beginFile(file, ACTIVE_SIGNAL)
    await createStarted.promise
    const finalizing = session.finalizeDirectory(root, ACTIVE_SIGNAL)

    await expect(session.beginFile({
      ...outputFile('late', 'revision', 0n),
      parentAdmission: root,
    }, ACTIVE_SIGNAL)).rejects.toThrow(/sealed/u)
    await expectPending(finalizing)

    releaseCreate.resolve()
    const begun = await beginning
    await expectPending(finalizing)
    await begun.transaction.pause(new Error('settle reserved file'))
    await expect(finalizing).resolves.toMatchObject({ kind: 'Finalized' })
  })

  it('keeps the first terminal transition idempotent across complete and pause', async () => {
    const completing = await open(new MemoryOutputTree(), new MemoryOutputJournal())
    const completion = completing.completeJob(SUCCESS_OUTCOME, ACTIVE_SIGNAL)
    const pauseDuringCompletion = completing.pauseJob(new Error('late pause'))
    await expect(completion).resolves.toMatchObject({ kind: 'Completed' })
    await expect(pauseDuringCompletion).resolves.toMatchObject({ kind: 'Completed' })
    await expect(completing.pauseJob(new Error('settled pause')))
      .resolves.toMatchObject({ kind: 'Completed' })

    const pausing = await open(new MemoryOutputTree(), new MemoryOutputJournal())
    const pause = pausing.pauseJob(new Error('first pause'))
    const completionDuringPause = pausing.completeJob(SUCCESS_OUTCOME, ACTIVE_SIGNAL)
    await expect(pause).resolves.toMatchObject({ kind: 'Paused' })
    await expect(completionDuringPause).resolves.toMatchObject({ kind: 'Paused' })
    await expect(pausing.completeJob(SUCCESS_OUTCOME, ACTIVE_SIGNAL))
      .resolves.toMatchObject({ kind: 'Paused' })
  })

  for (const race of [
    { phase: 'DataWritten', operation: 'write', close: 'pause' },
    { phase: 'DataFlushed', operation: 'checkpoint', close: 'pause' },
    { phase: 'CheckpointVerified', operation: 'commit', close: 'complete' },
  ] as const) {
    it(`drains ${race.operation} through ${race.phase} before ${race.close} settles`, async () => {
      const reached = deferred<void>()
      const release = deferred<void>()
      let blocked = false
      const session = await open(
        new MemoryOutputTree(),
        new MemoryOutputJournal(),
        async (phase) => {
          if (phase !== race.phase || blocked) return
          blocked = true
          reached.resolve()
          await release.promise
        },
      )
      const begun = await beginAdmittedFile(session, outputFile(race.operation, 'revision', 1n))
      if (race.operation !== 'write') {
        await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
      }
      let mutation: Promise<unknown>
      if (race.operation === 'write') {
        mutation = begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)
      } else if (race.operation === 'checkpoint') {
        mutation = begun.transaction.checkpoint(ACTIVE_SIGNAL)
      } else {
        mutation = begun.transaction.commit(ACTIVE_SIGNAL)
      }
      await reached.promise

      const closing = race.close === 'pause'
        ? session.pauseJob(new Error('deterministic lifecycle race'))
        : session.completeJob(SUCCESS_OUTCOME, ACTIVE_SIGNAL)
      await expectPending(closing)
      await expect(begun.transaction.checkpoint(ACTIVE_SIGNAL)).rejects.toThrow(/closing or closed/u)

      release.resolve()
      await mutation
      await expect(closing).resolves.toMatchObject({
        kind: race.close === 'pause' ? 'Paused' : 'Completed',
      })
    })
  }

  it('lets close cancel a finalization wait before pausing its active descendant', async () => {
    const session = await open(new MemoryOutputTree(), new MemoryOutputJournal())
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    await session.beginFile({
      ...outputFile('active-descendant', 'revision', 0n),
      parentAdmission: root,
    }, ACTIVE_SIGNAL)

    const finalizing = session.finalizeDirectory(root, ACTIVE_SIGNAL)
    await expectPending(finalizing)
    const pausing = session.pauseJob(new Error('close finalization wait'))

    await expect(finalizing).rejects.toThrow(/closing or closed/u)
    await expect(pausing).resolves.toMatchObject({ kind: 'Paused' })
  })

  it('drains directory metadata finalization before pause reaches its stable cut', async () => {
    const tree = new MemoryOutputTree()
    const metadataStarted = deferred<void>()
    const releaseMetadata = deferred<void>()
    tree.beforeDirectoryModification = async () => {
      metadataStarted.resolve()
      await releaseMetadata.promise
    }
    const session = await open(tree, new MemoryOutputJournal())
    const root = await session.admitDirectory(directoryRequest([]), ACTIVE_SIGNAL)
    const child = await session.admitDirectory({
      ...directoryRequest(['child'], root),
      modifiedTime: testOutputModifiedTime(1n),
    }, ACTIVE_SIGNAL)

    const finalizing = session.finalizeDirectory(child, ACTIVE_SIGNAL)
    await metadataStarted.promise
    const pausing = session.pauseJob(new Error('pause during directory metadata'))
    await expectPending(pausing)
    await expect(session.admitDirectory(directoryRequest(['late'], root), ACTIVE_SIGNAL))
      .rejects.toThrow(/closing or closed/u)

    releaseMetadata.resolve()
    await expect(finalizing).resolves.toMatchObject({ kind: 'Finalized' })
    await expect(pausing).resolves.toMatchObject({ kind: 'Paused' })
  })
})

async function open(
  tree: MemoryOutputTree,
  journal: MemoryOutputJournal,
  crashHook?: (phase: CheckpointCrashPhase) => void | Promise<void>,
): Promise<PersistentTreeOutputSession> {
  return PersistentTreeOutputSession.open({
    identity: IDENTITY,
    directoryAdmissionScope: TEST_DIRECTORY_ADMISSION_SCOPE,
    tree,
    journal,
    ...(crashHook === undefined ? {} : { crashHook }),
  })
}

async function beginAdmittedFile(session: PersistentTreeOutputSession, file: OutputFile) {
  return session.beginFile(await admittedOutputFile(session, file), ACTIVE_SIGNAL)
}

function outputFile(name: string, revision: string, exactSize: bigint): OutputFile {
  return {
    source: {
      shareInstance: testOutputIdentity('share'),
      fileId: testOutputIdentity(`file:${name}`),
      fileRevision: testOutputIdentity(`revision:${revision}`),
    },
    path: [name],
    exactSize,
  }
}

function directoryRequest(path: readonly string[], parentAdmission?: Awaited<ReturnType<PersistentTreeOutputSession['admitDirectory']>>) {
  const binding = path.join('/') || 'root'
  return {
    directoryId: path.length === 0
      ? TEST_DIRECTORY_ADMISSION_SCOPE.syntheticRoot
      : testOutputIdentity(`directory:${binding}`),
    generation: testOutputIdentity(`generation:${binding}`),
    path,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
  }
}

async function checkpoint(
  session: PersistentTreeOutputSession,
  file: OutputFile,
  value: number,
): Promise<void> {
  const begun = await beginAdmittedFile(session, file)
  await begun.transaction.writeRange(0n, Uint8Array.of(value), ACTIVE_SIGNAL)
  await begun.transaction.checkpoint(ACTIVE_SIGNAL)
}

function permanentRetirement() {
  const authorization = authorizeFileRetirement(
    sourceFault(FaultScope.FileLocal, SourceFaultCode.Permanent),
  )
  if (authorization === undefined) throw new Error('permanent source retirement was not authorized')
  return authorization
}

function expectCheckpointAttention(value: unknown): void {
  expect(value).toBeInstanceOf(BoundaryFaultError)
  expect(value).toMatchObject({
    fault: {
      domain: 'checkpoint',
      scope: FaultScope.OutputPause,
      code: CheckpointFaultCode.OwnershipMismatch,
    },
  })
}

class SimulatedCrash extends Error {
  constructor(phase: CheckpointCrashPhase) {
    super(`crash after ${phase}`)
    this.name = 'SimulatedCrash'
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
} {
  let resolve: (value: T | PromiseLike<T>) => void = () => undefined
  const promise = new Promise<T>((complete) => { resolve = complete })
  return { promise, resolve }
}

async function expectPending(promise: Promise<unknown>): Promise<void> {
  const state = await Promise.race([
    promise.then(() => 'settled' as const, () => 'settled' as const),
    Promise.resolve('pending' as const),
  ])
  expect(state).toBe('pending')
}
