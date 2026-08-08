import { expect, test, type Page } from '@playwright/test'

import {
  requireOriginPrivateStorage,
} from './browser-storage-support'

interface PausedTaskBrowserFixture {
  readonly databaseName: string
  readonly backend: 'file-system-access' | 'origin-private-staging'
  readonly rootName?: string
  readonly initialTransferJobId: string
  readonly initialOutputSessionId: string
}

interface PausedTaskDiscardHarnessResult {
  readonly kind: string
  readonly preservedCompletedFiles: number
  readonly exportedPartialZip: boolean
  readonly descriptorCount: number
  readonly completedBytes: readonly number[]
  readonly incompleteRemoved: boolean
  readonly zipEntries: readonly string[]
  readonly zipMagic: readonly number[]
  readonly stagingRemoved: boolean
  readonly permissionStartedSynchronously: boolean
  readonly partialOutputStartedSynchronously: boolean
}

interface PausedTaskResumeHarness {
  createPausedTaskBrowserFixture(
    backend: PausedTaskBrowserFixture['backend'],
  ): Promise<PausedTaskBrowserFixture>
  createDiscardTaskBrowserFixture(
    backend: PausedTaskBrowserFixture['backend'],
  ): Promise<PausedTaskBrowserFixture>
  resumePausedTaskBrowserFixture(fixture: PausedTaskBrowserFixture): Promise<{
    readonly descriptorCount: number
    readonly ranges: readonly string[]
    readonly freshTransferJobId: boolean
    readonly freshOutputSessionId: boolean
    readonly permissionStartedSynchronously: boolean
    readonly finalOutputStartedSynchronously: boolean
  }>
  discardPausedTaskBrowserFixture(
    fixture: PausedTaskBrowserFixture,
  ): Promise<PausedTaskDiscardHarnessResult>
  interruptFsaDiscardAfterOwnedFileRemoval(fixture: PausedTaskBrowserFixture): Promise<void>
  probeOriginPrivateDiscardExportFailure(): Promise<{
    readonly firstKind: string
    readonly firstReason: string
    readonly retryKind: string
    readonly retryReason: string
    readonly outputCalls: number
    readonly descriptorCount: number
    readonly stagingRetained: boolean
  }>
  probePausedTaskPermissionAndShareAuthority(): Promise<{
    readonly deniedFailure: string
    readonly deniedRunCreations: number
    readonly mismatchName: string
    readonly mismatchPermissionCalls: number
  }>
  probePausedTaskStaleCapability(): Promise<string>
}

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
const PAUSED_TASK_HARNESS_PATH = '/test/browser/paused-task-resume-harness.ts'
const TEST_TRANSFER_INTENT_DIGEST = 'CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const TEST_ROOT_IDENTITY = 'DAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

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

test('FSA paused-task reconstruction survives reload with renewed permission and fresh run IDs', async ({ page }) => {
  const fixture = await createPausedTaskFixture(page, 'file-system-access')
  await page.reload()
  expect(await resumePausedTaskFixture(page, fixture)).toEqual({
    descriptorCount: 1,
    ranges: ['0:3'],
    freshTransferJobId: true,
    freshOutputSessionId: true,
    permissionStartedSynchronously: true,
    finalOutputStartedSynchronously: true,
  })
})

test('OPFS paused-task reconstruction reacquires the root and a fresh final output after reload', async ({ page }) => {
  const fixture = await createPausedTaskFixture(page, 'origin-private-staging')
  await page.reload()
  expect(await resumePausedTaskFixture(page, fixture)).toEqual({
    descriptorCount: 1,
    ranges: ['0:3'],
    freshTransferJobId: true,
    freshOutputSessionId: true,
    permissionStartedSynchronously: true,
    finalOutputStartedSynchronously: true,
  })
})

