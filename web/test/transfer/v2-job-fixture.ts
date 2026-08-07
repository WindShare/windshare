import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../src/content/v2-session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import { freezeTransferIntent } from '../../src/transfer/intent'
import {
  VerifiedDurableRanges,
  type OutputDirectoryAdmission,
  type OutputDirectory,
  type OutputFile,
  type OutputSession,
  type V2OutputAuthority,
} from '../../src/transfer/output-session'
import { TransferJob } from '../../src/transfer/v2-job'

export function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

export function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

export function opaqueOutputIdentityText(first: number): string {
  const value = new Uint8Array(32)
  value[0] = first
  return encodeBase64Url(value)
}

export function catalogCommitment(): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(32)
  value[0] = 1
  return value
}

export function openedRevision(
  file: Uint8Array<ArrayBuffer>,
  exactSize: bigint,
  blockSize: bigint,
): V2OpenedRevision {
  const revision = identity(12)
  return Object.freeze({
    descriptor: Object.freeze({
      shareInstance: identity(1),
      shareInstanceId: identityText(1),
      fileId: file,
      fileIdText: encodeBase64Url(file),
      fileRevision: revision,
      fileRevisionText: encodeBase64Url(revision),
      exactSize,
      geometry: new FileGeometry(exactSize, blockSize),
    }),
    leaseId: identity(13),
    release: async () => undefined,
  })
}

export function traversalPage(
  directoryId: Uint8Array<ArrayBuffer>,
  entries: readonly V2CatalogEntry[],
  overrides: {
    readonly shareInstance?: Uint8Array<ArrayBuffer>
    readonly generation?: Uint8Array<ArrayBuffer>
    readonly pageIndex?: number
    readonly terminal?: boolean
    readonly previousCommitment?: Uint8Array<ArrayBuffer>
    readonly omittedCount?: bigint
    readonly objectCommitment?: Uint8Array<ArrayBuffer>
    readonly preserveEntryOrder?: boolean
  } = {},
) {
  const canonicalEntries = entries.map((entry) => Object.freeze({
    ...entry,
    idText: encodeBase64Url(entry.id),
  }))
  if (!overrides.preserveEntryOrder) {
    canonicalEntries.sort((left, right) => compareUtf8(left.name, right.name))
  }
  return {
    shareInstance: overrides.shareInstance ?? identity(1),
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    generation: overrides.generation ?? identity(3),
    generationText: 'generation',
    pageIndex: overrides.pageIndex ?? 0,
    terminal: overrides.terminal ?? true,
    previousCommitment: overrides.previousCommitment ?? new Uint8Array(32),
    entries: canonicalEntries,
    omittedCount: overrides.omittedCount ?? 0n,
    objectCommitment: overrides.objectCommitment ?? catalogCommitment(),
    senderObjectBytes: 1,
  }
}

export function compareUtf8(left: string, right: string): number {
  const encoder = new TextEncoder()
  const leftBytes = encoder.encode(left)
  const rightBytes = encoder.encode(right)
  const shared = Math.min(leftBytes.byteLength, rightBytes.byteLength)
  for (let index = 0; index < shared; index += 1) {
    const difference = (leftBytes[index] ?? 0) - (rightBytes[index] ?? 0)
    if (difference !== 0) return difference
  }
  return leftBytes.byteLength - rightBytes.byteLength
}

export function outputAuthority(session: OutputSession): V2OutputAuthority {
  let opened = false
  return {
    confirmOutput: async (draft) => ({
      intent: await freezeTransferIntent(draft, {
        target: opaqueOutputIdentityText(201),
        targetKind: 2,
        backend: session.identity.backend,
        format: session.format,
      }),
      session,
    }),
    openOutput: async () => {
      if (opened) throw new Error('Test output authority was opened twice')
      opened = true
      return session
    },
    abort: (reason: unknown) => session.abortJob(reason),
  }
}

export function terminalBoundaryOutput(initialDurableStart?: bigint): OutputSession {
  const identity = { backend: 'test-terminal', outputSessionId: 'selection-bound' }
  const directoryAdmissions = new DirectoryAdmissionLedger()
  return {
    identity,
    format: 'directory',
    capabilities: {
      durability: 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime: false,
    },
    admitDirectory: (directory: OutputDirectoryAdmission, signal: AbortSignal) =>
      directoryAdmissions.admitDirectory(directory, signal),
    finalizeDirectory: async (directory) => {
      directoryAdmissions.validateDirectoryFinalization(directory)
    },
    beginFile: async (input) => {
      const file = directoryAdmissions.validateFileParent(input)
      const ownership = {
        ...identity,
        canonicalPath: file.path,
        ownedFileIdentity: 'terminal-file',
      }
      return {
        durableRanges: new VerifiedDurableRanges(
          ownership,
          file.source,
          file.exactSize,
          initialDurableStart === undefined || initialDurableStart >= file.exactSize
            ? []
            : [byteRange(initialDurableStart, file.exactSize)],
        ),
        transaction: {
          writeRange: async () => undefined,
          checkpoint: async () => new VerifiedDurableRanges(
            ownership,
            file.source,
            file.exactSize,
            [byteRange(0n, file.exactSize)],
          ),
          commit: async () => undefined,
          abort: async () => 'FileIsolated' as const,
        },
      }
    },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
    suspendJob: async () => undefined,
  }
}

