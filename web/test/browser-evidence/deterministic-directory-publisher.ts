import { createHash } from 'node:crypto'
import { lstat, mkdir, open, readFile, readdir, rename, rm } from 'node:fs/promises'
import { join } from 'node:path'

import type { GuardUploadDirectoryPublisher } from '../../scripts/browser-evidence/artifact/directory-publisher.ts'
import { assertGuardExecutionWindowUsable } from '../../scripts/browser-evidence/execution/guard-execution-lease.ts'
import type { GuardExecutionWindow } from '../../scripts/browser-evidence/execution/guard-execution-lease.ts'
import type {
  ExistingDirectoryPublisherInventory,
  ExistingDirectoryPublisherRequest,
  ExistingDirectoryPublisherResponse,
  ExistingDirectoryPublisherResponseFor,
  ExistingDirectoryPublisherSnapshot,
  PublisherHelperFailureCode,
} from '../../scripts/browser-network-matrix/cli/publisher-helper-protocol.ts'

interface PreparedDirectory {
  readonly receipt: string
}

/**
 * The unit contract needs the publisher's transaction semantics, but native
 * process execution is a platform integration concern. This in-process owner
 * deliberately performs the same prepare/materialize/publish/verify lifecycle
 * against disposable fixture roots without acquiring executable authority.
 */
export function createDeterministicDirectoryPublisher(): GuardUploadDirectoryPublisher {
  const preparedDirectories = new Map<string, PreparedDirectory>()
  return Object.freeze({
    invoke: async <Request extends ExistingDirectoryPublisherRequest>(
      request: Request,
      executionWindow: GuardExecutionWindow,
    ): Promise<ExistingDirectoryPublisherResponseFor<Request['operation']>> => {
      assertGuardExecutionWindowUsable(executionWindow)
      executionWindow.signal.throwIfAborted()
      const response = await invokeDeterministicPublisher(
        preparedDirectories,
        request,
        executionWindow.signal,
      )
      executionWindow.signal.throwIfAborted()
      return response as ExistingDirectoryPublisherResponseFor<Request['operation']>
    },
  })
}

async function invokeDeterministicPublisher(
  preparedDirectories: Map<string, PreparedDirectory>,
  request: ExistingDirectoryPublisherRequest,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  switch (request.operation) {
    case 'prepare-existing-directory':
      return prepareDirectory(preparedDirectories, request, signal)
    case 'publish-existing-directory':
      return publishDirectory(preparedDirectories, request, signal)
    case 'verify-existing-directory':
      return verifyDirectory(request, signal)
    case 'cleanup-existing-directory':
      return cleanupDirectory(preparedDirectories, request, signal)
  }
}

async function prepareDirectory(
  preparedDirectories: Map<string, PreparedDirectory>,
  request: Extract<ExistingDirectoryPublisherRequest, { readonly operation: 'prepare-existing-directory' }>,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  const stagingPath = join(request.parentPath, request.stagingName)
  const outputPath = join(request.parentPath, request.outputName)
  if (await pathExists(stagingPath) || await pathExists(outputPath)) {
    return failed(request.operation, 'destination-exists')
  }
  signal.throwIfAborted()
  await mkdir(stagingPath, { mode: 0o700 })
  try {
    for (const relativePath of request.inventory.directories) {
      signal.throwIfAborted()
      await mkdir(inventoryPath(stagingPath, relativePath), { recursive: true, mode: 0o700 })
    }
    for (const file of request.inventory.files) {
      signal.throwIfAborted()
      const handle = await open(inventoryPath(stagingPath, file.relativePath), 'wx', 0o600)
      try {
        await handle.truncate(Number(file.byteLength))
      } finally {
        await handle.close()
      }
    }
  } catch (cause) {
    await rm(stagingPath, { force: true, recursive: true }).catch(() => undefined)
    throw cause
  }
  const receipt = createHash('sha256')
    .update(request.parentPath)
    .update('\0')
    .update(request.stagingName)
    .digest('hex')
  preparedDirectories.set(stagingPath, Object.freeze({ receipt }))
  return Object.freeze({
    outcome: 'completed',
    operation: request.operation,
    stagingReceipt: receipt,
  })
}

async function publishDirectory(
  preparedDirectories: Map<string, PreparedDirectory>,
  request: Extract<ExistingDirectoryPublisherRequest, { readonly operation: 'publish-existing-directory' }>,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  const stagingPath = join(request.parentPath, request.stagingName)
  const outputPath = join(request.parentPath, request.outputName)
  const prepared = preparedDirectories.get(stagingPath)
  if (prepared?.receipt !== request.stagingReceipt) {
    return failed(request.operation, 'publication-unsafe')
  }
  if (await pathExists(outputPath)) return failed(request.operation, 'destination-exists')
  if (!await inventoryMatches(stagingPath, request.inventory, true, signal)) {
    return failed(request.operation, 'publication-unsafe')
  }
  signal.throwIfAborted()
  await rename(stagingPath, outputPath)
  preparedDirectories.delete(stagingPath)
  return snapshotResponse(request, outputPath, signal)
}

async function verifyDirectory(
  request: Extract<ExistingDirectoryPublisherRequest, { readonly operation: 'verify-existing-directory' }>,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  const outputPath = join(request.parentPath, request.outputName)
  if (!await inventoryMatches(outputPath, request.inventory, true, signal)) {
    return failed(request.operation, 'publication-unsafe')
  }
  return snapshotResponse(request, outputPath, signal)
}

