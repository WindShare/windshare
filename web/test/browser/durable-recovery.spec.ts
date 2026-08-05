import { expect, test, type Page } from '@playwright/test'

import {
  requireOriginPrivateStorage,
} from './browser-storage-support'

interface DurableRecoveryHarness {
  createCheckpoint(outputSessionId: string): Promise<readonly string[]>
  reopenCheckpoint(outputSessionId: string): Promise<{
    readonly ranges: readonly string[]
    readonly coversPrefix: boolean
    readonly durability: string
  }>
  createPersistentHandleCheckpoint(outputSessionId: string): Promise<readonly string[]>
  reopenPersistentHandleCheckpoint(outputSessionId: string): Promise<readonly string[]>
  holdOutputSession(outputSessionId: string): Promise<void>
  competingSessionError(outputSessionId: string): Promise<string | undefined>
  releaseOutputSession(outputSessionId: string): Promise<void>
  completePersistentHandleOutput(outputSessionId: string): Promise<{
    readonly bytes: readonly number[]
    readonly metadataRetired: boolean
  }>
  completeOriginPrivateOutput(outputSessionId: string): Promise<{
    readonly exported: readonly number[]
    readonly reopenedRanges: readonly string[]
  }>
}

const HARNESS_PATH = '/test/browser/durable-recovery-harness.ts'

test.beforeEach(async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
})

test('OPFS journal is revalidated after a page reload', async ({ page }) => {
  await page.goto('/')
  const outputSessionId = `reload-${crypto.randomUUID()}`
  expect(await createCheckpoint(page, outputSessionId)).toEqual(['0:3'])

  await page.reload()
  expect(await reopenCheckpoint(page, outputSessionId)).toEqual({
    ranges: ['0:3'],
    coversPrefix: true,
    durability: 'ProcessRestart',
  })
})

test('a persisted FSA-like handle is reopened and identity-checked after reload', async ({ page }) => {
  await page.goto('/')
  const outputSessionId = `handle-${crypto.randomUUID()}`
  const created = await callHarness<readonly string[]>(
    page,
    outputSessionId,
    'createPersistentHandleCheckpoint',
  )
  expect(created).toEqual(['0:3'])

  await page.reload()
  const reopened = await callHarness<readonly string[]>(
    page,
    outputSessionId,
    'reopenPersistentHandleCheckpoint',
  )
  expect(reopened).toEqual(['0:3'])
})

test('one output session cannot publish competing checkpoint heads from two pages', async ({
  context,
  page,
}) => {
  await page.goto('/')
  const competitor = await context.newPage()
  await competitor.goto('/')
  const outputSessionId = `lease-${crypto.randomUUID()}`
  await callHarness<void>(page, outputSessionId, 'holdOutputSession')
  expect(await callHarness<string | undefined>(
    competitor,
    outputSessionId,
    'competingSessionError',
  )).toBe('InvalidStateError')
  await callHarness<void>(page, outputSessionId, 'releaseOutputSession')
  await competitor.close()
})

test('completed persistent output keeps bytes but retires journal and handle metadata', async ({ page }) => {
  await page.goto('/')
  const outputSessionId = `complete-${crypto.randomUUID()}`
  expect(await callHarness<{
    readonly bytes: readonly number[]
    readonly metadataRetired: boolean
  }>(page, outputSessionId, 'completePersistentHandleOutput')).toEqual({
    bytes: [1, 2, 3, 4, 5],
    metadataRetired: true,
  })
})

test('completed OPFS output exports before exact-session staging cleanup', async ({ page }) => {
  await page.goto('/')
  const outputSessionId = `opfs-complete-${crypto.randomUUID()}`
  expect(await callHarness<{
    readonly exported: readonly number[]
    readonly reopenedRanges: readonly string[]
  }>(page, outputSessionId, 'completeOriginPrivateOutput')).toEqual({
    exported: [1, 2, 3, 4, 5],
    reopenedRanges: [],
  })
})

