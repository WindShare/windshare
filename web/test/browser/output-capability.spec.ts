import { expect, test } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'

interface BrowserOutputWindow extends Window {
  showDirectoryPicker?: () => Promise<FileSystemDirectoryHandle>
  showSaveFilePicker?: (
    options?: { readonly suggestedName?: string },
  ) => Promise<FileSystemFileHandle>
}

const SINGLE_FILE_NAME = 'portable-matrix.bin'
const ZIP_FILE_NAME = 'portable-matrix.zip'
const SINGLE_BYTES = Uint8Array.of(0, 1, 2, 127, 128, 254, 255)
const ZIP_MEMBER_BYTES = Uint8Array.of(9, 8, 7, 6, 5)
const FULL_PORTABLE_STRESS_BYTES = 64 * 1024 * 1024
const CROSS_ENGINE_PORTABLE_STRESS_BYTES = 4 * 1024 * 1024

test('reports all four output capabilities exactly as the active engine exposes them', async ({
  page,
}) => {
  await page.goto('/')
  const capabilities = await page.evaluate(async () => {
    const modulePath = '/src/ui/v2-output.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const runtime = window as BrowserOutputWindow
    return {
      reported: output.browserV2OutputCapabilities(
        runtime as unknown as import('../../src/ui/v2-output').V2BrowserOutputWindow,
      ),
      nativeDirectory: typeof runtime.showDirectoryPicker === 'function',
      nativeSave: typeof runtime.showSaveFilePicker === 'function',
      portableDownload: typeof Blob === 'function' &&
        typeof WritableStream === 'function' &&
        typeof URL.createObjectURL === 'function' &&
        typeof URL.revokeObjectURL === 'function' &&
        document.documentElement !== null,
      originPrivateStaging: typeof (
        navigator.storage as (StorageManager & { getDirectory?: unknown }) | undefined
      )?.getDirectory === 'function',
    }
  })

  expect(capabilities.reported).toEqual({
    nativeDirectory: capabilities.nativeDirectory,
    nativeSave: capabilities.nativeSave,
    portableDownload: capabilities.portableDownload,
    originPrivateStaging: capabilities.originPrivateStaging,
  })
  expect(capabilities.reported.portableDownload).toBe(true)
})

test('runs the native picker adapter synchronously inside a real browser click handler', async ({
  page,
}) => {
  await page.goto('/')
  await page.evaluate(async () => {
    const modulePath = '/src/ui/v2-output.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const events: string[] = []
    Object.assign(window, { __windsharePickerEvents: events })

    const button = document.createElement('button')
    button.textContent = 'Acquire output'
    button.addEventListener('click', () => {
      events.push('handler')
      const runtime = {
        navigator: window.navigator,
        showSaveFilePicker: async () => {
          events.push('picker')
          return {
            createWritable: async () => new WritableStream<Uint8Array>(),
          } as FileSystemFileHandle
        },
      } as unknown as import('../../src/ui/v2-output').V2BrowserOutputWindow
      const acquired = output.acquireBrowserV2Output(
        'download',
        { kind: 'KnownSingleFile', suggestedName: 'matrix.bin', exactBytes: 1n },
        runtime,
      )
      events.push('returned')
      acquired.then(
        () => events.push('resolved'),
        () => events.push('rejected'),
      )
    })
    document.body.append(button)
  })

  const button = page.getByRole('button', { name: 'Acquire output' })
  await expect(button).toHaveCount(1)
  await button.click()
  await expect.poll(
    () => page.evaluate(() => (
      window as Window & { __windsharePickerEvents?: readonly string[] }
    ).__windsharePickerEvents),
  ).toEqual(['handler', 'picker', 'returned', 'resolved'])
})

