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

test('downloads exact single-file bytes without a StorageManager through the production portable backend', async ({ page }) => {
  await page.goto('/')
  const downloadPromise = page.waitForEvent('download')
  await page.evaluate(async ({ bytes, name }) => {
    const modulePath = '/src/ui/v2-output.ts'
    const admissionPath = '/test/output/admission-fixture.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const admission = await import(admissionPath) as typeof import('../output/admission-fixture')
    const portableWindow = portableWindowWithoutNativeSave()
    const acquired = await output.acquireBrowserV2Output(
      'download',
      { kind: 'KnownSingleFile', suggestedName: name, exactBytes: BigInt(bytes.length) },
      portableWindow,
    )
    if (acquired.kind !== 'SingleFileStream') {
      throw new Error(`Expected portable single-file stream, received ${acquired.kind}`)
    }
    const session = await output.openBrowserV2OutputSession(
      acquired,
      'portable-single',
      admission.TEST_DIRECTORY_ADMISSION_SCOPE,
    )
    const signal = new AbortController().signal
    const begun = await session.beginFile(await admission.admittedOutputFile(session, {
      source: {
        shareInstance: admission.testOutputIdentity('portable-share'),
        fileId: admission.testOutputIdentity('portable-single'),
        fileRevision: admission.testOutputIdentity('portable-single-revision'),
      },
      path: [name],
      exactSize: BigInt(bytes.length),
    }), signal)
    await begun.transaction.writeRange(0n, Uint8Array.from(bytes), signal)
    await begun.transaction.commit(signal)
    await session.completeJob({
      status: 'Succeeded',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
    }, signal)

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
    const admissionPath = '/test/output/admission-fixture.ts'
    const output = await import(modulePath) as typeof import('../../src/ui/v2-output')
    const admission = await import(admissionPath) as typeof import('../output/admission-fixture')
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
    const session = await output.openBrowserV2OutputSession(
      acquired,
      'portable-zip',
      admission.TEST_DIRECTORY_ADMISSION_SCOPE,
    )
    const directory = await admission.admittedOutputDirectory(session, { path: ['tree'] })
    const signal = new AbortController().signal
    const begun = await session.beginFile(await admission.admittedOutputFile(session, {
      source: {
        shareInstance: admission.testOutputIdentity('portable-share'),
        fileId: admission.testOutputIdentity('portable-zip-member'),
        fileRevision: admission.testOutputIdentity('portable-zip-revision'),
      },
      path: ['tree', 'payload.bin'],
      exactSize: BigInt(bytes.length),
    }), signal)
    await begun.transaction.writeRange(0n, Uint8Array.from(bytes), signal)
    await begun.transaction.commit(signal)
    await session.finalizeDirectory(directory, signal)
    await session.completeJob({
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

async function readDownload(
  download: import('@playwright/test').Download,
): Promise<Buffer> {
  const stream = await download.createReadStream()
  if (stream === null) throw new Error('Playwright download stream is unavailable')
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  return Buffer.concat(chunks)
}