test('serializes OPFS quota across independent realms and reclaims an expired crash lease', async ({ context, page }) => {
  const secondPage = await context.newPage()
  await Promise.all([page.goto('/'), secondPage.goto('/')])
  const databaseName = `admission-${crypto.randomUUID()}`
  const reserveBytes = 512 * 1024 * 1024

  await Promise.all([
    openAdmission(page, 'realm-a'),
    openAdmission(secondPage, 'realm-b'),
  ])
  const raced = await Promise.allSettled([
    reserve(page, 'file-a', 6),
    reserve(secondPage, 'file-b', 6),
  ])
  expect(raced.filter((result) => result.status === 'fulfilled')).toHaveLength(1)
  const rejected = raced.find((result): result is PromiseRejectedResult => result.status === 'rejected')
  expect(String(rejected?.reason)).toContain('shared browser quota reserve')
  await Promise.all([release(page), release(secondPage)])

  await page.evaluate(async ({ name, quota }) => {
    const admissionPath = '/src/output/origin-private/admission.ts'
    const admission = await import(admissionPath) as typeof import(
      '../../src/output/origin-private/admission'
    )
    ;(globalThis as Record<string, unknown>).__crashedAdmission =
      await admission.OriginPrivateStagingAdmission.open('crashed-realm', {
        logicalBytes: 5n,
        additionalBytes: 5n,
      }, {
        estimate: async () => ({ quota, usage: 0 }),
        admissionDatabaseName: name,
        now: () => 0,
        leaseMilliseconds: 1_000,
        heartbeatMilliseconds: 500,
      })
  }, { name: databaseName, quota: reserveBytes + 10 })
  await page.close()

  await secondPage.evaluate(async ({ name, quota }) => {
    const admissionPath = '/src/output/origin-private/admission.ts'
    const admission = await import(admissionPath) as typeof import(
      '../../src/output/origin-private/admission'
    )
    const recovered = await admission.OriginPrivateStagingAdmission.open('after-crash', {
      logicalBytes: 5n,
      additionalBytes: 5n,
    }, {
      estimate: async () => ({ quota, usage: 0 }),
      admissionDatabaseName: name,
      now: () => 2_000,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await recovered.release()
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.deleteDatabase(name)
      request.addEventListener('success', () => resolve(), { once: true })
      request.addEventListener('error', () => reject(request.error), { once: true })
    })
  }, { name: databaseName, quota: reserveBytes + 10 })
  await secondPage.close()

  async function openAdmission(target: typeof page, session: string): Promise<void> {
    await target.evaluate(async ({ name, sessionKey, quota }) => {
      const admissionPath = '/src/output/origin-private/admission.ts'
      const admission = await import(admissionPath) as typeof import(
        '../../src/output/origin-private/admission'
      )
      ;(globalThis as Record<string, unknown>).__outputAdmission =
        await admission.OriginPrivateStagingAdmission.open(sessionKey, {
          logicalBytes: 0n,
          additionalBytes: 0n,
        }, {
          estimate: async () => ({ quota, usage: 0 }),
          admissionDatabaseName: name,
          jobLimit: 100n,
          processLimit: 100n,
        })
    }, { name: databaseName, sessionKey: session, quota: reserveBytes + 10 })
  }

  async function reserve(target: typeof page, file: string, bytes: number): Promise<void> {
    await target.evaluate(async ({ path, size }) => {
      const admission = (globalThis as Record<string, unknown>).__outputAdmission as
        import('../../src/output/origin-private/admission').OriginPrivateStagingAdmission
      await admission.reserve([path], BigInt(size), { logicalBytes: 0n, coveredBytes: 0n })
    }, { path: file, size: bytes })
  }

  async function release(target: typeof page): Promise<void> {
    await target.evaluate(async () => {
      const admission = (globalThis as Record<string, unknown>).__outputAdmission as
        import('../../src/output/origin-private/admission').OriginPrivateStagingAdmission
      await admission.release()
    })
  }
})

