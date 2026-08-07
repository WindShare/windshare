import { expect, test } from '@playwright/test'

test('ZIP spool budgets reject atomically without committing the rejected record', async ({ page }) => {
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const spoolPath = '/src/output/streams/zip-spool.ts'
    const outputContractPath = '/src/transfer/output-session.ts'
    const [spoolModule, outputContract] = await Promise.all([
      import(spoolPath) as Promise<typeof import('../../src/output/streams/zip-spool')>,
      import(outputContractPath) as Promise<typeof import('../../src/transfer/output-session')>,
    ])
    const entryDatabase = `zip-entry-budget-${crypto.randomUUID()}`
    const byteDatabase = `zip-byte-budget-${crypto.randomUUID()}`
    const entrySpool = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: entryDatabase,
      namespace: 'entry-budget',
      token: 'entry-budget-token',
      maxEntries: 1,
      maxBytes: 8,
    })
    const byteSpool = new spoolModule.IndexedDbZipCentralDirectorySpool({
      databaseName: byteDatabase,
      namespace: 'byte-budget',
      token: 'byte-budget-token',
      maxEntries: 4,
      maxBytes: 3,
    })
    try {
      await entrySpool.append(Uint8Array.of(0x11, 0x12))
      const entryFailure = await budgetFailure(entrySpool.append(Uint8Array.of(0x21)))
      const entryManifest = await entrySpool.seal()
      const entryChunk = await entrySpool.readChunk(0)

      await byteSpool.append(Uint8Array.of(0x31, 0x32))
      const byteFailure = await budgetFailure(byteSpool.append(Uint8Array.of(0x41, 0x42)))
      await byteSpool.append(Uint8Array.of(0x51))
      const byteManifest = await byteSpool.seal()
      const byteChunk = await byteSpool.readChunk(0)

      return {
        entryFailure,
        entryManifest: {
          chunkCount: entryManifest.chunkCount,
          recordCount: entryManifest.recordCount.toString(),
          byteLength: entryManifest.byteLength.toString(),
        },
        entryChunk: [...(entryChunk ?? [])],
        byteFailure,
        byteManifest: {
          chunkCount: byteManifest.chunkCount,
          recordCount: byteManifest.recordCount.toString(),
          byteLength: byteManifest.byteLength.toString(),
        },
        byteChunk: [...(byteChunk ?? [])],
      }
    } finally {
      await Promise.all([entrySpool.clear(), byteSpool.clear()])
      await Promise.all([deleteDatabase(entryDatabase), deleteDatabase(byteDatabase)])
    }

    async function budgetFailure(operation: Promise<void>): Promise<{
      readonly name: string
      readonly budget: string
      readonly limit: string
      readonly attempted: string
    }> {
      try {
        await operation
      } catch (error) {
        if (error instanceof outputContract.OutputBudgetExceededError) {
          return {
            name: error.name,
            budget: error.budget,
            limit: error.limit.toString(),
            attempted: error.attempted.toString(),
          }
        }
        throw error
      }
      throw new Error('ZIP spool operation budget unexpectedly accepted a record')
    }

    function deleteDatabase(databaseName: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(databaseName)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }
  })

  expect(result).toEqual({
    entryFailure: {
      name: 'OutputBudgetExceededError',
      budget: 'zip-central-directory-entries',
      limit: '1',
      attempted: '2',
    },
    entryManifest: { chunkCount: 1, recordCount: '1', byteLength: '2' },
    entryChunk: [0x11, 0x12],
    byteFailure: {
      name: 'OutputBudgetExceededError',
      budget: 'zip-central-directory-bytes',
      limit: '3',
      attempted: '4',
    },
    byteManifest: { chunkCount: 1, recordCount: '2', byteLength: '3' },
    byteChunk: [0x31, 0x32, 0x51],
  })
})
