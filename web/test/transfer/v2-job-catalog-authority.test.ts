import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import { V2_CATALOG_PATH_DEPTH } from '../../src/catalog/path-policy'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createDirectoryAdmission,
  DirectoryAdmissionBindingError,
  OutputDirectoryMutationError,
  type OutputSession,
} from '../../src/transfer/output-session'
import {
  V2CatalogTraversalError,
  V2DirectoryTraversalError,
  V2DirectoryAncestry,
  TransferJob,
  type TransferProgress,
} from '../../src/transfer/v2-job'
import { V2SelectionTargetMissingError } from '../../src/transfer/v2-job-selection'
import {
  committedDirectory,
  committedDirectoryFor,
  depthCatalog,
  depthFileCatalog,
  depthIdentity,
  depthIdentityText,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  maximumBytePathSegments,
  openedRevision,
  outputAuthority,
  pathCatalog,
  terminalBoundaryOutput,
  traversalJob,
  traversalOutput,
  traversalPage,
  withTimeout,
} from './v2-job-fixture'

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

  it('pauses after committed-root discovery when an adapter returns a forged admission', async () => {
    const rootId = identity(2)
    let loads = 0
    const catalog = {
      loadDirectory: async () => {
        loads += 1
        return committedDirectory('root', 0)
      },
    } as unknown as V2CatalogClient
    const output = traversalOutput()
    output.session.admitDirectory = async (request) => createDirectoryAdmission({
      ...request,
      generation: identityText(99),
    })

    const result = await traversalJob(catalog, output.session, rootId, 'root').run()

    expect(result.outcome.status).toBe('Paused')
    expect(loads).toBe(1)
    expect(output.suspendReasons[0]).toBeInstanceOf(DirectoryAdmissionBindingError)
    expect(output.abortReasons).toEqual([])
  })

  it('rejects root omission before output admission or page consumption', async () => {
    const root = identity(2)
    const admitDirectory = vi.fn()
    const pages = vi.fn(async function* () { yield traversalPage(root, []) })
    const base = terminalBoundaryOutput()
    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog: {
        loadDirectory: async () => committedDirectory('root', 0, 1n),
        pages,
      } as unknown as V2CatalogClient,
      selection: new V2SelectionPolicy(),
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority({ ...base, admitDirectory }),
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(admitDirectory).not.toHaveBeenCalled()
    expect(pages).not.toHaveBeenCalled()
  })

  it('isolates child omission while leaving unmatched explicit targets unknown', async () => {
    const root = identity(2)
    const child = identity(3)
    const missing = fileEntry(identity(19), 'possibly-hidden.bin', 1n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(missing, [identityText(2)])
    const catalog = {
      loadDirectory: async (id: Uint8Array) => id[0] === child[0]
        ? committedDirectoryFor(child, identityText(3), 0, 1n)
        : committedDirectoryFor(root, identityText(2), 1),
      pages: async function* (directory: V2CommittedDirectory) {
        yield directory.directoryId[0] === root[0]
          ? traversalPage(root, [directoryEntry(child, identityText(3), 'hidden')])
          : traversalPage(child, [], { omittedCount: 1n })
      },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection,
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failures).toHaveLength(1)
    expect(result.outcome.failures[0]).toMatchObject({
      kind: 'directory',
      directoryId: identityText(3),
    })
    expect(result.outcome.failures.some(({ reason }) => reason instanceof V2SelectionTargetMissingError))
      .toBe(false)
    expect(result.measure.discovery).toBe('failed')
  })

  it.each(['admission', 'finalization'] as const)(
    'isolates one child directory output %s failure and completes its sibling',
    async (failurePoint) => {
      const root = identity(2)
      const bad = identity(3)
      const good = identity(4)
      const fixture = traversalOutput()
      const base = fixture.session
      const output: OutputSession = {
        ...base,
        capabilities: { ...base.capabilities, fileFailureIsolation: true },
        admitDirectory: async (directory, signal) => {
          if (failurePoint === 'admission' && directory.path.at(-1) === 'bad') {
            throw new OutputDirectoryMutationError('bad child admission failed', false)
          }
          return base.admitDirectory(directory, signal)
        },
        finalizeDirectory: async (directory, signal) => {
          if (failurePoint === 'finalization' && directory.path.at(-1) === 'bad') {
            throw new OutputDirectoryMutationError('bad child finalization failed', false)
          }
          return base.finalizeDirectory(directory, signal)
        },
      }
      const catalog = {
        loadDirectory: async (id: Uint8Array<ArrayBuffer>) => committedDirectoryFor(
          id,
          encodeBase64Url(id),
          id[0] === root[0] ? 2 : 0,
        ),
        pages: async function* (directory: V2CommittedDirectory) {
          if (directory.directoryId[0] === root[0]) {
            yield traversalPage(root, [
              directoryEntry(bad, identityText(3), 'bad'),
              directoryEntry(good, identityText(4), 'good'),
            ])
            return
          }
          yield traversalPage(directory.directoryId, [])
        },
      } as unknown as V2CatalogClient

      const result = await traversalJob(catalog, output, root, identityText(2)).run()

      expect(result.outcome.status).toBe('CompletedWithErrors')
      expect(result.outcome.failureCount).toBe(1)
      expect(fixture.finalizedPaths).toContainEqual(['good'])
      expect(fixture.finalizedPaths).not.toContainEqual(['bad'])
      expect(fixture.abortReasons).toEqual([])
    },
  )

  it('pauses and retains output when a corrupt cached root cycle is found', async () => {
    const rootId = identity(2)
    let loads = 0
    const catalog = {
      loadDirectory: async () => {
        loads += 1
        return committedDirectory('root', 1)
      },
      pages: async function* () {
        yield traversalPage(rootId, [directoryEntry(rootId, 'root', 'root-loop')])
      },
    } as unknown as V2CatalogClient
    const output = traversalOutput()

    const result = await traversalJob(catalog, output.session, rootId, 'root').run()

    expect(result.outcome.status).toBe('Paused')
    expect(loads).toBe(1)
    expect(output.suspendReasons).toHaveLength(1)
    expect(output.suspendReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
    expect(output.abortReasons).toEqual([])
    expect(result.abortReason).toBe(output.suspendReasons[0])
  })

  it('rejects repeated sibling identity even when neither file is selected', async () => {
    const rootId = identity(2)
    const repeatedFileId = identity(9)
    let revisionOpens = 0
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 2),
      pages: async function* () {
        yield traversalPage(rootId, [{
          kind: 'file',
          id: repeatedFileId,
          idText: 'repeated-file',
          name: 'first.bin',
          expectedSize: 1n,
        }, {
          kind: 'file',
          id: repeatedFileId.slice(),
          idText: 'repeated-file',
          name: 'second.bin',
          expectedSize: 1n,
        }])
      },
    } as unknown as V2CatalogClient
    const output = traversalOutput()
    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1),
        syntheticRoot: rootId,
        syntheticRootId: 'root',
      } as never,
      catalog,
      selection: new V2SelectionPolicy(false),
      revisions: {
        open: async () => {
          revisionOpens += 1
          throw new Error('Unselected duplicate reached revision I/O')
        },
      } as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output.session),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(revisionOpens).toBe(0)
    expect(output.suspendReasons[0]).toBeInstanceOf(V2CatalogTraversalError)
  })

  it('settles a selected opaque target that never appears as a visible failure', async () => {
    const root = identity(2)
    const missing = {
      kind: 'file' as const,
      id: identity(19),
      idText: identityText(19),
      name: 'missing.bin',
      expectedSize: 1n,
    }
    const selection = new V2SelectionPolicy(false)
    selection.toggle(missing, ['root'])
    const progress: TransferProgress[] = []
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 0),
      pages: async function* () { yield traversalPage(root, []) },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root' } as never,
      catalog,
      selection,
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
      onProgress: (value) => progress.push(value),
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(result.outcome.failures[0]).toMatchObject({ kind: 'file', fileId: missing.idText })
    expect(result.outcome.failures[0]?.reason).toBeInstanceOf(V2SelectionTargetMissingError)
    expect(result.measure.discovery).toBe('complete')
    expect(progress.at(-1)).toMatchObject({
      discovery: 'complete', fileErrors: 0, selectionErrors: 1, partial: true,
    })
  })

  it('keeps completed discovery distinct from a proven-missing directory target', async () => {
    const root = identity(2)
    const missing = directoryEntry(identity(19), identityText(19), 'missing-directory')
    const selection = new V2SelectionPolicy(false)
    selection.toggle(missing, ['root'])
    const progress: TransferProgress[] = []
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 0),
      pages: async function* () { yield traversalPage(root, []) },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root' } as never,
      catalog,
      selection,
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
      onProgress: (value) => progress.push(value),
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failures[0]).toMatchObject({ kind: 'directory', directoryId: missing.idText })
    expect(result.outcome.failures[0]?.reason).toBeInstanceOf(V2SelectionTargetMissingError)
    expect(result.measure.discovery).toBe('complete')
    expect(progress.at(-1)).toMatchObject({
      discovery: 'complete', failedDirectories: 0, selectionErrors: 1, partial: true,
    })
  })

  it('keeps an unmatched opaque target unknown when a child branch fails', async () => {
    const root = identity(2)
    const child = identity(3)
    const missing = {
      kind: 'file' as const,
      id: identity(19),
      idText: identityText(19),
      name: 'possibly-hidden.bin',
      expectedSize: 1n,
    }
    const selection = new V2SelectionPolicy(false)
    selection.toggle(missing, ['root'])
    const catalog = {
      loadDirectory: async (id: Uint8Array) => id[0] === child[0]
        ? committedDirectory('child', 0)
        : committedDirectory('root', 1),
      pages: async function* (directory: V2CommittedDirectory) {
        if (directory.directoryIdText === 'child') {
          throw new V2DirectoryTraversalError('Authenticated child generation could not be traversed')
        }
        yield traversalPage(root, [directoryEntry(child, 'child', 'unavailable')])
      },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root' } as never,
      catalog,
      selection,
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(result.outcome.failures[0]).toMatchObject({
      kind: 'directory',
      directoryId: identityText(3),
    })
    expect(result.outcome.failures.some(({ reason }) => reason instanceof V2SelectionTargetMissingError)).toBe(false)
    expect(result.measure.discovery).toBe('failed')
  })
})

