import { expect, test, type Page } from '@playwright/test'
import { Uint8ArrayReader, Uint8ArrayWriter, ZipReader } from '@zip.js/zip.js'
import type { ConcurrentZipInput } from './concurrency-probe'

const FIRST_PAYLOAD = [1, 2, 3, 4, 5, 6]
const SECOND_PAYLOAD = [9, 8, 7, 6, 5, 4, 3]

for (const parentLayout of ['same-directory', 'same-leaf-distinct-directories'] as const) {
  test('production ZIP writers overlap across tabs: ' + parentLayout, async ({ page, context }) => {
    const secondPage = await context.newPage()
    const databaseName = 'direct-zip-concurrency-' + crypto.randomUUID()
    await Promise.all([page.goto('/'), secondPage.goto('/')])
    try {
      const first = await start(page, { databaseName, branch: 'A', payload: FIRST_PAYLOAD })
      const second = await start(secondPage, {
        databaseName, branch: 'B', payload: SECOND_PAYLOAD,
        ...(parentLayout === 'same-directory' ? { savedParentOperationId: first.operationId } : {}),
      })
      expect(first.parentName).toBe('Downloads')
      expect(second.parentName).toBe('Downloads')
      expect(first.operationId).not.toBe(second.operationId)
      expect(first.directSupport).toBe('runtime-supported')
      expect(second.directSupport).toBe('runtime-supported')
      // Both pages have acknowledged payload on separate native streams. A
      // serialized implementation cannot reach this barrier before finalization.
      const writers = await Promise.all([snapshot(page), snapshot(secondPage)])
      for (const writer of writers) {
        expect(writer.opens).toBe(2)
        expect(writer.closes).toBe(1)
        expect(writer.writtenBytes).toBeGreaterThan(FIRST_PAYLOAD.length)
      }
      expect(await operationLease(secondPage, databaseName, first.operationId)).toBe('busy')
      for (const source of ['persisted-handle', 'reacquired-handle'] as const) {
        expect(await targetLease(secondPage, databaseName, first.operationId, source)).toEqual({
          status: 'busy', scope: 'fsa-entry',
          message: 'This file is already being changed by another WindShare task',
        })
      }
      const archives = await Promise.all([finish(page), finish(secondPage)])
      expect(archives[0]!.stableName).not.toBe(archives[1]!.stableName)
      await Promise.all(archives.map((result, index) =>
        assertArchive(result, index === 0 ? FIRST_PAYLOAD : SECOND_PAYLOAD)))
      await detach(page)
      expect(await operationLease(secondPage, databaseName, first.operationId)).toBe('acquired')
      expect(await targetLease(secondPage, databaseName, first.operationId, 'persisted-handle'))
        .toEqual({ status: 'acquired' })
    } finally {
      await Promise.all([detach(page), detach(secondPage)])
      await cleanup(page, databaseName)
      await secondPage.close()
    }
  })
}

test('failed production ZIP activation releases ownership for another tab', async ({ page, context }) => {
  const secondPage = await context.newPage()
  const databaseName = 'direct-zip-activation-release-' + crypto.randomUUID()
  await Promise.all([page.goto('/'), secondPage.goto('/')])
  try {
    const failed = await page.evaluate(async name => {
      const path = '/test/browser/direct-zip/concurrency-probe.ts'
      return (await import(path) as typeof import('./concurrency-probe'))
        .failProductionConcurrentZipActivation(name)
    }, databaseName)
    expect(await operationLease(secondPage, databaseName, failed.operationId)).toBe('acquired')
    expect(await targetLease(secondPage, databaseName, failed.operationId, 'reacquired-handle'))
      .toEqual({ status: 'acquired' })
    await start(secondPage, { databaseName, branch: 'A',
      savedParentOperationId: failed.operationId, payload: SECOND_PAYLOAD })
    await assertArchive(await finish(secondPage), SECOND_PAYLOAD)
  } finally {
    await Promise.all([detach(page), detach(secondPage)])
    await cleanup(page, databaseName)
    await secondPage.close()
  }
})

test('persisted ZIP target identity admits deletion retry without blocking unrelated files', async ({ page, context }) => {
  const secondPage = await context.newPage()
  const databaseName = 'direct-zip-deleted-target-' + crypto.randomUUID()
  await Promise.all([page.goto('/'), secondPage.goto('/')])
  try {
    const first = await start(page, { databaseName, branch: 'A', payload: FIRST_PAYLOAD })
    await assertArchive(await finish(page), FIRST_PAYLOAD)
    await detach(page)
    const identities = await secondPage.evaluate(async input => {
      const path = '/test/browser/direct-zip/concurrency-probe.ts'
      return (await import(path) as typeof import('./concurrency-probe'))
        .probeDeletedZipTargetLease(input.databaseName, input.operationId)
    }, { databaseName, operationId: first.operationId })
    expect(identities).toEqual({
      persistedIdentityRetained: true, otherParentIdentityDistinct: true, siblingIdentityDistinct: true,
    })
  } finally {
    await Promise.all([detach(page), detach(secondPage)])
    await cleanup(page, databaseName)
    await secondPage.close()
  }
})

function start(page: Page, input: ConcurrentZipInput) {
  return page.evaluate(async value => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe')).startProductionConcurrentZip(value)
  }, input)
}

function snapshot(page: Page) {
  return page.evaluate(async () => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe')).productionConcurrentWriterSnapshot()
  })
}

function finish(page: Page) {
  return page.evaluate(async () => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe')).finishProductionConcurrentZip()
  })
}

function detach(page: Page) {
  return page.evaluate(async () => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe')).detachProductionConcurrentZip()
  })
}

function operationLease(page: Page, databaseName: string, operationId: string) {
  return page.evaluate(async value => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe'))
      .probeConcurrentOperationLease(value.databaseName, value.operationId)
  }, { databaseName, operationId })
}

function targetLease(page: Page, databaseName: string, operationId: string,
  source: 'persisted-handle' | 'reacquired-handle') {
  return page.evaluate(async value => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe'))
      .probeConcurrentTargetLease(value.databaseName, value.operationId, value.source)
  }, { databaseName, operationId, source })
}

function cleanup(page: Page, databaseName: string) {
  return page.evaluate(async name => {
    const path = '/test/browser/direct-zip/concurrency-probe.ts'
    return (await import(path) as typeof import('./concurrency-probe')).cleanupProductionConcurrentZip(name)
  }, databaseName)
}

async function assertArchive(result: Awaited<ReturnType<typeof finish>>, payload: number[]) {
  expect(result.lifecycle).toBe('published')
  const archive = new ZipReader(new Uint8ArrayReader(Uint8Array.from(result.archive)), {
    checkSignature: true, useWebWorkers: false,
  })
  try {
    const files = (await archive.getEntries()).filter(entry => !entry.directory)
    expect(files.map(entry => entry.filename)).toEqual(['shared/a.txt'])
    expect(await files[0]!.getData!(new Uint8ArrayWriter())).toEqual(Uint8Array.from(payload))
  } finally { await archive.close() }
}