test('FSA cancel preserves completed files and discards only incomplete resume state after reload', async ({ page }) => {
  const fixture = await createDiscardTaskFixture(page, 'file-system-access')
  await page.reload()

  expect(await discardPausedTaskFixture(page, fixture)).toEqual({
    kind: 'Discarded',
    preservedCompletedFiles: 1,
    exportedPartialZip: false,
    descriptorCount: 0,
    completedBytes: [9, 8, 7, 6, 5],
    incompleteRemoved: true,
    zipEntries: [],
    zipMagic: [],
    stagingRemoved: false,
    permissionStartedSynchronously: true,
    partialOutputStartedSynchronously: true,
  })
})

test('FSA cancel replays a certified physical retirement after reload', async ({ page }) => {
  const fixture = await createDiscardTaskFixture(page, 'file-system-access')
  await page.reload()
  await page.evaluate(async ({ path, paused }) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    await harness.interruptFsaDiscardAfterOwnedFileRemoval(paused)
  }, { path: PAUSED_TASK_HARNESS_PATH, paused: fixture })
  await page.reload()

  expect(await discardPausedTaskFixture(page, fixture)).toMatchObject({
    kind: 'Discarded',
    preservedCompletedFiles: 1,
    descriptorCount: 0,
    completedBytes: [9, 8, 7, 6, 5],
    incompleteRemoved: true,
  })
})

test('OPFS cancel exports committed members as a partial ZIP before discarding staging', async ({ page }) => {
  const fixture = await createDiscardTaskFixture(page, 'origin-private-staging')
  await page.reload()

  expect(await discardPausedTaskFixture(page, fixture)).toEqual({
    kind: 'Discarded',
    preservedCompletedFiles: 1,
    exportedPartialZip: true,
    descriptorCount: 0,
    completedBytes: [9, 8, 7, 6, 5],
    incompleteRemoved: true,
    zipEntries: ['completed-browser-file.bin'],
    zipMagic: [0x50, 0x4b],
    stagingRemoved: true,
    permissionStartedSynchronously: true,
    partialOutputStartedSynchronously: true,
  })
})

test('OPFS cancel retains staging when partial ZIP publication is ambiguous', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.probeOriginPrivateDiscardExportFailure()
  }, PAUSED_TASK_HARNESS_PATH)
  expect(result).toEqual({
    firstKind: 'NeedsAttention',
    firstReason: 'export-failed',
    retryKind: 'NeedsAttention',
    retryReason: 'export-failed',
    outputCalls: 1,
    descriptorCount: 1,
    stagingRetained: true,
  })
})

test('paused-task resume gates permission and run creation on exact current share authority', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.probePausedTaskPermissionAndShareAuthority()
  }, PAUSED_TASK_HARNESS_PATH)
  expect(result).toEqual({
    deniedFailure: 'permission-denied',
    deniedRunCreations: 0,
    mismatchName: 'PausedTaskShareAuthorityError',
    mismatchPermissionCalls: 0,
  })
})

test('paused-task resume rejects a capability replaced after preparation', async ({ page }) => {
  const result = await page.evaluate(async (path) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.probePausedTaskStaleCapability()
  }, PAUSED_TASK_HARNESS_PATH)
  expect(result).toBe('stale')
})

