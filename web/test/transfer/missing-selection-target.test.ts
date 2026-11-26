import { describe, expect, it, vi } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { frozenSelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { discoverSelection, type MissingSelectionTarget } from '../../src/transfer/discovery'
import { TransferJob } from '../../src/transfer/job'
import type { TransferOutputSession } from '../../src/transfer/output-port'
import { id, makeShareFixture, outputFile } from './share-fixture'

function makeMultiDirCatalog(options: Readonly<{
  rootEntries: readonly CatalogEntry[]
  subdirectories?: ReadonlyMap<string, readonly CatalogEntry[]>
  failingDirs?: ReadonlySet<string>
}>) {
  const shareInstance = id(1)
  const rootId = id(2)
  const rootIdText = encodeBase64Url(rootId)
  const makeDir = (dirId: Uint8Array<ArrayBuffer>, entries: readonly CatalogEntry[]) => {
    const dirIdText = encodeBase64Url(dirId)
    const generation = id(3)
    const committed: CommittedDirectory = Object.freeze({
      directoryId: dirId,
      directoryIdText: dirIdText,
      generation,
      generationText: encodeBase64Url(generation),
      pageCount: 1,
      entryCount: entries.length,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    })
    const page: CatalogPage = Object.freeze({
      shareInstance,
      directoryId: dirId,
      directoryIdText: dirIdText,
      generation,
      generationText: committed.generationText,
      pageIndex: 0,
      terminal: true,
      previousCommitment: new Uint8Array(32),
      entries: Object.freeze([...entries]),
      omittedCount: 0n,
      objectCommitment: new Uint8Array(32),
      encodedBytes: 64,
    })
    return { committed, page }
  }

  const dirs = new Map([[rootIdText, makeDir(rootId, options.rootEntries)]])
  for (const [dirText, entries] of options.subdirectories ?? []) {
    const rawEntry = options.rootEntries.find(e => e.kind === 'directory' && e.idText === dirText)
    dirs.set(dirText, makeDir(rawEntry?.id ?? id(9), entries))
  }

  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: rootIdText,
  }) as unknown as ShareDescriptor

  const catalog = Object.freeze({
    loadDirectory: async (directoryId: Uint8Array) => {
      const key = encodeBase64Url(directoryId)
      if (options.failingDirs?.has(key)) throw new Error(`load failed for ${key}`)
      return dirs.get(key)!.committed
    },
    page: async (committed: CommittedDirectory) => dirs.get(committed.directoryIdText)!.page,
  }) as unknown as CatalogClient

  return { descriptor, catalog }
}

function makeOutputSession(overrides: Partial<TransferOutputSession> = {}): TransferOutputSession {
  return {
    sessionId: 'missing-selection-test',
    capabilities: { maxConcurrentFiles: 2, resumable: true, partialOutput: true },
    openFile: async () => outputFile({}),
    ...overrides,
  }
}