test('downloads exact single-file bytes without a StorageManager through the production portable backend', async ({ page }) => {
  await page.goto('/')
  const downloadPromise = page.waitForEvent('download')
  await page.evaluate(async ({ bytes, name }) => {
    const modulePath = '/src/ui/v2-output.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const portableWindow = portableWindowWithoutNativeSave()
    const acquired = await output.acquireBrowserV2Output(
      'download',
      { kind: 'KnownSingleFile', suggestedName: name, exactBytes: BigInt(bytes.length) },
      portableWindow,
    )
    if (acquired.kind !== 'SingleFileStream') {
      throw new Error(`Expected portable single-file stream, received ${acquired.kind}`)
    }
    const session = await output.openBrowserV2OutputSession(acquired, 'portable-single')
    const begun = await session.beginFile({
      source: { shareInstance: 'share', fileId: 'single', fileRevision: 'revision' },
      path: [name],
      exactSize: BigInt(bytes.length),
    })
    await begun.transaction.writeRange(0n, Uint8Array.from(bytes))
    await begun.transaction.commit()
    await session.finishJob({
      status: 'Succeeded',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
    }, new AbortController().signal)

    function portableWindowWithoutNativeSave():
    import('../../src/ui/v2-output').V2BrowserOutputWindow {
      return {
        Blob: window.Blob,
        WritableStream: window.WritableStream,
        URL: window.URL,
        document: window.document,
        navigator: {} as Navigator,
        setTimeout: window.setTimeout.bind(window),
      }
    }
  }, { bytes: [...SINGLE_BYTES], name: SINGLE_FILE_NAME })

  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe(SINGLE_FILE_NAME)
  expect(await readDownload(download)).toEqual(Buffer.from(SINGLE_BYTES))
})

test('downloads a valid exact-content ZIP through the production portable backend', async ({ page }) => {
  await page.goto('/')
  const downloadPromise = page.waitForEvent('download')
  await page.evaluate(async ({ bytes, name }) => {
    const modulePath = '/src/ui/v2-output.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const acquired = await output.acquireBrowserV2Output(
      'download',
      {
        kind: 'Progressive',
        terminalBytes: BigInt(bytes.length),
        suggestedArchiveName: name,
      },
      {
        Blob: window.Blob,
        WritableStream: window.WritableStream,
        URL: window.URL,
        document: window.document,
        navigator: window.navigator,
        setTimeout: window.setTimeout.bind(window),
      },
    )
    if (acquired.kind !== 'ZipStream') {
      throw new Error(`Expected portable ZIP stream, received ${acquired.kind}`)
    }
    const session = await output.openBrowserV2OutputSession(acquired, 'portable-zip')
    const directory = { path: ['tree'] }
    await session.ensureDirectory(directory)
    const begun = await session.beginFile({
      source: { shareInstance: 'share', fileId: 'zip-member', fileRevision: 'revision' },
      path: ['tree', 'payload.bin'],
      exactSize: BigInt(bytes.length),
    })
    await begun.transaction.writeRange(0n, Uint8Array.from(bytes))
    await begun.transaction.commit()
    const signal = new AbortController().signal
    await session.finalizeDirectory(directory, signal)
    await session.finishJob({
      status: 'Succeeded',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
    }, signal)
  }, { bytes: [...ZIP_MEMBER_BYTES], name: ZIP_FILE_NAME })

  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe(ZIP_FILE_NAME)
  const archiveBytes = await readDownload(download)
  const reader = new ZipReader(new Uint8ArrayReader(archiveBytes))
  try {
    const entries = await reader.getEntries()
    expect(entries.map((entry) => entry.filename).sort()).toEqual([
      'tree/',
      'tree/payload.bin',
    ])
    const member = entries.find((entry) => entry.filename === 'tree/payload.bin')
    if (member === undefined || member.directory) {
      throw new Error('Portable ZIP member is missing')
    }
    expect(await member.getData(new Uint8ArrayWriter())).toEqual(ZIP_MEMBER_BYTES)
  } finally {
    await reader.close()
  }
})

test('rejects a declared portable output above the production memory bound', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const outputPath = '/src/ui/v2-output.ts'
    const portablePath = '/src/output/portable/browser-download.ts'
    const output = await import(outputPath) as typeof import('../../src/ui/v2-output')
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    try {
      await output.acquireBrowserV2Output(
        'download',
        {
          kind: 'KnownSingleFile',
          suggestedName: 'too-large.bin',
          exactBytes: BigInt(portable.PORTABLE_DOWNLOAD_MAXIMUM_BYTES) + 1n,
        },
        {
          Blob: window.Blob,
          WritableStream: window.WritableStream,
          URL: window.URL,
          document: window.document,
          navigator: window.navigator,
          setTimeout: window.setTimeout.bind(window),
        },
      )
      return { name: '', message: '' }
    } catch (error) {
      return error instanceof DOMException
        ? { name: error.name, message: error.message }
        : { name: 'UnexpectedError', message: String(error) }
    }
  })

  expect(result).toEqual({
    name: 'NotSupportedError',
    message: 'Portable browser downloads are limited to 64 MiB',
  })
})

