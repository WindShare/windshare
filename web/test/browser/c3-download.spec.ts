import { expect, test } from '@playwright/test'

type C3Module = typeof import('../../src/download/index')
type ContractsModule = typeof import('../../src/contracts/index')

test('uses real Chromium filesystem capabilities for random writes and abort cleanup', async ({
  page,
}) => {
  await page.goto('/')

  const result = await page.evaluate(async () => {
    const c3Path = '/src/download/index.ts'
    const contractsPath = '/src/contracts/index.ts'
    const c3 = (await import(c3Path)) as C3Module
    const contracts = (await import(contractsPath)) as ContractsModule
    const storage = navigator.storage as StorageManager & {
      getDirectory(): Promise<FileSystemDirectoryHandle>
    }
    const originRoot = await storage.getDirectory()
    const sandboxName = `c3-${crypto.randomUUID()}`
    const root = await originRoot.getDirectoryHandle(sandboxName, { create: true })

    const context = {
      plan: {
        planId: new Uint8Array(32),
        selectedEntries: [
          { kind: 'directory', path: 'tree', mtime: 0 },
          { kind: 'file', path: 'tree/file.bin', size: 4, mtime: 0 },
          { kind: 'file', path: 'tree/empty.bin', size: 0, mtime: 0 },
        ],
        selectedBytes: 4,
        chunks: contracts.createChunkSet([{ first: 0, end: 2 }]),
      },
      layout: {
        chunkRanges(index: number) {
          return index === 0
            ? [
                { path: 'tree/skipped.bin', offset: 0, length: 2 },
                { path: 'tree/file.bin', offset: 0, length: 2 },
              ]
            : [{ path: 'tree/file.bin', offset: 2, length: 2 }]
        },
      },
      validatePath(path: string) {
        if (path.startsWith('/') || path.split('/').includes('..')) {
          throw new Error('invalid path')
        }
        return path
      },
    } as unknown as Parameters<C3Module['createFileSystemDownloadSink']>[0]

    try {
      const sink = c3.createFileSystemDownloadSink(context, root)
      await sink.writeBlock(contracts.createChunkIndex(1), Uint8Array.of(3, 4))
      await sink.writeBlock(
        contracts.createChunkIndex(0),
        Uint8Array.of(90, 91, 1, 2),
      )
      await sink.finalize()

      const tree = await root.getDirectoryHandle('tree')
      const file = await (await tree.getFileHandle('file.bin')).getFile()
      const empty = await (await tree.getFileHandle('empty.bin')).getFile()
      let skippedExists = true
      try {
        await tree.getFileHandle('skipped.bin')
      } catch (error) {
        skippedExists = (error as DOMException).name !== 'NotFoundError'
      }

      const abortContext = {
        plan: {
          planId: new Uint8Array(32),
          selectedEntries: [{ kind: 'file', path: 'abort.bin', size: 1, mtime: 0 }],
          selectedBytes: 1,
          chunks: contracts.createChunkSet([{ first: 0, end: 1 }]),
        },
        layout: {
          chunkRanges: () => [{ path: 'abort.bin', offset: 0, length: 1 }],
        },
        validatePath: (path: string) => path,
      } as unknown as Parameters<C3Module['createFileSystemDownloadSink']>[0]
      const aborted = c3.createFileSystemDownloadSink(abortContext, root)
      await aborted.writeBlock(contracts.createChunkIndex(0), Uint8Array.of(9))
      await aborted.abort(new Error('cancelled'))
      let abortedExists = true
      try {
        await root.getFileHandle('abort.bin')
      } catch (error) {
        abortedExists = (error as DOMException).name !== 'NotFoundError'
      }

      const partialContext = {
        plan: {
          planId: new Uint8Array(32),
          selectedEntries: [{ kind: 'file', path: 'partial.bin', size: 2, mtime: 0 }],
          selectedBytes: 2,
          chunks: contracts.createChunkSet([{ first: 0, end: 1 }]),
        },
        layout: {
          chunkRanges: () => [{ path: 'partial.bin', offset: 0, length: 1 }],
        },
        validatePath: (path: string) => path,
      } as unknown as Parameters<C3Module['createFileSystemDownloadSink']>[0]
      const partial = c3.createFileSystemDownloadSink(partialContext, root)
      await partial.writeBlock(contracts.createChunkIndex(0), Uint8Array.of(5))
      let partialFinalizeCode: string | undefined
      try {
        await partial.finalize()
      } catch (error) {
        partialFinalizeCode = (error as { readonly code?: string }).code
      }
      let partialExists = true
      try {
        await root.getFileHandle('partial.bin')
      } catch (error) {
        partialExists = (error as DOMException).name !== 'NotFoundError'
      }

      const collisionHandle = await root.getFileHandle('collision.bin', { create: true })
      const collisionWriter = await collisionHandle.createWritable()
      await collisionWriter.write(Uint8Array.of(7))
      await collisionWriter.close()
      const collisionContext = {
        plan: {
          planId: new Uint8Array(32),
          selectedEntries: [{ kind: 'file', path: 'collision.bin', size: 1, mtime: 0 }],
          selectedBytes: 1,
          chunks: contracts.createChunkSet([{ first: 0, end: 1 }]),
        },
        layout: {
          chunkRanges: () => [{ path: 'collision.bin', offset: 0, length: 1 }],
        },
        validatePath: (path: string) => path,
      } as unknown as Parameters<C3Module['createFileSystemDownloadSink']>[0]
      const collision = c3.createFileSystemDownloadSink(collisionContext, root)
      let collisionCode: string | undefined
      try {
        await collision.writeBlock(contracts.createChunkIndex(0), Uint8Array.of(9))
      } catch (error) {
        collisionCode = (error as { readonly code?: string }).code
      }
      await collision.abort(new Error('collision cleanup'))
      const collisionBytes = [
        ...new Uint8Array(await (await collisionHandle.getFile()).arrayBuffer()),
      ]

      return {
        bytes: [...new Uint8Array(await file.arrayBuffer())],
        emptyBytes: empty.size,
        skippedExists,
        abortedExists,
        partialFinalizeCode,
        partialExists,
        collisionCode,
        collisionBytes,
        deliveryOrder: sink.deliveryOrder,
      }
    } finally {
      await originRoot.removeEntry(sandboxName, { recursive: true })
    }
  })

  expect(result).toEqual({
    bytes: [1, 2, 3, 4],
    emptyBytes: 0,
    skippedExists: false,
    abortedExists: false,
    partialFinalizeCode: 'output-finalize',
    partialExists: false,
    collisionCode: 'output-write',
    collisionBytes: [7],
    deliveryOrder: 'any',
  })
})