test('sweeps expired ZIP spool namespaces after reload without deleting a live writer', async ({ context, page }) => {
  const databaseName = `spool-recovery-${crypto.randomUUID()}`
  await page.goto('/')
  await page.evaluate(async (name) => {
    const spoolPath = '/src/output/streams/zip-spool.ts'
    const spoolModule = await import(spoolPath) as typeof import(
      '../../src/output/streams/zip-spool'
    )
    const orphan = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'orphan',
      token: 'orphan-token',
      now: () => 0,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await orphan.append(new Uint8Array(256 * 1024))
    ;(globalThis as Record<string, unknown>).__orphanSpool = orphan
  }, databaseName)
  await page.close()

  const recoveredPage = await context.newPage()
  await recoveredPage.goto('/')
  const counts = await recoveredPage.evaluate(async (name) => {
    const spoolPath = '/src/output/streams/zip-spool.ts'
    const spoolModule = await import(spoolPath) as typeof import(
      '../../src/output/streams/zip-spool'
    )
    const live = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'live',
      token: 'live-token',
      now: () => 2_000,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await live.append(new Uint8Array(256 * 1024))
    const beforeClear = await countStores(name)
    await live.clear()
    const afterClear = await countStores(name)

    let clock = 0
    const stale = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'fenced',
      token: 'stale-token',
      now: () => clock,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await stale.append(new Uint8Array(256 * 1024).fill(1))
    clock = 2_000
    const replacement = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'fenced',
      token: 'replacement-token',
      now: () => clock,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await replacement.append(new Uint8Array(256 * 1024).fill(2))
    await stale.clear()
    const replacementManifest = await replacement.seal()
    const replacementChunk = await replacement.readChunk(0)
    await replacement.clear()
    const afterFencing = await countStores(name)

    const prefix = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'prefix',
      token: 'prefix-token',
      now: () => clock,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    const nested = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: name,
      namespace: 'prefix\0nested',
      token: 'nested-token',
      now: () => clock,
      leaseMilliseconds: 1_000,
      heartbeatMilliseconds: 500,
    })
    await prefix.append(new Uint8Array(256 * 1024).fill(3))
    await nested.append(new Uint8Array(256 * 1024).fill(4))
    await prefix.clear()
    const nestedManifest = await nested.seal()
    const nestedChunk = await nested.readChunk(0)
    await nested.clear()
    const afterStructuralFencing = await countStores(name)
    await deleteDatabase(name)
    return {
      beforeClear,
      afterClear,
      afterFencing,
      afterStructuralFencing,
      replacementChunkBytes: replacementChunk?.byteLength,
      replacementChunkMarker: replacementChunk?.[0],
      replacementRecords: replacementManifest.recordCount.toString(),
      nestedChunkMarker: nestedChunk?.[0],
      nestedRecords: nestedManifest.recordCount.toString(),
    }

    async function countStores(databaseName: string): Promise<readonly number[]> {
      const database = await openDatabase(databaseName)
      const transaction = database.transaction(
        ['central-directory-chunks', 'central-directory-namespaces'],
        'readonly',
      )
      const values = await Promise.all([
        count(transaction.objectStore('central-directory-chunks')),
        count(transaction.objectStore('central-directory-namespaces')),
      ])
      await done(transaction)
      database.close()
      return values
    }

    function openDatabase(databaseName: string): Promise<IDBDatabase> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.open(databaseName)
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }

    function count(store: IDBObjectStore): Promise<number> {
      return new Promise((resolve, reject) => {
        const request = store.count()
        request.onsuccess = () => resolve(request.result)
        request.onerror = () => reject(request.error)
      })
    }

    function done(transaction: IDBTransaction): Promise<void> {
      return new Promise((resolve, reject) => {
        transaction.addEventListener('complete', () => resolve(), { once: true })
        transaction.addEventListener('error', () => reject(transaction.error), { once: true })
      })
    }

    function deleteDatabase(databaseName: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(databaseName)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }
  }, databaseName)
  expect(counts.beforeClear).toEqual([1, 1])
  expect(counts.afterClear).toEqual([0, 0])
  expect(counts.afterFencing).toEqual([0, 0])
  expect(counts.afterStructuralFencing).toEqual([0, 0])
  expect(counts).toMatchObject({
    replacementChunkBytes: 256 * 1024,
    replacementChunkMarker: 2,
    replacementRecords: '1',
    nestedChunkMarker: 4,
    nestedRecords: '1',
  })
  await recoveredPage.close()
})

