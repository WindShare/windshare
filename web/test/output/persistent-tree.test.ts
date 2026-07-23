import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import type { OutputFile } from '../../src/transfer/output-session'
import type { CheckpointCrashPhase } from '../../src/output/persistent-tree/contracts'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'

const ACTIVE_SIGNAL = new AbortController().signal

const IDENTITY = Object.freeze({ backend: 'memory-tree', outputSessionId: 'session' })

describe('persistent tree output session', () => {
  it('publishes checkpoints only after data and journal durability in the required order', async () => {
    const events: string[] = []
    const tree = new MemoryOutputTree(events)
    const journal = new MemoryOutputJournal(events)
    const session = await open(tree, journal, (phase) => events.push(`cut:${phase}`))
    const begun = await session.beginFile(outputFile('file', 'revision', 3n))
    events.length = 0

    await begun.transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    const durable = await begun.transaction.checkpoint()

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
      const begun = await session.beginFile(file)

      if (phase === 'DataWritten') {
        await expect(begun.transaction.writeRange(0n, Uint8Array.of(1, 2)))
          .rejects.toBeInstanceOf(SimulatedCrash)
      } else {
        await begun.transaction.writeRange(0n, Uint8Array.of(1, 2))
        await expect(begun.transaction.checkpoint()).rejects.toBeInstanceOf(SimulatedCrash)
      }
      tree.crash()
      journal.crash()

      const recovered = await open(tree, journal)
      const reopened = await recovered.beginFile(file)
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
    const begun = await first.beginFile(file)
    await begun.transaction.writeRange(0n, Uint8Array.of(7))
    await begun.transaction.checkpoint()
    tree.authorizationError = new DOMException('denied', 'NotAllowedError')

    await expect(open(tree, journal)).rejects.toMatchObject({
      kind: 'authorization',
    })
    tree.authorizationError = undefined
    const recovered = await open(tree, journal)
    expect((await recovered.beginFile(file)).durableRanges.covers(byteRange(0n, 1n))).toBe(true)
  })

  it('suspends live handles without deleting verified restart ranges', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const file = outputFile('file', 'revision', 2n)
    const first = await open(tree, journal)
    const begun = await first.beginFile(file)
    await begun.transaction.writeRange(0n, Uint8Array.of(1))
    await begun.transaction.checkpoint()

    await first.suspendJob()
    const recovered = await open(tree, journal)
    expect((await recovered.beginFile(file)).durableRanges.ranges).toEqual([byteRange(0n, 1n)])
  })

  it('invalidates only the file whose revision binding changed', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const first = await open(tree, journal)
    await checkpoint(first, outputFile('a', 'revision-a', 1n), 1)
    await checkpoint(first, outputFile('b', 'revision-b', 1n), 2)
    const oldA = tree.fileIdentity(['a'])

    const recovered = await open(tree, journal)
    const changed = await recovered.beginFile(outputFile('a', 'revision-a2', 1n))
    const unchanged = await recovered.beginFile(outputFile('b', 'revision-b', 1n))

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
    const resized = await recovered.beginFile({ ...file, exactSize: 2n })
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

    await expect(recovered.beginFile(file)).rejects.toMatchObject({
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

  it('bounds open transactions and requires every nested parent to be journal-owned', async () => {
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
    })).rejects.toMatchObject({ kind: 'output-state' })

    const active = await session.beginFile(outputFile('active', 'revision-active', 0n))
    await expect(session.beginFile(outputFile('blocked', 'revision-blocked', 0n)))
      .rejects.toMatchObject({ kind: 'resource-limit' })
    await active.transaction.abort(new Error('release slot'))
    await session.ensureDirectory({ path: ['parent'] })
    const nested = await session.beginFile({
      ...outputFile('nested', 'revision-nested', 0n),
      path: ['parent', 'nested'],
    })
    await nested.transaction.commit()
  })

  it('isolates quota failure and admits another file afterward', async () => {
    const tree = new MemoryOutputTree()
    const journal = new MemoryOutputJournal()
    const session = await open(tree, journal)
    const failed = await session.beginFile(outputFile('failed', 'revision-failed', 1n))
    tree.writeError = new DOMException('quota', 'QuotaExceededError')
    await expect(failed.transaction.writeRange(0n, Uint8Array.of(1)))
      .rejects.toMatchObject({ name: 'QuotaExceededError' })
    await expect(failed.transaction.abort(new Error('quota'))).resolves.toBe('FileIsolated')

    const healthy = await session.beginFile(outputFile('healthy', 'revision-healthy', 1n))
    await healthy.transaction.writeRange(0n, Uint8Array.of(2))
    await expect(healthy.transaction.commit()).resolves.toBeUndefined()
  })

  it('materializes empty files and directories while applying only authorized owned mtimes', async () => {
    const tree = new MemoryOutputTree()
    tree.seedDirectory(['existing'])
    const session = await open(tree, new MemoryOutputJournal())
    await session.ensureDirectory({ path: ['empty'], modifiedTimeMilliseconds: 10n })
    await session.finalizeDirectory(
      { path: ['empty'], modifiedTimeMilliseconds: 10n },
      ACTIVE_SIGNAL,
    )
    await session.ensureDirectory({ path: ['existing'], modifiedTimeMilliseconds: 20n })
    await session.finalizeDirectory(
      { path: ['existing'], modifiedTimeMilliseconds: 20n },
      ACTIVE_SIGNAL,
    )
    const empty = await session.beginFile({
      ...outputFile('empty-file', 'revision-empty', 0n),
      modifiedTimeMilliseconds: 30n,
    })
    await empty.transaction.commit()

    expect(tree.has(['empty'])).toBe(true)
    expect(tree.has(['empty-file'])).toBe(true)
    expect(tree.directoryModificationTimes.get('empty')).toBe(10n)
    expect(tree.directoryModificationTimes.has('existing')).toBe(false)
    expect(tree.fileModificationTimes.get('empty-file')).toBe(30n)
  })

  it('rejects paths that could escape the selected capability root', async () => {
    const session = await open(new MemoryOutputTree(), new MemoryOutputJournal())
    await expect(session.beginFile({
      ...outputFile('safe', 'revision', 0n),
      path: ['..', 'escape'],
    })).rejects.toThrow('unsafe structural segment')
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

function outputFile(name: string, revision: string, exactSize: bigint): OutputFile {
  return {
    source: { shareInstance: 'share', fileId: name, fileRevision: revision },
    path: [name],
    exactSize,
  }
}

async function checkpoint(
  session: PersistentTreeOutputSession,
  file: OutputFile,
  value: number,
): Promise<void> {
  const begun = await session.beginFile(file)
  await begun.transaction.writeRange(0n, Uint8Array.of(value))
  await begun.transaction.checkpoint()
}

class SimulatedCrash extends Error {
  constructor(phase: CheckpointCrashPhase) {
    super(`crash after ${phase}`)
    this.name = 'SimulatedCrash'
  }
}
