import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import { V2_CATALOG_PATH_DEPTH } from '../../src/catalog/path-policy'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { FileGeometry } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import { createBoundedPortableDownloadStream } from '../../src/output/portable/browser-download'
import { SingleFileStreamOutputSession } from '../../src/output/streams/single-file'
import type { OutputSession } from '../../src/transfer/output-session'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { V2CborError, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
} from '../../src/content/v2-session-services'
import {
  V2CatalogTraversalError,
  V2DirectoryAncestry,
  V2TransferJob,
  isV2FileScopedTransferFailure,
} from '../../src/transfer/v2-job'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'

describe('v2 transfer failure domains', () => {
  it('keeps only typed revision and block domain errors file-local', () => {
    const revisionError = new V2RemoteOperationError(encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1], [1, 3], [2, 0x3001], [3, false], [4, null], [5, 'changed'],
      ]),
    ))
    const blockError = new V2RemoteOperationError(encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1], [1, 4], [2, 0x4001], [3, false], [4, null], [5, 'missing'],
      ]),
    ))

    expect(isV2FileScopedTransferFailure(revisionError)).toBe(true)
    expect(isV2FileScopedTransferFailure(blockError)).toBe(true)
    expect(isV2FileScopedTransferFailure(new V2RemoteRevisionError({
      code: 0x3002,
      retryable: false,
    }))).toBe(true)
    expect(isV2FileScopedTransferFailure(new V2BlockOperationError(
      'object-auth',
      'block sender object failed authentication',
      { cause: new Error('invalid signature') },
    ))).toBe(true)
  })

  it('promotes sender-object, wire, and session failures out of the file domain', () => {
    expect(isV2FileScopedTransferFailure(
      new SenderObjectError('authentication', 'ciphertext failed authentication'),
    )).toBe(false)
    expect(isV2FileScopedTransferFailure(new V2CborError('malformed signed result'))).toBe(false)
    expect(isV2FileScopedTransferFailure(
      new V2SessionRuntimeError('session', 'share identity changed'),
    )).toBe(false)
  })
})