test('fails closed at IndexedDB blocked and versionchange boundaries', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const probePath = '/test/browser/durable-recovery-idb-probe.ts'
    const probe = await import(probePath) as typeof import('./durable-recovery-idb-probe')
    return probe.probeIndexedDbFailureBoundaries()
  })

  expect(result).toEqual({
    journalBlocked: 'InvalidStateError',
    journalLateConnectionClosed: true,
    journalVersionChange: 'InvalidStateError',
    admissionVersionChange: 'Error',
    zipBlocked: 'InvalidStateError',
    zipLateConnectionClosed: true,
    zipVersionChange: 'Error',
  })
})

test('reopens real IndexedDB journal pages lazily and removes a crash candidate', async ({ page }) => {
  test.setTimeout(60_000)
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const repositoryPath = '/src/output/browser/indexeddb-repository.ts'
    const journalPath = '/src/output/persistence/journal.ts'
    const persistentPath = '/src/output/persistent-tree/session.ts'
    const fakesPath = '/test/output/fakes.ts'
    const repositoryModule = await import(repositoryPath) as typeof import(
      '../../src/output/browser/indexeddb-repository'
    )
    const journal = await import(journalPath) as typeof import(
      '../../src/output/persistence/journal'
    )
    const persistent = await import(persistentPath) as typeof import(
      '../../src/output/persistent-tree/session'
    )
    const fakes = await import(fakesPath) as typeof import('../output/fakes')
    const databaseName = `journal-pages-${crypto.randomUUID()}`
    const backend = 'real-indexeddb-test'
    const outputSessionId = 'paged-session'
    const identity = { backend, outputSessionId }
    const tree = new fakes.MemoryOutputTree()
    let repository = await repositoryModule.IndexedDbOutputRepository.open(
      databaseName,
      backend,
      outputSessionId,
    )
    const recordCount = journal.OUTPUT_JOURNAL_PAGE_RECORD_LIMIT + 1
    for (let index = 0; index < recordCount; index += 1) {
      const path = [`f-${index.toString().padStart(6, '0')}`]
      const handle = await tree.createFileExclusive(path)
      const record = journal.fileRecord(
        identity,
        { ...identity, canonicalPath: path, ownedFileIdentity: handle.identity },
        {
          source: {
            shareInstance: 'paged-share',
            fileId: `file-${index}`,
            fileRevision: 'revision',
          },
          path,
          exactSize: 0n,
        },
        [],
        true,
        1n,
      )
      const key = journal.outputRecordKey(record)
      await repository.writeCandidate(record)
      await repository.flushCandidate(key)
      await repository.commitCandidate(key)
      await handle.close()
    }
    const crashPath = ['crash-candidate']
    const crashHandle = await tree.createFileExclusive(crashPath)
    const crashRecord = journal.fileRecord(
      identity,
      { ...identity, canonicalPath: crashPath, ownedFileIdentity: crashHandle.identity },
      {
        source: {
          shareInstance: 'paged-share',
          fileId: 'crash-file',
          fileRevision: 'revision',
        },
        path: crashPath,
        exactSize: 0n,
      },
      [],
      false,
      1n,
    )
    await repository.writeCandidate(crashRecord)
    await repository.flushCandidate(journal.outputRecordKey(crashRecord))
    await crashHandle.close()
    repository.close()

    repository = await repositoryModule.IndexedDbOutputRepository.open(
      databaseName,
      backend,
      outputSessionId,
    )
    const session = await persistent.PersistentTreeOutputSession.open({
      identity,
      tree,
      journal: repository,
    })
    const ascending = await scanJournal('ascending')
    const descending = await scanJournal('descending')
    let lazilyEnumerated = 0
    for await (const file of session.stagedCatalog().files()) {
      if (!file.record.committed) throw new Error('Lazy export exposed an uncommitted file')
      lazilyEnumerated += 1
    }
    const crashCandidateRemoved = !tree.has(crashPath)
    await repository.deleteSessionData()
    repository.close()
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.deleteDatabase(databaseName)
      request.addEventListener('success', () => resolve(), { once: true })
      request.addEventListener('error', () => reject(request.error), { once: true })
    })
    return {
      recordCount,
      scanned: ascending.keys.length,
      descendingScanned: descending.keys.length,
      lazilyEnumerated,
      pageSizes: ascending.pageSizes,
      descendingPageSizes: descending.pageSizes,
      maximumPage: Math.max(...ascending.pageSizes, ...descending.pageSizes),
      ascendingMonotonic: ascending.keys.every(
        (key, index, keys) => index === 0 || keys[index - 1]! < key,
      ),
      descendingMonotonic: descending.keys.every(
        (key, index, keys) => index === 0 || keys[index - 1]! > key,
      ),
      crashCandidateRemoved,
    }

    async function scanJournal(direction: 'ascending' | 'descending'): Promise<{
      readonly keys: string[]
      readonly pageSizes: number[]
    }> {
      const keys: string[] = []
      const pageSizes: number[] = []
      let cursor: string | undefined
      do {
        const scan = {
          kind: 'file' as const,
          direction,
          ...(cursor === undefined ? {} : { cursor }),
        }
        const pageValue = journal.validateOutputJournalPage(
          await repository.scanCommitted(scan),
          scan,
          identity,
        )
        pageSizes.push(pageValue.records.length)
        keys.push(...pageValue.records.map(journal.outputRecordKey))
        cursor = pageValue.nextCursor
      } while (cursor !== undefined)
      return { keys, pageSizes }
    }
  })

  expect(result).toMatchObject({
    scanned: result.recordCount,
    descendingScanned: result.recordCount,
    lazilyEnumerated: result.recordCount,
    maximumPage: 128,
    ascendingMonotonic: true,
    descendingMonotonic: true,
    crashCandidateRemoved: true,
  })
  expect(result.pageSizes.length).toBeGreaterThan(1)
  expect(result.descendingPageSizes.length).toBeGreaterThan(1)
})

