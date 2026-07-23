import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, jobOutcome } from '../../src/transfer/outcome'
import {
  directoryRecord,
  fileRecord,
} from '../../src/output/persistence/journal'
import type {
  StagedOutputCatalog,
  StagedOutputFile,
} from '../../src/output/persistent-tree/session'
import { OriginPrivateZipExporter } from '../../src/output/origin-private/zip-exporter'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'

const identity = Object.freeze({
  backend: 'origin-private-staging',
  outputSessionId: 'export-session',
})
const OUTCOME = jobOutcome('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const ACTIVE_SIGNAL = new AbortController().signal

describe('origin-private ZIP export', () => {
  it('streams committed staged files and empty entries into a valid archive', async () => {
    const output = recordedOutput()
    const catalog = stagedCatalog()
    await exporter(output.stream).export(catalog, OUTCOME, ACTIVE_SIGNAL)

    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const entries = await reader.getEntries()
    expect(entries.map((entry) => entry.filename)).toEqual([
      'empty-directory/',
      'empty.bin',
      'tree/file.bin',
    ])
    const file = entries.find((entry) => entry.filename === 'tree/file.bin')
    if (file === undefined || file.directory) throw new Error('Exported file is missing')
    expect(await file.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
    expect(output.closed).toBe(true)
  })

  it('aborts the archive when staged size no longer matches its committed record', async () => {
    const output = recordedOutput()
    const catalog = stagedCatalog()
    const corrupted: StagedOutputCatalog = {
      directories: () => catalog.directories(),
      files: async function* () {
        let index = 0
        for await (const file of catalog.files()) {
          yield index++ === 0
            ? { ...file, read: async () => new Blob([Uint8Array.of(9)]) }
            : file
        }
      },
    }

    await expect(exporter(output.stream).export(corrupted, OUTCOME, ACTIVE_SIGNAL))
      .rejects.toThrow('Staged output size changed')
    expect(output.aborted).toBe(true)
  })

  it('shares one awaitable abort settlement across repeated callers', async () => {
    const abortStarted = deferred<void>()
    const releaseAbort = deferred<void>()
    const abortReasons: unknown[] = []
    const output = new WritableStream<Uint8Array>({
      abort: async (reason) => {
        abortReasons.push(reason)
        abortStarted.resolve()
        await releaseAbort.promise
      },
    })
    const subject = exporter(output)
    const reason = new DOMException('cancelled', 'AbortError')

    const first = subject.abort(reason)
    const second = subject.abort(new Error('must not replace the first reason'))
    expect(second).toBe(first)
    await abortStarted.promise
    let settled = false
    first.then(() => { settled = true }, () => { settled = true })
    await Promise.resolve()
    expect(settled).toBe(false)

    releaseAbort.resolve()
    await expect(first).resolves.toBeUndefined()
    await expect(second).resolves.toBeUndefined()
    expect(abortReasons).toEqual([reason])
  })

  it('fails closed when archive resource construction rejects', async () => {
    const output = recordedOutput()
    const subject = new OriginPrivateZipExporter(output.stream, () => {
      throw new Error('spool acquisition failed')
    })

    await expect(subject.export(stagedCatalog(), OUTCOME, ACTIVE_SIGNAL))
      .rejects.toThrow('spool acquisition failed')
    expect(output.aborted).toBe(true)
    await expect(subject.abort(new Error('repeated cleanup'))).resolves.toBeUndefined()
  })
})

function stagedCatalog(): StagedOutputCatalog {
  const emptyRecord = stagedFileRecord('empty.bin', 0n, [], 'empty-file')
  const fileRecordValue = stagedFileRecord(
    'tree/file.bin',
    3n,
    [byteRange(0n, 3n)],
    'regular-file',
  )
  const directories = [
    directoryRecord(identity, ['empty-directory'], 'empty-directory', true, undefined, true, 1n),
  ]
  const files: StagedOutputFile[] = [
    Object.freeze({ record: emptyRecord, read: async () => new Blob([]) }),
    Object.freeze({ record: fileRecordValue, read: async () => new Blob([Uint8Array.of(1, 2, 3)]) }),
  ]
  return Object.freeze({
    directories: async function* () { yield* directories },
    files: async function* () { yield* files },
  })
}

function exporter(output: WritableStream<Uint8Array>): OriginPrivateZipExporter {
  return new OriginPrivateZipExporter(output, () => new MemoryZipCentralDirectorySpool())
}

function stagedFileRecord(
  path: string,
  exactSize: bigint,
  ranges: readonly ReturnType<typeof byteRange>[],
  ownedFileIdentity: string,
) {
  const canonicalPath = path.split('/')
  return fileRecord(
    identity,
    { ...identity, canonicalPath, ownedFileIdentity },
    {
      source: {
        shareInstance: 'share',
        fileId: path,
        fileRevision: `revision-${path}`,
      },
      path: canonicalPath,
      exactSize,
    },
    ranges,
    true,
    1n,
  )
}

function recordedOutput(): {
  readonly stream: WritableStream<Uint8Array>
  readonly closed: boolean
  readonly aborted: boolean
  bytes(): Uint8Array
} {
  const chunks: Uint8Array[] = []
  const state = { closed: false, aborted: false }
  return {
    stream: new WritableStream<Uint8Array>({
      write: (chunk) => { chunks.push(chunk.slice()) },
      close: () => { state.closed = true },
      abort: () => { state.aborted = true },
    }),
    get closed() { return state.closed },
    get aborted() { return state.aborted },
    bytes: () => {
      const output = new Uint8Array(chunks.reduce((total, chunk) => total + chunk.byteLength, 0))
      let offset = 0
      for (const chunk of chunks) {
        output.set(chunk, offset)
        offset += chunk.byteLength
      }
      return output
    },
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
  readonly reject: (reason?: unknown) => void
} {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((complete, fail) => {
    resolve = complete
    reject = fail
  })
  return { promise, resolve, reject }
}