test('streams store-mode Zip64 through Chromium Web Streams', async ({ page }) => {
  await page.goto('/')

  const result = await page.evaluate(async () => {
    const c3Path = '/src/download/index.ts'
    const contractsPath = '/src/contracts/index.ts'
    const c3 = (await import(c3Path)) as C3Module
    const contracts = (await import(contractsPath)) as ContractsModule
    const chunks: Uint8Array[] = []
    let closed = false
    const output = new WritableStream<Uint8Array>({
      write: (chunk) => {
        chunks.push(chunk.slice())
      },
      close: () => {
        closed = true
      },
    })
    const context = {
      plan: {
        planId: new Uint8Array(32),
        selectedEntries: [
          { kind: 'directory', path: 'tree', mtime: 0 },
          { kind: 'file', path: 'tree/file.bin', size: 2, mtime: 0 },
        ],
        selectedBytes: 2,
        chunks: contracts.createChunkSet([{ first: 0, end: 2 }]),
      },
      layout: {
        chunkRanges: (index: number) => [
          { path: 'tree/file.bin', offset: index, length: 1 },
        ],
      },
      validatePath: (path: string) => path,
    } as unknown as Parameters<C3Module['createZipDownloadSink']>[0]
    const sink = c3.createZipDownloadSink(context, output)

    await sink.writeBlock(contracts.createChunkIndex(0), Uint8Array.of(1))
    const streamedBeforeFinalize = chunks.length > 0
    await sink.writeBlock(contracts.createChunkIndex(1), Uint8Array.of(2))
    await sink.finalize()

    const length = chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
    const archive = new Uint8Array(length)
    let offset = 0
    for (const chunk of chunks) {
      archive.set(chunk, offset)
      offset += chunk.byteLength
    }
    const zip64 = archive.some(
      (_, index) =>
        archive[index] === 0x50 &&
        archive[index + 1] === 0x4b &&
        archive[index + 2] === 0x06 &&
        archive[index + 3] === 0x06,
    )
    return {
      closed,
      deliveryOrder: sink.deliveryOrder,
      streamedBeforeFinalize,
      zip64,
    }
  })

  expect(result).toEqual({
    closed: true,
    deliveryOrder: 'ascending',
    streamedBeforeFinalize: true,
    zip64: true,
  })
})