test('converges marker-owned published OPFS staging after cleanup failure and reload', async ({ page }) => {
  await page.goto('/')
  const supported = await page.evaluate(() => {
    const storage = navigator.storage as (StorageManager & { getDirectory?: unknown }) | undefined
    return typeof storage?.getDirectory === 'function' && navigator.locks !== undefined
  })
  if (!supported) return
  const ids = {
    outputSessionId: `cleanup-${crypto.randomUUID()}`,
    checkpointDatabase: `cleanup-checkpoint-${crypto.randomUUID()}`,
    admissionDatabase: `cleanup-admission-${crypto.randomUUID()}`,
  }
  const first = await page.evaluate(async (options) => {
    const outputPath = '/src/output/origin-private/session.ts'
    const outcomePath = '/src/transfer/outcome.ts'
    const output = await import(outputPath) as typeof import(
      '../../src/output/origin-private/session'
    )
    const outcome = await import(outcomePath) as typeof import('../../src/transfer/outcome')
    const storage = navigator.storage as StorageManager & {
      getDirectory(): Promise<FileSystemDirectoryHandle>
    }
    const actualRoot = await storage.getDirectory()
    const rejectCleanup = async () => {
      throw new DOMException('injected cleanup failure', 'UnknownError')
    }
    const root = new Proxy(actualRoot, {
      get(target, property) {
        if (property === 'getDirectoryHandle') {
          return async (name: string, createOptions?: FileSystemGetDirectoryOptions) => {
            const directory = await target.getDirectoryHandle(name, createOptions)
            if (name !== '.windshare-receive-staging') return directory
            return new Proxy(directory, {
              get(stagingTarget, stagingProperty) {
                if (stagingProperty === 'removeEntry') {
                  return rejectCleanup
                }
                const value = Reflect.get(stagingTarget, stagingProperty, stagingTarget) as unknown
                return typeof value === 'function' ? value.bind(stagingTarget) : value
              },
            })
          }
        }
        const value = Reflect.get(target, property, target) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    })
    const session = await output.openOriginPrivateOutputSession({
      outputSessionId: options.outputSessionId,
      databaseName: options.checkpointDatabase,
      storage: {
        getDirectory: async () => root,
        estimate: () => navigator.storage.estimate(),
      },
      quota: {
        estimate: () => navigator.storage.estimate(),
        admissionDatabaseName: options.admissionDatabase,
        now: () => 0,
        leaseMilliseconds: 1_000,
        heartbeatMilliseconds: 500,
      },
      exporter: { export: async () => output.ORIGIN_PRIVATE_EXPORT_COMPLETE },
    })
    ;(globalThis as Record<string, unknown>).__failedCleanupSession = session
    const file = {
      source: {
        shareInstance: 'cleanup-share',
        fileId: 'cleanup-file',
        fileRevision: 'cleanup-revision',
      },
      path: ['cleanup.bin'],
      exactSize: 1n,
    }
    const begun = await session.beginFile(file)
    await begun.transaction.writeRange(0n, Uint8Array.of(1))
    await begun.transaction.commit()
    await session.finishJob(
      outcome.jobOutcome('Succeeded', outcome.EMPTY_TRANSFER_FAILURE_SUMMARY),
      new AbortController().signal,
    )
    return {
      committed: session.finalization?.committed,
      cleanupPending: session.finalization?.cleanupPending,
    }
  }, ids)
  expect(first).toEqual({ committed: true, cleanupPending: true })

  await page.reload()
  const recovered = await page.evaluate(async (options) => {
    const outputPath = '/src/output/origin-private/session.ts'
    const output = await import(outputPath) as typeof import(
      '../../src/output/origin-private/session'
    )
    const session = await output.openOriginPrivateOutputSession({
      outputSessionId: options.outputSessionId,
      databaseName: options.checkpointDatabase,
      quota: {
        estimate: () => navigator.storage.estimate(),
        admissionDatabaseName: options.admissionDatabase,
        now: () => 2_000,
        leaseMilliseconds: 1_000,
        heartbeatMilliseconds: 500,
      },
      exporter: { export: async () => output.ORIGIN_PRIVATE_EXPORT_COMPLETE },
    })
    const begun = await session.beginFile({
      source: {
        shareInstance: 'cleanup-share',
        fileId: 'cleanup-file',
        fileRevision: 'cleanup-revision',
      },
      path: ['cleanup.bin'],
      exactSize: 1n,
    })
    const recoveredRangeCount = begun.durableRanges.ranges.length
    await begun.transaction.abort(new DOMException('cleanup probe complete', 'AbortError'))
    await session.abortJob(new DOMException('cleanup retry', 'AbortError'))
    return { recoveredRangeCount }
  }, ids)
  expect(recovered).toEqual({ recoveredRangeCount: 0 })
})

async function createCheckpoint(page: Page, outputSessionId: string): Promise<readonly string[]> {
  return page.evaluate(async ({ path, sessionId }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    return harness.createCheckpoint(sessionId)
  }, { path: HARNESS_PATH, sessionId: outputSessionId })
}

async function reopenCheckpoint(
  page: Page,
  outputSessionId: string,
): Promise<Awaited<ReturnType<DurableRecoveryHarness['reopenCheckpoint']>>> {
  return page.evaluate(async ({ path, sessionId }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    return harness.reopenCheckpoint(sessionId)
  }, { path: HARNESS_PATH, sessionId: outputSessionId })
}

async function callHarness<T>(
  page: Page,
  outputSessionId: string,
  operation: keyof DurableRecoveryHarness,
): Promise<T> {
  return page.evaluate(async ({ path, sessionId, method }) => {
    const harness = (await import(path)) as DurableRecoveryHarness
    const call = harness[method] as (id: string) => Promise<unknown>
    return call(sessionId) as Promise<T>
  }, { path: HARNESS_PATH, sessionId: outputSessionId, method: operation })
}