export function traversalJob(
  catalog: V2CatalogClient,
  output: OutputSession,
  syntheticRoot: Uint8Array<ArrayBuffer>,
  syntheticRootId: string,
  revisions: V2RevisionReader = {} as V2RevisionReader,
): TransferJob {
  return new TransferJob({
    descriptor: { shareInstance: identity(1), syntheticRoot, syntheticRootId, chunkSize: 1 } as never,
    catalog,
    selection: new V2SelectionPolicy(),
    revisions,
    broker: {} as V2BlockRangeReader,
    lanes: { size: 1 },
    output: outputAuthority(output),
    maximumConcurrentFiles: 1,
  })
}

export function traversalOutput(): {
  readonly session: OutputSession
  readonly abortReasons: unknown[]
  readonly suspendReasons: unknown[]
  readonly finalizedPaths: readonly (readonly string[])[]
  readonly begunFilePaths: readonly (readonly string[])[]
} {
  const abortReasons: unknown[] = []
  const suspendReasons: unknown[] = []
  const finalizedPaths: string[][] = []
  const begunFilePaths: string[][] = []
  const directoryAdmissions = new DirectoryAdmissionLedger()
  const session = {
    identity: { backend: 'test', outputSessionId: 'traversal' },
    format: 'directory',
    capabilities: {
      durability: 'None',
      randomWrite: false,
      fileFailureIsolation: false,
      modificationTime: false,
    },
    admitDirectory: (directory: OutputDirectoryAdmission, signal: AbortSignal) =>
      directoryAdmissions.admitDirectory(directory, signal),
    finalizeDirectory: async (directory: OutputDirectory) => {
      const admitted = directoryAdmissions.validateDirectoryFinalization(directory)
      finalizedPaths.push([...admitted.path])
    },
    beginFile: async (file: OutputFile) => {
      begunFilePaths.push([...file.path])
      throw new Error('Traversal fixture unexpectedly opened a file')
    },
    finishJob: async () => undefined,
    abortJob: async (reason: unknown) => { abortReasons.push(reason) },
    suspendJob: async (reason: unknown) => { suspendReasons.push(reason) },
  } as unknown as OutputSession
  return { session, abortReasons, suspendReasons, finalizedPaths, begunFilePaths }
}

export function committedDirectory(
  directoryIdText: string,
  entryCount: number,
  omittedCount = 0n,
): V2CommittedDirectory {
  let directoryId: Uint8Array<ArrayBuffer>
  if (directoryIdText === 'root') {
    directoryId = identity(2)
  } else if (directoryIdText === 'child') {
    directoryId = identity(3)
  } else {
    directoryId = depthIdentity(Number(directoryIdText.slice('directory-'.length)))
  }
  return Object.freeze({
    directoryId,
    directoryIdText,
    generationText: 'generation',
    generation: identity(3),
    pageCount: 1,
    entryCount,
    omittedCount,
    terminalCommitment: catalogCommitment(),
  })
}

export function committedDirectoryFor(
  directoryId: Uint8Array<ArrayBuffer>,
  directoryIdText: string,
  entryCount: number,
  omittedCount = 0n,
): V2CommittedDirectory {
  return Object.freeze({
    directoryId: directoryId.slice(),
    directoryIdText,
    generationText: 'generation',
    generation: identity(3),
    pageCount: 1,
    entryCount,
    omittedCount,
    terminalCommitment: catalogCommitment(),
  })
}

export function directoryEntry(
  id: Uint8Array<ArrayBuffer>,
  idText: string,
  name: string,
): Extract<V2CatalogEntry, { kind: 'directory' }> {
  return Object.freeze({ kind: 'directory', id, idText, name })
}

export function fileEntry(
  id: Uint8Array<ArrayBuffer>,
  name: string,
  expectedSize: bigint,
): Extract<V2CatalogEntry, { kind: 'file' }> {
  return Object.freeze({ kind: 'file', id, idText: encodeBase64Url(id), name, expectedSize })
}

export function depthCatalog(leafDepth: number): {
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
      yield traversalPage(depthIdentity(depth), entries)
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

export function depthFileCatalog(parentDepth: number): {
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
      yield traversalPage(depthIdentity(depth), entries)
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

export function pathCatalog(segments: readonly string[]): {
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
      yield traversalPage(depthIdentity(depth), entries)
    },
  } as unknown as V2CatalogClient
  return { catalog, loads: () => loads }
}

export function maximumBytePathSegments(penultimateWidth: number): readonly string[] {
  return Object.freeze([
    ...Array.from({ length: 127 }, () => 'a'.repeat(255)),
    'a'.repeat(penultimateWidth),
    'b',
    'c',
  ])
}

export function depthIdentity(depth: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  new DataView(value.buffer).setUint16(14, depth + 1, false)
  return value
}

export function wideIdentity(number: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  new DataView(value.buffer).setUint32(12, number, false)
  return value
}

export function wideIdentityNumber(value: Uint8Array): number {
  return new DataView(value.buffer, value.byteOffset, value.byteLength).getUint32(12, false)
}

export function depthFromIdentity(identityValue: Uint8Array): number {
  return new DataView(
    identityValue.buffer,
    identityValue.byteOffset,
    identityValue.byteLength,
  ).getUint16(14, false) - 1
}

export function depthIdentityText(depth: number): string {
  return `directory-${depth}`
}

export async function withTimeout<T>(operation: Promise<T>, milliseconds: number, message: string): Promise<T> {
  let timeout: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_, reject) => {
        timeout = setTimeout(() => reject(new Error(message)), milliseconds)
      }),
    ])
  } finally {
    if (timeout !== undefined) clearTimeout(timeout)
  }
}