test('streams one million ZIP members through the production writer and durable spool', async ({
  browserName,
  page,
}) => {
  // Other engines run the same production quota/fencing paths below; this single
  // structural stress avoids tripling a deliberately million-entry browser gate.
  test.skip(browserName !== 'chromium', 'The million-member structural stress runs once in Chromium')
  test.setTimeout(120_000)
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const probePath = '/test/browser/r8-bounded-output-probe.ts'
    const probe = await import(probePath) as typeof import('./r8-bounded-output-probe')
    return probe.probeMillionMemberZipWriter()
  })

  expect(result).toMatchObject({
    memberCount: 1_000_000,
    closed: true,
    afterClose: [0, 0],
  })
  expect(result.beforeClose[0]).toBeGreaterThan(1_000)
  expect(result.beforeClose[1]).toBe(1)
  expect(result.outputBytes).toBeGreaterThan(0)
  expect(result.outputWrites).toBeGreaterThan(result.memberCount * 2)
  expect(result.maximumWriteBytes).toBeLessThanOrEqual(256 * 1024)
})

test('bounds production ZIP assembly and rejects at the exact portable byte', async ({
  browserName,
  page,
}) => {
  test.setTimeout(120_000)
  await page.goto('/')
  const maximumBytes = browserName === 'chromium'
    ? FULL_PORTABLE_STRESS_BYTES
    : CROSS_ENGINE_PORTABLE_STRESS_BYTES
  const result = await page.evaluate(async (byteLimit) => {
    const portablePath = '/src/output/portable/browser-download.ts'
    const zipPath = '/src/output/streams/streaming-zip.ts'
    const spoolPath = '/src/output/streams/zip-spool.ts'
    const portable = await import(portablePath) as typeof import(
      '../../src/output/portable/browser-download'
    )
    const zip = await import(zipPath) as typeof import(
      '../../src/output/streams/streaming-zip'
    )
    const spoolModule = await import(spoolPath) as typeof import(
      '../../src/output/streams/zip-spool'
    )
    const databaseName = `r8-million-zip-${crypto.randomUUID()}`
    let maximumParts = 0
    let rejectionBufferedBytes = -1
    let rejectedWriteBytes = -1
    let published = false
    const output = portable.createBoundedPortableDownloadStream('million.zip', {
      createBlob: (parts) => new Blob([...parts]),
      publish: () => { published = true },
      observeAssembly: (snapshot) => {
        maximumParts = Math.max(maximumParts, snapshot.retainedParts)
        if (snapshot.rejectedWriteBytes > 0) {
          rejectionBufferedBytes = snapshot.bufferedBytes
          rejectedWriteBytes = snapshot.rejectedWriteBytes
        }
      },
    }, byteLimit)
    const archive = new zip.StreamingZipArchiveWriter(
      output,
      new spoolModule.IndexedDbZipCentralDirectorySpool({ databaseName }),
    )
    let committed = 0
    let failureName = ''
    try {
      for (let index = 0; index < 1_000_000; index += 1) {
        const member = await archive.beginFile({ path: [`f${index.toString(36)}`], exactSize: 0n })
        await member.close()
        committed += 1
      }
      await archive.close(new AbortController().signal)
    } catch (error) {
      failureName = error instanceof DOMException ? error.name : String(error)
      await archive.abort(error).catch(() => undefined)
    }

    const encoder = new TextEncoder()
    let expectedCommitted = 0
    let expectedBufferedBytes = 0
    let expectedRejectedWriteBytes = 0
    for (let index = 0; index < 1_000_000; index += 1) {
      const nameBytes = encoder.encode(`f${index.toString(36)}`).byteLength
      const localHeaderBytes = 50 + nameBytes
      if (localHeaderBytes > byteLimit - expectedBufferedBytes) {
        expectedRejectedWriteBytes = localHeaderBytes
        break
      }
      expectedBufferedBytes += localHeaderBytes
      const descriptorBytes = 24
      if (descriptorBytes > byteLimit - expectedBufferedBytes) {
        expectedRejectedWriteBytes = descriptorBytes
        break
      }
      expectedBufferedBytes += descriptorBytes
      expectedCommitted += 1
    }

    const database = await openDatabase(databaseName)
    const transaction = database.transaction(
      ['central-directory-chunks', 'central-directory-namespaces'],
      'readonly',
    )
    const chunkCount = await requestCount(transaction.objectStore('central-directory-chunks'))
    const namespaceCount = await requestCount(
      transaction.objectStore('central-directory-namespaces'),
    )
    await transactionDone(transaction)
    database.close()
    await deleteDatabase(databaseName)
    return {
      committed,
      expectedCommitted,
      failureName,
      maximumParts,
      maximumAllowedParts: Math.ceil(byteLimit / portable.PORTABLE_DOWNLOAD_PART_BYTES),
      rejectionBufferedBytes,
      expectedBufferedBytes,
      rejectedWriteBytes,
      expectedRejectedWriteBytes,
      published,
      chunkCount,
      namespaceCount,
      byteLimit,
    }

    function openDatabase(name: string): Promise<IDBDatabase> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.open(name)
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }

    function requestCount(store: IDBObjectStore): Promise<number> {
      return new Promise((resolve, reject) => {
        const request = store.count()
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }

    function transactionDone(transaction: IDBTransaction): Promise<void> {
      return new Promise((resolve, reject) => {
        transaction.addEventListener('complete', () => resolve(), { once: true })
        transaction.addEventListener('error', () => reject(transaction.error), { once: true })
        transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
      })
    }

    function deleteDatabase(name: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(name)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }
  }, maximumBytes)

  expect(result).toMatchObject({
    failureName: 'QuotaExceededError',
    published: false,
    chunkCount: 0,
    namespaceCount: 0,
  })
  expect(result.committed).toBe(result.expectedCommitted)
  expect(result.rejectionBufferedBytes).toBe(result.expectedBufferedBytes)
  expect(result.rejectedWriteBytes).toBe(result.expectedRejectedWriteBytes)
  expect(result.maximumParts).toBeLessThanOrEqual(result.maximumAllowedParts)
  expect(result.byteLimit).toBe(maximumBytes)
  expect(result.committed).toBeLessThan(1_000_000)
})

test('serializes OPFS quota across independent realms and reclaims an expired crash lease', async ({ context, page }) => {
  const secondPage = await context.newPage()
  await Promise.all([page.goto('/'), secondPage.goto('/')])
  const databaseName = `r8-admission-${crypto.randomUUID()}`
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
      ;(globalThis as Record<string, unknown>).__r8Admission =
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
      const admission = (globalThis as Record<string, unknown>).__r8Admission as
        import('../../src/output/origin-private/admission').OriginPrivateStagingAdmission
      await admission.reserve([path], BigInt(size), { logicalBytes: 0n, coveredBytes: 0n })
    }, { path: file, size: bytes })
  }

  async function release(target: typeof page): Promise<void> {
    await target.evaluate(async () => {
      const admission = (globalThis as Record<string, unknown>).__r8Admission as
        import('../../src/output/origin-private/admission').OriginPrivateStagingAdmission
      await admission.release()
    })
  }
})

test('sweeps expired ZIP spool namespaces after reload without deleting a live writer', async ({ context, page }) => {
  const databaseName = `r8-spool-recovery-${crypto.randomUUID()}`
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
    const probePath = '/test/browser/r8-idb-failclosed-probe.ts'
    const probe = await import(probePath) as typeof import('./r8-idb-failclosed-probe')
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
    const databaseName = `r8-journal-pages-${crypto.randomUUID()}`
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

test('native picker permission prompt has an explicit headed-machine evidence boundary', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  const available = await page.evaluate(
    () => typeof (window as BrowserOutputWindow).showSaveFilePicker === 'function',
  )
  expect(typeof available).toBe('boolean')
  // Unsupported engines have no native permission prompt to exercise.
  test.skip(!available, `${browserName} does not expose showSaveFilePicker`)
  // Native destination selection is an explicitly separate headed manual gate.
  test.skip(
    true,
    'Headless Playwright cannot safely choose an external native destination; run the headed picker probe manually',
  )
})

async function readDownload(
  download: import('@playwright/test').Download,
): Promise<Buffer> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks)
}