describe('v2 portable output failure domain', () => {
  it('aborts the job when the portable final sink overflows', async () => {
    const publish = vi.fn()
    const stream = createBoundedPortableDownloadStream('unknown.zip', {
      createBlob: (parts) => new Blob([...parts]),
      publish,
    }, 3)
    const output = new SingleFileStreamOutputSession('portable-overflow', stream)
    const fileId = identity(11)
    const revision = {
      shareInstance: identity(1),
      shareInstanceId: 'share',
      fileId,
      fileIdText: 'file',
      fileRevision: identity(12),
      fileRevisionText: 'revision',
      exactSize: 4n,
      geometry: new FileGeometry(4n, 4n),
    }
    const committed = {
      directoryIdText: 'root',
      generationText: 'generation',
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      terminalCommitment: new Uint8Array(32),
    }
    const entry = {
      kind: 'file' as const,
      id: fileId,
      idText: 'file',
      name: 'overflow.bin',
      expectedSize: 4n,
    }
    const catalog = {
      loadDirectory: async () => committed,
      pages: async function* () {
        yield { entries: [entry] }
      },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async () => ({
        descriptor: revision,
        leaseId: identity(13),
        release: async () => undefined,
      }),
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () {
        yield { offset: 0n, data: Uint8Array.of(1, 2, 3, 4) }
      },
    } as unknown as V2BlockRangeReader

    const result = await new V2TransferJob({
      descriptor: {
        syntheticRoot: identity(2),
        syntheticRootId: 'root',
      } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output,
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome).toEqual({
      status: 'Aborted',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
    })
    expect(output.capabilities.durability).toBe('None')
    expect(publish).not.toHaveBeenCalled()
  })
})

describe('v2 catalog traversal authority', () => {
  it('releases a million sequential sibling identities from path-local ancestry', () => {
    const ancestry = new V2DirectoryAncestry()
    const leaveRoot = ancestry.enter('root')
    for (let index = 0; index < 1_000_000; index += 1) {
      const leaveSibling = ancestry.enter(`sibling-${index}`)
      leaveSibling()
    }
    expect(ancestry.depth).toBe(1)
    expect(ancestry.maximumDepth).toBe(2)
    leaveRoot()
    expect(ancestry.depth).toBe(0)
  })

  it('aborts a corrupt cached root cycle after one directory load', async () => {
    const rootId = identity(2)
    let loads = 0
    const catalog = {
      loadDirectory: async () => {
        loads += 1
        return committedDirectory('root', 1)
      },
      pages: async function* () {
        yield { entries: [directoryEntry(rootId, 'root', 'root-loop')] }
      },
    } as unknown as V2CatalogClient
    const output = traversalOutput()

    const result = await traversalJob(catalog, output.session, rootId, 'root').run()

    expect(result.outcome.status).toBe('Aborted')
    expect(loads).toBe(1)
    expect(output.abortReasons).toHaveLength(1)
    expect(output.abortReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
  })

  it('accepts the protocol depth boundary and rejects the next child without loading it', async () => {
    const accepted = depthCatalog(V2_CATALOG_PATH_DEPTH)
    const acceptedOutput = traversalOutput()
    const acceptedResult = await traversalJob(
      accepted.catalog,
      acceptedOutput.session,
      depthIdentity(0),
      depthIdentityText(0),
    ).run()

    expect(acceptedResult.outcome.status).toBe('Succeeded')
    expect(accepted.loads()).toBe(V2_CATALOG_PATH_DEPTH + 1)
    expect(acceptedOutput.abortReasons).toEqual([])

    const rejected = depthCatalog(V2_CATALOG_PATH_DEPTH + 1)
    const rejectedOutput = traversalOutput()
    const rejectedResult = await traversalJob(
      rejected.catalog,
      rejectedOutput.session,
      depthIdentity(0),
      depthIdentityText(0),
    ).run()

    expect(rejectedResult.outcome.status).toBe('Aborted')
    expect(rejected.loads()).toBe(V2_CATALOG_PATH_DEPTH + 1)
    expect(rejectedOutput.abortReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
  })

  it('rejects a file at depth 257 before revision or output I/O', async () => {
    const fixture = depthFileCatalog(V2_CATALOG_PATH_DEPTH)
    let revisionOpens = 0
    const output = traversalOutput()
    const result = await traversalJob(
      fixture.catalog,
      output.session,
      depthIdentity(0),
      depthIdentityText(0),
      {
        open: async () => {
          revisionOpens += 1
          throw new Error('Depth-invalid file reached revision I/O')
        },
      } as V2RevisionReader,
    ).run()

    expect(result.outcome.status).toBe('Aborted')
    expect(fixture.loads()).toBe(V2_CATALOG_PATH_DEPTH + 1)
    expect(revisionOpens).toBe(0)
    expect(output.abortReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
  })

  it('admits exactly 32 KiB of path bytes and rejects the next byte before child load', async () => {
    const exactSegments = maximumBytePathSegments(252)
    const exact = pathCatalog(exactSegments)
    const exactOutput = traversalOutput()
    const exactResult = await traversalJob(
      exact.catalog,
      exactOutput.session,
      depthIdentity(0),
      depthIdentityText(0),
    ).run()
    expect(exactResult.outcome.status).toBe('Succeeded')
    expect(exact.loads()).toBe(exactSegments.length + 1)

    const overSegments = maximumBytePathSegments(253)
    const over = pathCatalog(overSegments)
    const overOutput = traversalOutput()
    const overResult = await traversalJob(
      over.catalog,
      overOutput.session,
      depthIdentity(0),
      depthIdentityText(0),
    ).run()
    expect(overResult.outcome.status).toBe('Aborted')
    expect(over.loads()).toBe(overSegments.length)
    expect(overOutput.abortReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
  })
})

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function traversalJob(
  catalog: V2CatalogClient,
  output: OutputSession,
  syntheticRoot: Uint8Array<ArrayBuffer>,
  syntheticRootId: string,
  revisions: V2RevisionReader = {} as V2RevisionReader,
): V2TransferJob {
  return new V2TransferJob({
    descriptor: { syntheticRoot, syntheticRootId } as never,
    catalog,
    selection: new V2SelectionPolicy(),
    revisions,
    broker: {} as V2BlockRangeReader,
    lanes: { size: 1 },
    output,
    maximumConcurrentFiles: 1,
  })
}

function traversalOutput(): {
  readonly session: OutputSession
  readonly abortReasons: unknown[]
} {
  const abortReasons: unknown[] = []
  const session = {
    identity: { backend: 'test', outputSessionId: 'traversal' },
    capabilities: {
      durability: 'None',
      randomWrite: false,
      fileFailureIsolation: false,
      modificationTime: false,
    },
    ensureDirectory: async () => undefined,
    finalizeDirectory: async () => undefined,
    beginFile: async () => { throw new Error('Traversal fixture unexpectedly opened a file') },
    finishJob: async () => undefined,
    abortJob: async (reason: unknown) => { abortReasons.push(reason) },
  } as unknown as OutputSession
  return { session, abortReasons }
}

function committedDirectory(directoryIdText: string, entryCount: number): V2CommittedDirectory {
  return Object.freeze({
    directoryIdText,
    generationText: 'generation',
    pageCount: 1,
    entryCount,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
}

function directoryEntry(
  id: Uint8Array<ArrayBuffer>,
  idText: string,
  name: string,
): Extract<V2CatalogEntry, { kind: 'directory' }> {
  return Object.freeze({ kind: 'directory', id, idText, name })
}

function depthCatalog(leafDepth: number): {
  readonly catalog: V2CatalogClient
  loads(): number
} {
  let loads = 0
  const catalog = {
    loadDirectory: async (id: Uint8Array) => {
      loads += 1
      const depth = depthFromIdentity(id)
      return committedDirectory(depthIdentityText(depth), depth === leafDepth ? 0 : 1)
    },
    pages: async function* (directory: V2CommittedDirectory) {
      const depth = Number(directory.directoryIdText.slice('directory-'.length))
      const entries = depth === leafDepth
        ? []
        : [directoryEntry(
            depthIdentity(depth + 1),
            depthIdentityText(depth + 1),
            depthIdentityText(depth + 1),
          )]
      yield { entries }
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

function depthFileCatalog(parentDepth: number): {
  readonly catalog: V2CatalogClient
  loads(): number
} {
  let loads = 0
  const catalog = {
    loadDirectory: async (id: Uint8Array) => {
      loads += 1
      return committedDirectory(depthIdentityText(depthFromIdentity(id)), 1)
    },
    pages: async function* (directory: V2CommittedDirectory) {
      const depth = Number(directory.directoryIdText.slice('directory-'.length))
      const entries: V2CatalogEntry[] = depth === parentDepth
        ? [{
            kind: 'file',
            id: depthIdentity(depth + 1),
            idText: `file-${depth + 1}`,
            name: `file-${depth + 1}`,
            expectedSize: 0n,
          }]
        : [directoryEntry(
            depthIdentity(depth + 1),
            depthIdentityText(depth + 1),
            depthIdentityText(depth + 1),
          )]
      yield { entries }
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

function pathCatalog(segments: readonly string[]): {
  readonly catalog: V2CatalogClient
  loads(): number
} {
  let loads = 0
  const catalog = {
    loadDirectory: async (id: Uint8Array) => {
      loads += 1
      const depth = depthFromIdentity(id)
      return committedDirectory(depthIdentityText(depth), depth === segments.length ? 0 : 1)
    },
    pages: async function* (directory: V2CommittedDirectory) {
      const depth = Number(directory.directoryIdText.slice('directory-'.length))
      const name = segments[depth]
      const entries = name === undefined
        ? []
        : [directoryEntry(depthIdentity(depth + 1), depthIdentityText(depth + 1), name)]
      yield { entries }
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

function maximumBytePathSegments(penultimateWidth: number): readonly string[] {
  return Object.freeze([
    ...Array.from({ length: 127 }, () => 'a'.repeat(255)),
    'a'.repeat(penultimateWidth),
    'b',
    'c',
  ])
}

function depthIdentity(depth: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  new DataView(value.buffer).setUint16(14, depth + 1, false)
  return value
}

function depthFromIdentity(identityValue: Uint8Array): number {
  return new DataView(
    identityValue.buffer,
    identityValue.byteOffset,
    identityValue.byteLength,
  ).getUint16(14, false) - 1
}

function depthIdentityText(depth: number): string {
  return `directory-${depth}`
}