describe('v2 catalog selection authority', () => {
  it('settles an explicit synthetic-root target from its authenticated generation', async () => {
    const root = identity(2)
    const rootText = identityText(2)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(directoryEntry(root, rootText, 'root'), [])
    const catalog = {
      loadDirectory: async () => committedDirectoryFor(root, rootText, 0),
      pages: async function* () { yield traversalPage(root, []) },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: rootText } as never,
      catalog,
      selection,
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.failures).toEqual([])
    expect(result.outcome.status).toBe('Succeeded')
    expect(result.outcome.failureCount).toBe(0)
  })

  it('isolates a child beyond the protocol depth boundary without loading it', async () => {
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

    expect(rejectedResult.outcome.status).toBe('CompletedWithErrors')
    expect(rejectedResult.outcome.failureCount).toBe(1)
    expect(rejected.loads()).toBe(V2_CATALOG_PATH_DEPTH + 1)
    expect(rejectedOutput.abortReasons).toEqual([])
  })

  it('isolates a file path beyond the depth boundary before revision or output I/O', async () => {
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

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(fixture.loads()).toBe(V2_CATALOG_PATH_DEPTH + 1)
    expect(revisionOpens).toBe(0)
    expect(output.abortReasons).toEqual([])
  })

  it('admits exactly 32 KiB of path bytes and isolates the next child before load', async () => {
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
    expect(overResult.outcome.status).toBe('CompletedWithErrors')
    expect(overResult.outcome.failureCount).toBe(1)
    expect(over.loads()).toBe(overSegments.length)
    expect(overOutput.abortReasons).toEqual([])
  })

  it('materializes only the authenticated ancestry of an opaque file target', async () => {
    const root = identity(2)
    const selectedDirectory = identity(3)
    const nestedDirectory = identity(4)
    const unrelatedDirectory = identity(5)
    const unrelatedLeaf = identity(6)
    const target = fileEntry(identity(7), 'target.bin', 1n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(target, [identityText(2), identityText(3), identityText(4)])
    const catalogs = new Map<number, readonly ReturnType<typeof directoryEntry | typeof fileEntry>[]>([
      [2, [
        directoryEntry(selectedDirectory, identityText(3), 'selected'),
        directoryEntry(unrelatedDirectory, identityText(5), 'unrelated'),
      ]],
      [3, [directoryEntry(nestedDirectory, identityText(4), 'nested')]],
      [4, [target]],
      [5, [directoryEntry(unrelatedLeaf, identityText(6), 'leaf')]],
      [6, []],
    ])
    const loaded: number[] = []
    const catalog = {
      loadDirectory: async (id: Uint8Array<ArrayBuffer>) => {
        loaded.push(id[0] ?? -1)
        return committedDirectoryFor(id, encodeBase64Url(id), catalogs.get(id[0] ?? -1)?.length ?? 0)
      },
      pages: async function* (directory: V2CommittedDirectory) {
        yield traversalPage(directory.directoryId, catalogs.get(directory.directoryId[0] ?? -1) ?? [])
      },
    } as unknown as V2CatalogClient
    const base = terminalBoundaryOutput()
    const admittedPaths: string[][] = []
    const finalizedPaths: string[][] = []
    const output: OutputSession = {
      ...base,
      admitDirectory: async (request, signal) => {
        admittedPaths.push([...request.path])
        return base.admitDirectory(request, signal)
      },
      finalizeDirectory: async (directory, signal) => {
        finalizedPaths.push([...directory.path])
        return base.finalizeDirectory(directory, signal)
      },
    }

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection,
      revisions: { open: async () => openedRevision(target.id, 1n, 1n) } as V2RevisionReader,
      broker: {
        readRange: async function* () { yield { offset: 0n, data: Uint8Array.of(1) } },
      } as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentDirectories: 2,
    }).run()

    expect(result.outcome.failures).toEqual([])
    expect(result.outcome.status).toBe('Succeeded')
    expect(loaded.sort()).toEqual([2, 3, 4, 5])
    expect(admittedPaths).toEqual(expect.arrayContaining([[], ['selected'], ['selected', 'nested']]))
    expect(admittedPaths).not.toContainEqual(['unrelated'])
    expect(admittedPaths).not.toContainEqual(['unrelated', 'leaf'])
    expect(finalizedPaths).toEqual(expect.arrayContaining([['selected'], ['selected', 'nested']]))
    expect(finalizedPaths).not.toContainEqual(['unrelated'])
  })

  it('stops opaque search after the target resolves and ignores a later sibling failure', async () => {
    const root = identity(2)
    const selectedDirectory = identity(3)
    const delayedDirectory = identity(4)
    const target = fileEntry(identity(7), 'target.bin', 1n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(target, [identityText(2), identityText(3)])
    let releaseDelayed: (() => void) | undefined
    const delayed = new Promise<void>((resolve) => { releaseDelayed = resolve })
    let signalFileStarted: (() => void) | undefined
    const fileStarted = new Promise<void>((resolve) => { signalFileStarted = resolve })
    const catalog = {
      loadDirectory: async (id: Uint8Array<ArrayBuffer>) => {
        if (id[0] === delayedDirectory[0]) {
          await delayed
          throw new V2CatalogTraversalError('unrelated branch exhausted its node budget')
        }
        let count = 0
        if (id[0] === root[0]) count = 2
        else if (id[0] === selectedDirectory[0]) count = 1
        return committedDirectoryFor(id, encodeBase64Url(id), count)
      },
      pages: async function* (directory: V2CommittedDirectory) {
        if (directory.directoryId[0] === root[0]) {
          yield traversalPage(directory.directoryId, [
            directoryEntry(selectedDirectory, identityText(3), 'selected'),
            directoryEntry(delayedDirectory, identityText(4), 'z-delayed'),
          ])
          return
        }
        yield traversalPage(
          directory.directoryId,
          directory.directoryId[0] === selectedDirectory[0] ? [target] : [],
        )
      },
    } as unknown as V2CatalogClient
    const base = terminalBoundaryOutput()
    const admittedPaths: string[][] = []
    const output: OutputSession = {
      ...base,
      admitDirectory: async (request, signal) => {
        admittedPaths.push([...request.path])
        return base.admitDirectory(request, signal)
      },
      beginFile: async (file, signal) => {
        signalFileStarted?.()
        return base.beginFile(file, signal)
      },
    }
    const running = new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection,
      revisions: { open: async () => openedRevision(target.id, 1n, 1n) } as V2RevisionReader,
      broker: {
        readRange: async function* () { yield { offset: 0n, data: Uint8Array.of(1) } },
      } as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentDirectories: 2,
    }).run()

    await withTimeout(fileStarted, 1_000, 'selected file waited for unrelated discovery')
    expect(admittedPaths).toContainEqual(['selected'])
    expect(admittedPaths).not.toContainEqual(['z-delayed'])
    releaseDelayed?.()
    const result = await running
    expect(result.outcome.status).toBe('Succeeded')
    expect(result.outcome.failures).toEqual([])
    expect(admittedPaths).not.toContainEqual(['z-delayed'])
  })

  it('measures every selected file in a terminal generation before child admission can fail', async () => {
    const root = identity(2)
    const child = identity(3)
    const first = fileEntry(identity(7), 'first.bin', 2n)
    const second = fileEntry(identity(8), 'second.bin', 3n)
    const catalog = {
      loadDirectory: async (id: Uint8Array<ArrayBuffer>) => committedDirectoryFor(
        id,
        encodeBase64Url(id),
        id[0] === root[0] ? 1 : 2,
      ),
      pages: async function* (directory: V2CommittedDirectory) {
        yield traversalPage(
          directory.directoryId,
          directory.directoryId[0] === root[0]
            ? [directoryEntry(child, identityText(3), 'child')]
            : [first, second],
        )
      },
    } as unknown as V2CatalogClient
    const base = terminalBoundaryOutput()
    const openRevision = vi.fn()
    const output: OutputSession = {
      ...base,
      admitDirectory: async (request, signal) => {
        if (request.path.at(-1) === 'child') {
          throw new OutputDirectoryMutationError('child admission failed', false)
        }
        return base.admitDirectory(request, signal)
      },
    }

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection: new V2SelectionPolicy(true),
      revisions: { open: openRevision } as unknown as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentDirectories: 1,
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.measure).toMatchObject({ discoveredFiles: 2, discoveredBytes: 5n, discovery: 'failed' })
    expect(result.outcome.failures[0]).toMatchObject({ kind: 'directory', directoryId: identityText(3) })
    expect(openRevision).not.toHaveBeenCalled()
  })
})