describe('missing explicit selection targets', () => {
  it('reports unmatched positive targets only after complete catalog traversal and suppresses them on skipped or aborted walks', async () => {
    const subDirId = id(10)
    const subDirText = encodeBase64Url(subDirId)
    const missingFileId = id(20)
    const missingDirId = id(21)
    const selection = frozenSelectionPolicy(false, [
      { kind: 'file', id: missingFileId, selected: true },
      { kind: 'directory', id: missingDirId, selected: true },
    ])

    const healthy = makeMultiDirCatalog({
      rootEntries: [Object.freeze({ kind: 'directory', id: subDirId, idText: subDirText, name: 'sub' })],
      subdirectories: new Map([[subDirText, []]]),
    })
    let reported: readonly MissingSelectionTarget[] | undefined
    const healthyItems: string[] = []
    for await (const item of discoverSelection(
      healthy.descriptor,
      healthy.catalog,
      selection,
      [],
      new AbortController().signal,
      { onUnmatchedTargets: targets => { reported = targets } },
    )) {
      healthyItems.push(item.kind)
    }
    expect(healthyItems).toEqual([])
    expect(reported).toEqual([
      { kind: 'directory', idText: encodeBase64Url(missingDirId) },
      { kind: 'file', idText: encodeBase64Url(missingFileId) },
    ])

    // When any reachable directory fails to load, the target might live inside it, so do not claim absence.
    const partialFail = makeMultiDirCatalog({
      rootEntries: [Object.freeze({ kind: 'directory', id: subDirId, idText: subDirText, name: 'sub' })],
      failingDirs: new Set([subDirText]),
    })
    let reportedOnDirFailure: readonly MissingSelectionTarget[] | undefined
    let failedDirs = 0
    const failedWalkItems: string[] = []
    for await (const item of discoverSelection(
      partialFail.descriptor,
      partialFail.catalog,
      selection,
      [],
      new AbortController().signal,
      {
        onDirectoryFailed: () => { failedDirs += 1 },
        onUnmatchedTargets: targets => { reportedOnDirFailure = targets },
      },
    )) {
      failedWalkItems.push(item.kind)
    }
    expect(failedWalkItems).toEqual([])
    expect(failedDirs).toBe(1)
    expect(reportedOnDirFailure).toBeUndefined()
  })

  it('settles TransferJob as source-changed when an explicitly selected single file is absent from the catalog', async () => {
    const fixture = makeShareFixture([
      { seed: 10, name: 'other.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
    ])
    const missingId = id(99)
    const selection = frozenSelectionPolicy(false, [{ kind: 'file', id: missingId, selected: true }])
    const job = new TransferJob({
      ...fixture,
      selection,
      output: makeOutputSession(),
    })

    const result = await job.run()
    expect(result.status).toBe('source-changed')
    expect(result.discoveryComplete).toBe(true)
    expect(result.completedFiles).toBe(0n)
    expect(result.failedFiles).toBe(1n)
    expect(result.failedDirectories).toBe(0n)
    expect(result.sourceRevisionFailures).toBeUndefined()
    expect(job.progress().selectionErrors).toBe(1)
  })

  it('settles multi-file selection with one missing explicit file as partial on resumable output and source-changed on all-or-nothing output', async () => {
    const fixture = makeShareFixture([
      { seed: 10, name: 'present.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
    ])
    const presentId = id(10)
    const missingId = id(11)
    const selection = frozenSelectionPolicy(false, [
      { kind: 'file', id: presentId, selected: true },
      { kind: 'file', id: missingId, selected: true },
    ])

    const partialJob = new TransferJob({
      ...fixture,
      selection,
      output: makeOutputSession({ capabilities: { maxConcurrentFiles: 2, resumable: true, partialOutput: true } }),
    })
    const partialResult = await partialJob.run()
    expect(partialResult.status).toBe('partial')
    if (partialResult.status !== 'partial') throw new Error('expected partial result')
    expect(partialResult.partialReason).toBe('failures')
    expect(partialResult.completedFiles).toBe(1n)
    expect(partialResult.failedFiles).toBe(1n)
    expect(partialResult.sourceRevisionFailures).toBeUndefined()
    expect(partialJob.progress().selectionErrors).toBe(1)

    const allOrNothingResult = await new TransferJob({
      ...fixture,
      selection,
      output: makeOutputSession({ capabilities: { maxConcurrentFiles: 1, resumable: false, partialOutput: false } }),
    }).run()
    expect(allOrNothingResult.status).toBe('source-changed')
    expect(allOrNothingResult.failedFiles).toBe(1n)
  })

  it('accepts an unmatched explicit file target on resume when output checkCompleted already verified it locally', async () => {
    const fixture = makeShareFixture([
      { seed: 10, name: 'remaining.txt', bytes: new Uint8Array([5, 6, 7, 8]) },
    ])
    const remainingId = id(10)
    const priorDoneId = id(12)
    const priorDoneText = encodeBase64Url(priorDoneId)
    const selection = frozenSelectionPolicy(false, [
      { kind: 'file', id: remainingId, selected: true },
      { kind: 'file', id: priorDoneId, selected: true },
    ])
    const checkCompleted = vi.fn(async (fileId: string) => (
      fileId === priorDoneText ? Object.freeze({ size: 16n }) : undefined
    ))

    const job = new TransferJob({
      ...fixture,
      selection,
      output: makeOutputSession({ checkCompleted }),
    })
    const result = await job.run()
    expect(result.status).toBe('completed')
    expect(result.discoveredFiles).toBe(2n)
    expect(result.discoveredBytes).toBe(20n)
    expect(result.completedFiles).toBe(2n)
    expect(result.completedBytes).toBe(20n)
    expect(result.failedFiles).toBe(0n)
    expect(job.progress().selectionErrors).toBe(0)
    expect(checkCompleted).toHaveBeenCalledWith(priorDoneText, expect.any(AbortSignal))
  })

  it('keeps explicitly selected empty directories completing cleanly with zero selection errors', async () => {
    const emptyDirId = id(30)
    const emptyDirText = encodeBase64Url(emptyDirId)
    const catalogFixture = makeMultiDirCatalog({
      rootEntries: [Object.freeze({ kind: 'directory', id: emptyDirId, idText: emptyDirText, name: 'EmptyFolder' })],
      subdirectories: new Map([[emptyDirText, []]]),
    })
    const selection = frozenSelectionPolicy(false, [{ kind: 'directory', id: emptyDirId, selected: true }])
    const ensureList: (readonly string[])[] = []
    const job = new TransferJob({
      descriptor: catalogFixture.descriptor,
      catalog: catalogFixture.catalog,
      selection,
      revisions: { open: async () => { throw new Error('unexpected revision open') } },
      broker: { readRange: async function* () { yield { offset: 0n, data: new Uint8Array(0) } } },
      output: makeOutputSession({
        ensureDirectory: async req => { ensureList.push(req.path) },
      }),
    })

    const result = await job.run()
    expect(result.status).toBe('completed')
    expect(result.completedFiles).toBe(0n)
    expect(result.failedFiles).toBe(0n)
    expect(result.failedDirectories).toBe(0n)
    expect(job.progress().selectionErrors).toBe(0)
    expect(ensureList).toEqual([['EmptyFolder']])
  })
})
