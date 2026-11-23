import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry, type ByteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { TransferOutputFile } from '../../src/transfer/output-port'

export function gate() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

export function outputFile(overrides: Partial<TransferOutputFile>): TransferOutputFile {
  return {
    completedRanges: [], writeAt: async () => undefined, checkpoint: async () => 'saved',
    pause: async () => undefined, commit: async () => undefined, abort: async () => undefined,
    ...overrides,
  }
}

export function id(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

export function makeShareFixture(files: readonly {
  readonly seed: number
  readonly name: string
  readonly bytes: Uint8Array<ArrayBuffer>
  readonly revSeed?: number
  readonly missing?: boolean
}[]) {
  const shareInstance = id(1)
  const rootId = id(2)
  const entries: CatalogEntry[] = files.map(file => Object.freeze({
    kind: 'file' as const,
    id: id(file.seed),
    idText: encodeBase64Url(id(file.seed)),
    name: file.name,
    expectedSize: BigInt(file.bytes.byteLength),
  }))
  const committed: CommittedDirectory = Object.freeze({
    directoryId: rootId,
    directoryIdText: encodeBase64Url(rootId),
    generation: id(3),
    generationText: encodeBase64Url(id(3)),
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId: rootId,
    directoryIdText: committed.directoryIdText,
    generation: committed.generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze(entries),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: committed.directoryIdText,
  }) as unknown as ShareDescriptor
  const catalog = Object.freeze({
    loadDirectory: async () => committed,
    page: async () => page,
  }) as unknown as CatalogClient

  const byId = new Map(files.map(file => [encodeBase64Url(id(file.seed)), file]))
  const openedFileIds: string[] = []
  const revisions: RevisionReader = {
    open: async (fileId: Uint8Array) => {
      const key = encodeBase64Url(fileId)
      openedFileIds.push(key)
      const file = byId.get(key)
      if (file === undefined || file.missing) throw new Error('missing file')
      const rev = id(file.revSeed ?? (file.seed + 50))
      const revDesc: FileRevisionDescriptor = Object.freeze({
        shareInstance,
        shareInstanceId: descriptor.shareInstanceId,
        fileId: id(file.seed),
        fileIdText: key,
        fileRevision: rev,
        fileRevisionText: encodeBase64Url(rev),
        exactSize: BigInt(file.bytes.byteLength),
        geometry: new FileGeometry(BigInt(file.bytes.byteLength), 8n),
      })
      return Object.freeze({
        descriptor: revDesc,
        leaseId: id(99),
        release: async () => undefined,
      })
    },
  }
  const readRanges: ByteRange[] = []
  const broker: BlockRangeReader = {
    readRange: async function* (revDesc, _leaseId, span) {
      readRanges.push(span)
      const file = byId.get(revDesc.fileIdText)!
      for (let pos = span.start; pos < span.end; pos += 4n) {
        const end = pos + 4n < span.end ? pos + 4n : span.end
        yield Object.freeze({
          offset: pos,
          data: file.bytes.subarray(Number(pos), Number(end)),
        })
      }
    },
  }
  return { descriptor, catalog, revisions, broker, readRanges, openedFileIds }
}
