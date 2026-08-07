import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import {
  OutputDirectoryMutationError,
  OutputSessionCompromisedError,
  type OutputFile,
} from '../../src/transfer/output-session'
import type { CheckpointCrashPhase } from '../../src/output/persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'
import {
  admitOutputDirectory,
  admittedOutputDirectory,
  admittedOutputFile,
  testOutputIdentity,
  testOutputModifiedTime,
} from './admission-fixture'

const ACTIVE_SIGNAL = new AbortController().signal

const IDENTITY = Object.freeze({ backend: 'memory-tree', outputSessionId: 'session' })

describe('persistent tree output session', () => {
  it('publishes checkpoints only after data and journal durability in the required order', async () => {
    const events: string[] = []
    const tree = new MemoryOutputTree(events)
    const journal = new MemoryOutputJournal(events)
    const session = await open(tree, journal, (phase) => events.push(`cut:${phase}`))
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

    await first.suspendJob()
    const recovered = await open(tree, journal)
    expect((await beginAdmittedFile(recovered, file)).durableRanges.ranges).toEqual([byteRange(0n, 1n)])
  })

  it('invalidates only the file whose revision binding changed', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const first = await open(tree, journal)
    await checkpoint(first, outputFile('a', 'revision-a', 1n), 1)
    await checkpoint(first, outputFile('b', 'revision-b', 1n), 2)
    const oldA = tree.fileIdentity(['a'])

    const recovered = await open(tree, journal)
    const changed = await beginAdmittedFile(recovered, outputFile('a', 'revision-a2', 1n))
    const unchanged = await beginAdmittedFile(recovered, outputFile('b', 'revision-b', 1n))

    expect(changed.durableRanges.ranges).toEqual([])
    expect(tree.fileIdentity(['a'])).not.toBe(oldA)
    expect(unchanged.durableRanges.ranges).toEqual([byteRange(0n, 1n)])
  })

  it('binds a checkpoint to exact size and output session identity', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 1n)
    await checkpoint(await open(tree, journal), file, 1)

    await expect(PersistentTreeOutputSession.open({
      identity: { backend: 'memory-tree', outputSessionId: 'another-session' },
      tree,
      journal,
    })).rejects.toMatchObject({ kind: 'journal-binding' })

    const recovered = await open(tree, journal)
    const resized = await beginAdmittedFile(recovered, { ...file, exactSize: 2n })
    expect(resized.durableRanges.ranges).toEqual([])
  })

  it('never treats a same-path replacement as journal-owned output', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 1n)
    await checkpoint(await open(tree, journal), file, 4)
    const replacement = Uint8Array.of(99)
    tree.replaceFile(file.path, replacement)
    const recovered = await open(tree, journal)

    await expect(beginAdmittedFile(recovered, file)).rejects.toMatchObject({
      kind: 'output-identity',
    })
    expect(tree.has(file.path)).toBe(true)
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
    await active.transaction.abort(new Error('release slot'))
    await admitOutputDirectory(session, { path: ['parent'] })
    const nested = await beginAdmittedFile(session, {
      ...outputFile('nested', 'revision-nested', 0n),
      path: ['parent', 'nested'],
    })
    await nested.transaction.commit(ACTIVE_SIGNAL)
  })

  it('isolates quota failure and admits another file afterward', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const failed = await beginAdmittedFile(session, outputFile('failed', 'revision-failed', 1n))
    tree.writeError = new DOMException('quota', 'QuotaExceededError')
    await expect(failed.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL))
      .rejects.toMatchObject({ name: 'QuotaExceededError' })
    await expect(failed.transaction.abort(new Error('quota'))).resolves.toBe('FileIsolated')

    const healthy = await beginAdmittedFile(session, outputFile('healthy', 'revision-healthy', 1n))
    await healthy.transaction.writeRange(0n, Uint8Array.of(2), ACTIVE_SIGNAL)
    await expect(healthy.transaction.commit(ACTIVE_SIGNAL)).resolves.toBeUndefined()
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
    await first.suspendJob()
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

async function open(
  tree: MemoryOutputTree,
  journal: MemoryOutputJournal,
  crashHook?: (phase: CheckpointCrashPhase) => void,
): Promise<PersistentTreeOutputSession> {
  return PersistentTreeOutputSession.open({
    identity: IDENTITY,
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
    directoryId: testOutputIdentity(`directory:${binding}`),
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

class SimulatedCrash extends Error {
  constructor(phase: CheckpointCrashPhase) {
    super(`crash after ${phase}`)
    this.name = 'SimulatedCrash'
  }
}