test('one durable checkpoint namespace cannot publish competing heads from two pages or runs', async ({
  context,
  page,
}) => {
  await page.goto('/')
  const competitor = await context.newPage()
  await competitor.goto('/')
  const runPrefix = `lease-${crypto.randomUUID()}`
  const firstOutputSessionId = `${runPrefix}-first`
  const competingOutputSessionId = `${runPrefix}-second`
  await callHarness<void>(page, firstOutputSessionId, 'holdOutputSession')
  expect(await callHarness<string | undefined>(
    competitor,
    competingOutputSessionId,
    'competingSessionError',
  )).toBe('InvalidStateError')
  await callHarness<void>(page, firstOutputSessionId, 'releaseOutputSession')
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
  const result = await page.evaluate(async (binding) => {
    const repositoryPath = '/src/output/browser/indexeddb-repository.ts'
    const journalPath = '/src/output/persistence/journal.ts'
    const persistentPath = '/src/output/persistent-tree/session.ts'
    const fakesPath = '/test/output/fakes.ts'
    const admissionPath = '/test/output/admission-fixture.ts'
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
    const admission = await import(admissionPath) as typeof import('../output/admission-fixture')
    const databaseName = `journal-pages-${crypto.randomUUID()}`
    const backend = 'real-indexeddb-test'
    const outputSessionId = 'paged-session'
    const identity = { backend, outputSessionId, ...binding }
    const tree = new fakes.MemoryOutputTree()
    let repository = await repositoryModule.IndexedDbOutputRepository.open(
      databaseName,
      { backend, ...binding },
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
            fileId: indexedIdentity(0x20, 16, index),
            fileRevision: indexedIdentity(0x30, 16, index),
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
          fileId: indexedIdentity(0x40, 16, 1),
          fileRevision: indexedIdentity(0x50, 16, 1),
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

    const alternateRootIdentity = indexedIdentity(0x60, 32, 1)
    const alternateRepository = await repositoryModule.IndexedDbOutputRepository.open(
      databaseName,
      { backend, transferIntentDigest: binding.transferIntentDigest, rootIdentity: alternateRootIdentity },
    )
    const alternateNamespaceRecords = (await alternateRepository.scanCommitted({
      direction: 'ascending',
    })).records.length
    alternateRepository.close()

    repository = await repositoryModule.IndexedDbOutputRepository.open(
      databaseName,
      { backend, ...binding },
    )
    const reopenedIdentity = { ...identity, outputSessionId: 'paged-session-reopened' }
    const session = await persistent.PersistentTreeOutputSession.open({
      identity: reopenedIdentity,
      directoryAdmissionScope: {
        ...admission.TEST_DIRECTORY_ADMISSION_SCOPE,
        transferIntentDigest: binding.transferIntentDigest,
      },
      tree,
      journal: repository,
    })
    const firstReopened = await repository.scanCommitted({ direction: 'ascending' })
    const persistedRuntimeNeutral = firstReopened.records[0] !== undefined &&
      !Object.hasOwn(firstReopened.records[0], 'outputSessionId')
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
      alternateNamespaceRecords,
      persistedRuntimeNeutral,
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

    function indexedIdentity(first: number, length: number, value: number): string {
      const bytes = Uint8Array.from(
        { length },
        (_, index) => (first + index) & 0xff,
      )
      new DataView(bytes.buffer).setUint32(length - 4, value, false)
      let binary = ''
      for (const byte of bytes) binary += String.fromCharCode(byte)
      return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
    }
  }, {
    transferIntentDigest: TEST_TRANSFER_INTENT_DIGEST,
    rootIdentity: TEST_ROOT_IDENTITY,
  })

  expect(result).toMatchObject({
    scanned: result.recordCount,
    descendingScanned: result.recordCount,
    lazilyEnumerated: result.recordCount,
    maximumPage: 128,
    ascendingMonotonic: true,
    descendingMonotonic: true,
    crashCandidateRemoved: true,
    alternateNamespaceRecords: 0,
    persistedRuntimeNeutral: true,
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
    transferIntentDigest: TEST_TRANSFER_INTENT_DIGEST,
    rootIdentity: TEST_ROOT_IDENTITY,
  }
  const first = await page.evaluate(async (options) => {
    const outputPath = '/src/output/origin-private/session.ts'
    const outcomePath = '/src/transfer/outcome.ts'
    const admissionPath = '/test/output/admission-fixture.ts'
    const output = await import(outputPath) as typeof import(
      '../../src/output/origin-private/session'
    )
    const outcome = await import(outcomePath) as typeof import('../../src/transfer/outcome')
    const admission = await import(admissionPath) as typeof import('../output/admission-fixture')
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
      directoryAdmissionScope: {
        ...admission.TEST_DIRECTORY_ADMISSION_SCOPE,
        transferIntentDigest: options.transferIntentDigest,
      },
      transferIntentDigest: options.transferIntentDigest,
      rootIdentity: options.rootIdentity,
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
        shareInstance: admission.testOutputIdentity('cleanup-share'),
        fileId: admission.testOutputIdentity('cleanup-file'),
        fileRevision: admission.testOutputIdentity('cleanup-revision'),
      },
      path: ['cleanup.bin'],
      exactSize: 1n,
    }
    const signal = new AbortController().signal
    const begun = await session.beginFile(await admission.admittedOutputFile(session, file), signal)
    await begun.transaction.writeRange(0n, Uint8Array.of(1), signal)
    await begun.transaction.commit(signal)
    await session.completeJob(
      outcome.jobOutcome('Succeeded', outcome.EMPTY_TRANSFER_FAILURE_SUMMARY),
      signal,
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
    const faultPath = '/src/transfer/fault.ts'
    const admissionPath = '/test/output/admission-fixture.ts'
    const output = await import(outputPath) as typeof import(
      '../../src/output/origin-private/session'
    )
    const fault = await import(faultPath) as typeof import('../../src/transfer/fault')
    const admission = await import(admissionPath) as typeof import('../output/admission-fixture')
    const session = await output.openOriginPrivateOutputSession({
      outputSessionId: options.outputSessionId,
      directoryAdmissionScope: {
        ...admission.TEST_DIRECTORY_ADMISSION_SCOPE,
        transferIntentDigest: options.transferIntentDigest,
      },
      transferIntentDigest: options.transferIntentDigest,
      rootIdentity: options.rootIdentity,
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
    const begun = await session.beginFile(await admission.admittedOutputFile(session, {
      source: {
        shareInstance: admission.testOutputIdentity('cleanup-share'),
        fileId: admission.testOutputIdentity('cleanup-file'),
        fileRevision: admission.testOutputIdentity('cleanup-revision'),
      },
      path: ['cleanup.bin'],
      exactSize: 1n,
    }), new AbortController().signal)
    const recoveredRangeCount = begun.durableRanges.ranges.length
    const retirement = fault.authorizeFileRetirement(fault.sourceFault(
      fault.FaultScope.FileLocal,
      fault.SourceFaultCode.Permanent,
    ))
    if (retirement === undefined) throw new Error('cleanup probe retirement was not authorized')
    await begun.transaction.retire(retirement)
    await session.pauseJob(new DOMException('cleanup retry', 'AbortError'))
    return { recoveredRangeCount }
  }, ids)
  expect(recovered).toEqual({ recoveredRangeCount: 0 })
})

async function createPausedTaskFixture(
  page: Page,
  backend: PausedTaskBrowserFixture['backend'],
): Promise<PausedTaskBrowserFixture> {
  return page.evaluate(async ({ path, selectedBackend }) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.createPausedTaskBrowserFixture(selectedBackend)
  }, { path: PAUSED_TASK_HARNESS_PATH, selectedBackend: backend })
}

async function createDiscardTaskFixture(
  page: Page,
  backend: PausedTaskBrowserFixture['backend'],
): Promise<PausedTaskBrowserFixture> {
  return page.evaluate(async ({ path, selectedBackend }) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.createDiscardTaskBrowserFixture(selectedBackend)
  }, { path: PAUSED_TASK_HARNESS_PATH, selectedBackend: backend })
}

async function discardPausedTaskFixture(
  page: Page,
  fixture: PausedTaskBrowserFixture,
): Promise<PausedTaskDiscardHarnessResult> {
  return page.evaluate(async ({ path, paused }) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.discardPausedTaskBrowserFixture(paused)
  }, { path: PAUSED_TASK_HARNESS_PATH, paused: fixture })
}

async function resumePausedTaskFixture(
  page: Page,
  fixture: PausedTaskBrowserFixture,
): Promise<Awaited<ReturnType<PausedTaskResumeHarness['resumePausedTaskBrowserFixture']>>> {
  return page.evaluate(async ({ path, paused }) => {
    const harness = (await import(path)) as PausedTaskResumeHarness
    return harness.resumePausedTaskBrowserFixture(paused)
  }, { path: PAUSED_TASK_HARNESS_PATH, paused: fixture })
}

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
