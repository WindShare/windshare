import { describe, expect, it, vi } from 'vitest'
import { ZipReader, Uint8ArrayReader, Uint8ArrayWriter } from '@zip.js/zip.js'
import { buildCompleteZipBlob, exportCompleteZipEntries } from '../../../src/output/progressive-zip/partial-export'
import { zipBytesCrc32 } from '../../../src/output/progressive-zip/crc-ranges'
import type { CompleteZipEntrySpan } from '../../../src/output/progressive-zip/archive'
import { readCompleteZipEntries } from '../../../src/output/resume/reopen/partial-zip-continuation'
import type { TaskCheckpointStore } from '../../../src/output/origin-private/task-checkpoint/store'
import {
  DEFAULT_PARTIAL_EXPORT_NAME,
  partialExportSuggestedName,
  preparePartialExport,
  supportsPartialExport,
} from '../../../src/output/capability/partial-export'
import { saveProgressivePartial } from '../../../src/output/portable/partial-export'
import { acquireArtifactWriter } from '../../../src/output/origin-private/export-readers'
import type { BrowserReceiveWindow } from '../../../src/output/browser/environment'

describe('on-demand partial ZIP export', () => {
  it('streams only complete spans into an independently readable partial ZIP', async () => {
    const sourceBytes = new TextEncoder().encode('unfinished-prefixHELLOunreceived-gapBYE')
    const spans = [
      span('first.txt', 17n, 'HELLO'),
      span('second.txt', 36n, 'BYE'),
    ]
    const original = structuredClone(spans)
    let scans = 0
    const chunks: Uint8Array[] = []
    const result = await exportCompleteZipEntries({
      source: new Blob([sourceBytes]),
      entries: async function* () { scans += 1; yield* spans },
      output: new WritableStream({ write: bytes => { chunks.push(bytes.slice()) } }),
      signal: new AbortController().signal,
    })
    expect(scans).toBe(2)
    expect(spans).toEqual(original)
    const bytes = new Uint8Array(await new Blob(chunks as BlobPart[]).arrayBuffer())
    expect(result).toEqual({ entryCount: 2n, exactBytes: BigInt(bytes.length) })
    const reader = new ZipReader(new Uint8ArrayReader(bytes))
    try {
      const entries = await reader.getEntries()
      expect(entries.map(entry => entry.filename)).toEqual(['first.txt', 'second.txt'])
      const files = entries.filter(entry => !entry.directory)
      expect(new TextDecoder().decode(await files[0]!.getData!(new Uint8ArrayWriter()))).toBe('HELLO')
      expect(new TextDecoder().decode(await files[1]!.getData!(new Uint8ArrayWriter()))).toBe('BYE')
    } finally { await reader.close() }
  })

  it('aborts a cancelled export without touching the original source or checkpoint', async () => {
    const controller = new AbortController()
    controller.abort(new DOMException('cancelled', 'AbortError'))
    let aborted = false
    await expect(exportCompleteZipEntries({
      source: new Blob(['abc']),
      entries: async function* () { yield span('file.txt', 0n, 'abc') },
      output: new WritableStream({ abort: () => { aborted = true } }),
      signal: controller.signal,
    })).rejects.toMatchObject({ name: 'AbortError' })
    expect(aborted).toBe(true)
  })

  it('assembles a browser-downloadable ZIP Blob in one scan, detaching small payloads and slicing large ones', async () => {
    const sourceBytes = new TextEncoder().encode('unfinished-prefixHELLOunreceived-gapBYE')
    const spans = [
      directorySpan('folder'),
      span('folder/first.txt', 17n, 'HELLO'),
      span('folder/empty.txt', 0n, ''),
      span('folder/second.txt', 36n, 'BYE'),
    ]
    let detachedScans = 0
    const detached = await buildCompleteZipBlob({
      source: new Blob([sourceBytes]),
      entries: async function* () { detachedScans += 1; yield* spans },
      signal: new AbortController().signal,
    })
    expect(detachedScans).toBe(1)
    expect(detached.detached).toBe(true)
    expect(detached.entryCount).toBe(4n)
    expect(detached.blob.type).toBe('application/zip')

    const largeSource = new Blob([sourceBytes])
    const sliceSpy = vi.spyOn(largeSource, 'slice')
    const fileBacked = await buildCompleteZipBlob({
      source: largeSource,
      entries: async function* () { yield* spans },
      signal: new AbortController().signal,
      detachMaxBytes: 0n,
    })
    expect(fileBacked.detached).toBe(false)
    expect(fileBacked.exactBytes).toBe(detached.exactBytes)
    // Only non-empty file spans are sliced; directories and empty files do not allocate slice parts.
    expect(sliceSpy).toHaveBeenCalledTimes(2)

    for (const candidate of [detached.blob, fileBacked.blob]) {
      const bytes = new Uint8Array(await candidate.arrayBuffer())
      const reader = new ZipReader(new Uint8ArrayReader(bytes), { checkSignature: true })
      try {
        const entries = await reader.getEntries()
        expect(entries.map(entry => entry.filename)).toEqual([
          'folder/', 'folder/first.txt', 'folder/empty.txt', 'folder/second.txt',
        ])
        const files = entries.filter(entry => !entry.directory)
        expect(new TextDecoder().decode(await files[0]!.getData!(new Uint8ArrayWriter()))).toBe('HELLO')
        expect(new TextDecoder().decode(await files[1]!.getData!(new Uint8ArrayWriter()))).toBe('')
        expect(new TextDecoder().decode(await files[2]!.getData!(new Uint8ArrayWriter()))).toBe('BYE')
      } finally { await reader.close() }
    }
  })

  it('rejects browser-download Blob assembly when the entry bound is exceeded', async () => {
    const spans = [span('a.txt', 0n, 'a'), span('b.txt', 1n, 'b')]
    await expect(buildCompleteZipBlob({
      source: new Blob(['ab']),
      entries: async function* () { yield* spans },
      signal: new AbortController().signal,
      maxEntries: 1,
    })).rejects.toMatchObject({ name: 'NotSupportedError' })
  })

  it('skips zero-byte entries marked with a revision failure when scanning complete spans', async () => {
    const entries = [
      {
        entryId: 'file:bad-empty', kind: 'file' as const, path: ['bad.txt'], ranges: [],
        revisionFailure: 'permanent',
        source: { shareInstance: 's', directoryId: 'd', generation: 'g', sourcePath: ['bad.txt'] },
        revision: { fileId: 'bad-empty', fileRevision: 'r1', exactSize: 0n },
        zipLayout: { entryId: 'file:bad-empty', sequence: 0n, localHeaderOffset: 0n, payloadOffset: 37n,
          exactSize: 0n, descriptorOffset: 37n, endOffset: 53n, encodingVersion: 1 as const },
      },
      {
        entryId: 'file:good-empty', kind: 'file' as const, path: ['good.txt'], ranges: [],
        source: { shareInstance: 's', directoryId: 'd', generation: 'g', sourcePath: ['good.txt'] },
        revision: { fileId: 'good-empty', fileRevision: 'r2', exactSize: 0n },
        zipLayout: { entryId: 'file:good-empty', sequence: 1n, localHeaderOffset: 53n, payloadOffset: 91n,
          exactSize: 0n, descriptorOffset: 91n, endOffset: 107n, encodingVersion: 1 as const },
      },
    ]
    const store = {
      readEntries: async ({ afterSequence }: { afterSequence?: bigint }) =>
        afterSequence === undefined ? entries : [],
    } as unknown as TaskCheckpointStore
    const collected: string[] = []
    for await (const item of readCompleteZipEntries(store)) collected.push(item.entry.entryId)
    expect(collected).toEqual(['file:good-empty'])
  })

  it('supports browser-download fallback when showSaveFilePicker is absent and coordinates writer leases', async () => {
    const bareWindow = { indexedDB: { open: vi.fn() } } as unknown as BrowserReceiveWindow
    expect(supportsPartialExport(bareWindow)).toBe(false)
    await expect(preparePartialExport(bareWindow)).rejects.toMatchObject({ name: 'NotSupportedError' })

    const handoff = fakeBrowserDownloadWindow()
    expect(supportsPartialExport(handoff.windowPort)).toBe(true)
    const destination = await preparePartialExport(handoff.windowPort)
    expect(destination.kind).toBe('browser-download')

    expect(partialExportSuggestedName('photos.zip')).toBe('photos-partial.zip')
    expect(partialExportSuggestedName('photos')).toBe('photos-partial.zip')
    expect(partialExportSuggestedName('.zip')).toBe(DEFAULT_PARTIAL_EXPORT_NAME)
    expect(partialExportSuggestedName(undefined)).toBe(DEFAULT_PARTIAL_EXPORT_NAME)

    const sourceBytes = new TextEncoder().encode('HELLO')
    const makeReader = (operationId: string) => ({
      object: { operationId, objectId: 'obj', kind: 'zip-archive' as const, handleId: 'h' },
      handle: { getFile: async () => new File([sourceBytes], 'archive.bin') } as unknown as FileSystemFileHandle,
      completeEntries: async function* () { yield span('keep.txt', 0n, 'HELLO') },
      close: async () => undefined,
    })

    // Small export detaches payloads and releases its OPFS reader lease immediately.
    await saveProgressivePartial(
      makeReader('op-detached'),
      { kind: 'browser-download', windowPort: handoff.windowPort, suggestedName: 'photos.zip' },
      new AbortController().signal,
    )
    expect(handoff.downloads).toHaveLength(1)
    expect(handoff.downloads[0]!.suggestedName).toBe('photos-partial.zip')
    const immediateWriter = await acquireArtifactWriter('op-detached')
    immediateWriter.release()

    // Large file-backed export retains its reader lease until the browser URL lease finishes.
    await saveProgressivePartial(
      makeReader('op-file-backed'),
      { kind: 'browser-download', windowPort: handoff.windowPort, suggestedName: 'big.zip' },
      new AbortController().signal,
      undefined,
      { detachMaxBytes: 0n },
    )
    await expect(acquireArtifactWriter('op-file-backed')).rejects.toMatchObject({ name: 'InvalidStateError' })
    handoff.expireAllLeases()
    const resumedWriter = await acquireArtifactWriter('op-file-backed')
    resumedWriter.release()
  })
})

