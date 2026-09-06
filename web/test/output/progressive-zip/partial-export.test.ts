import { describe, expect, it } from 'vitest'
import { ZipReader, Uint8ArrayReader, Uint8ArrayWriter } from '@zip.js/zip.js'
import { exportCompleteZipEntries } from '../../../src/output/progressive-zip/partial-export'
import { zipBytesCrc32 } from '../../../src/output/progressive-zip/crc-ranges'
import type { CompleteZipEntrySpan } from '../../../src/output/progressive-zip/archive'

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
})

function span(name: string, payloadOffset: bigint, text: string): CompleteZipEntrySpan {
  const bytes = new TextEncoder().encode(text)
  return {
    payloadOffset, length: BigInt(bytes.length), crc32: zipBytesCrc32(bytes),
    entry: {
      entryId: name, kind: 'file', path: [name], ranges: [],
      source: { shareInstance: 'share', directoryId: 'directory', generation: 'generation', sourcePath: [name] },
      revision: { fileId: name, fileRevision: 'revision', exactSize: BigInt(bytes.length) },
    },
  }
}
