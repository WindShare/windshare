import { expect, test } from '@playwright/test'

const FULL_PORTABLE_STRESS_BYTES = 64 * 1024 * 1024
const CROSS_ENGINE_PORTABLE_STRESS_BYTES = 4 * 1024 * 1024
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
    const probePath = '/test/browser/portable-output-periodic-probe.ts'
    const probe = await import(probePath) as typeof import('./portable-output-periodic-probe')
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
    const databaseName = `million-zip-${crypto.randomUUID()}`
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