function fakeBrowserDownloadWindow() {
  const downloads: { suggestedName: string; blob: Blob }[] = []
  const timers: (() => void)[] = []
  const urls = new Map<string, Blob>()
  let nextUrl = 0
  const windowPort = {
    Blob,
    URL: {
      createObjectURL: (blob: Blob) => {
        const url = `blob:test/${++nextUrl}`
        urls.set(url, blob)
        return url
      },
      revokeObjectURL: (url: string) => { urls.delete(url) },
    },
    document: {
      documentElement: { append: () => undefined },
      createElement: () => {
        const anchor = {
          download: '', href: '', hidden: false,
          click: () => { downloads.push({ suggestedName: anchor.download, blob: urls.get(anchor.href)! }) },
          remove: () => undefined,
        }
        return anchor
      },
    },
    setTimeout: (callback: () => void) => {
      timers.push(callback)
      return timers.length
    },
    clearTimeout: () => undefined,
  } as unknown as BrowserReceiveWindow
  return {
    windowPort,
    downloads,
    expireAllLeases: () => { for (const callback of timers.splice(0)) callback() },
  }
}

function directorySpan(name: string): CompleteZipEntrySpan {
  return {
    payloadOffset: 0n, length: 0n, crc32: 0,
    entry: {
      entryId: `directory:${name}`, kind: 'directory', path: [name], ranges: [],
      source: { shareInstance: 'share', directoryId: 'directory', generation: 'generation', sourcePath: [name] },
    },
  }
}

function span(name: string, payloadOffset: bigint, text: string): CompleteZipEntrySpan {
  const bytes = new TextEncoder().encode(text)
  return {
    payloadOffset, length: BigInt(bytes.length), crc32: zipBytesCrc32(bytes),
    entry: {
      entryId: name, kind: 'file', path: name.split('/'), ranges: [],
      source: { shareInstance: 'share', directoryId: 'directory', generation: 'generation', sourcePath: name.split('/') },
      revision: { fileId: name, fileRevision: 'revision', exactSize: BigInt(bytes.length) },
    },
  }
}