async function cleanupDirectory(
  preparedDirectories: Map<string, PreparedDirectory>,
  request: Extract<ExistingDirectoryPublisherRequest, { readonly operation: 'cleanup-existing-directory' }>,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  const stagingPath = join(request.parentPath, request.stagingName)
  if (!await pathExists(stagingPath)) {
    preparedDirectories.delete(stagingPath)
    return Object.freeze({
      outcome: 'completed',
      operation: request.operation,
      cleanupOutcome: 'absent',
    })
  }
  const prepared = preparedDirectories.get(stagingPath)
  if (
    prepared?.receipt !== request.stagingReceipt ||
    !await inventoryMatches(stagingPath, request.inventory, false, signal)
  ) {
    return Object.freeze({
      outcome: 'completed',
      operation: request.operation,
      cleanupOutcome: 'ambiguous',
    })
  }
  signal.throwIfAborted()
  await rm(stagingPath, { recursive: true })
  preparedDirectories.delete(stagingPath)
  return Object.freeze({
    outcome: 'completed',
    operation: request.operation,
    cleanupOutcome: 'completed',
  })
}

async function snapshotResponse(
  request: Extract<ExistingDirectoryPublisherRequest, {
    readonly operation: 'publish-existing-directory' | 'verify-existing-directory'
  }>,
  root: string,
  signal: AbortSignal,
): Promise<ExistingDirectoryPublisherResponse> {
  const snapshots: ExistingDirectoryPublisherSnapshot[] = []
  for (const relativePath of request.snapshotPaths) {
    signal.throwIfAborted()
    const bytes = Uint8Array.from(await readFile(inventoryPath(root, relativePath)))
    snapshots.push(Object.freeze({
      relativePath,
      byteLength: String(bytes.byteLength),
      bytes,
      sha256: sha256(bytes),
    }))
  }
  return Object.freeze({
    outcome: 'completed',
    operation: request.operation,
    manifestSha256: request.expectedManifestSha256,
    snapshots: Object.freeze(snapshots),
  })
}

async function inventoryMatches(
  root: string,
  inventory: ExistingDirectoryPublisherInventory,
  verifyDigests: boolean,
  signal: AbortSignal,
): Promise<boolean> {
  try {
    const rootMetadata = await lstat(root)
    if (!rootMetadata.isDirectory() || rootMetadata.isSymbolicLink()) return false
    const observedDirectories = new Set<string>()
    const observedFiles = new Map<string, { readonly byteLength: string; readonly sha256: string }>()
    await enumerateInventory(root, '', observedDirectories, observedFiles, verifyDigests, signal)
    const expectedDirectories = new Set(inventory.directories)
    if (!sameSet(observedDirectories, expectedDirectories)) return false
    if (observedFiles.size !== inventory.files.length) return false
    for (const expected of inventory.files) {
      const observed = observedFiles.get(expected.relativePath)
      if (observed?.byteLength !== expected.byteLength) return false
      if (verifyDigests && observed.sha256 !== expected.sha256) return false
    }
    return true
  } catch {
    return false
  }
}

async function enumerateInventory(
  root: string,
  relativeDirectory: string,
  directories: Set<string>,
  files: Map<string, { readonly byteLength: string; readonly sha256: string }>,
  verifyDigests: boolean,
  signal: AbortSignal,
): Promise<void> {
  const directory = relativeDirectory === '' ? root : inventoryPath(root, relativeDirectory)
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    signal.throwIfAborted()
    const relativePath = relativeDirectory === ''
      ? entry.name
      : `${relativeDirectory}/${entry.name}`
    const path = join(directory, entry.name)
    const metadata = await lstat(path)
    if (entry.isSymbolicLink() || metadata.isSymbolicLink()) throw new Error('symbolic inventory entry')
    if (entry.isDirectory() && metadata.isDirectory()) {
      directories.add(relativePath)
      await enumerateInventory(root, relativePath, directories, files, verifyDigests, signal)
      continue
    }
    if (!entry.isFile() || !metadata.isFile()) throw new Error('unsupported inventory entry')
    const bytes = verifyDigests ? await readFile(path) : undefined
    files.set(relativePath, Object.freeze({
      byteLength: String(metadata.size),
      sha256: bytes === undefined ? '' : sha256(bytes),
    }))
  }
}

function inventoryPath(root: string, relativePath: string): string {
  const segments = relativePath.split('/')
  if (segments.some((segment) => segment.length === 0 || segment === '.' || segment === '..')) {
    throw new Error('deterministic publisher received a non-portable inventory path')
  }
  return join(root, ...segments)
}

function failed(
  operation: ExistingDirectoryPublisherRequest['operation'],
  failureCode: PublisherHelperFailureCode,
): ExistingDirectoryPublisherResponse {
  return Object.freeze({
    outcome: 'failed',
    operation,
    failureCode,
    stagingReceipt: null,
    cleanupOutcome: null,
  })
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await lstat(path)
    return true
  } catch (cause) {
    if (isNotFound(cause)) return false
    throw cause
  }
}

function isNotFound(cause: unknown): boolean {
  return typeof cause === 'object' && cause !== null && 'code' in cause && cause.code === 'ENOENT'
}

function sameSet(left: ReadonlySet<string>, right: ReadonlySet<string>): boolean {
  return left.size === right.size && [...left].every((value) => right.has(value))
}

function sha256(bytes: Uint8Array): string {
  return createHash('sha256').update(bytes).digest('hex')
}
