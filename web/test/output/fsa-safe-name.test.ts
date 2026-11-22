import { describe, expect, it } from 'vitest'

import {
  deterministicSafeComponent,
  isSafeComponentName,
  SafeNameRegistry,
} from '../../src/output/file-system-access/safe-name'
import { isPortableOutputName } from '../../src/output/names/policy'
import {
  reopenFileSystemAccessOutput,
  reserveNewFileSystemAccessOutput,
} from '../../src/output/file-system-access/session'
import {
  discardReopenedFileSystemAccessOutput,
  type ReopenedFileSystemAccessDiscardOperation,
} from '../../src/output/file-system-access/fresh-page-discard'
import { verifyFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import { receiveOperationLeaseRecord } from '../../src/output/workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { createDirectTreeCoordinateContract } from '../../src/transfer/job/coordinate/direct-tree'
import { MAX_SAFE_RENAMED_SAMPLE } from '../../src/transfer/lifecycle/safe-names'
import { acquireFSARootMutationLease, memoryFSAIdentities } from './fsa-mutation-lock-fixture'
import { identity } from './planning/fixture'
import {
  MemoryDirectory,
  MemoryFile,
  MemoryLockManager,
  MemoryOperationRepository,
  PAUSED,
  SIGNAL,
  bindTask,
  fsaExecution,
  memoryCheckpointFactory,
  outputFileRequest,
  resultRootArtifact,
  rootDirectoryMaterializationRequest,
  singleFileArtifact,
  startReceiving,
} from './file-system-access-lifecycle-fixture'

describe('deterministic FSA safe component naming', () => {
  it.each([
    'CON',
    'CON.txt',
    'NUL',
    'COM¹',
    'LPT1.bin',
    'pyvenv.cfg',
    'shortcut.lnk',
    'site.url',
    'app.local',
    '???',
    '...',
    '   ',
    '界'.repeat(100) + '.txt',
  ])('derives a deterministic portable safe name for %j without keeping blocked suffixes', async name => {
    const first = await deterministicSafeComponent({ logicalPath: ['dir', name], entryKind: 'file' })
    const second = await deterministicSafeComponent({ logicalPath: ['dir', name], entryKind: 'file' })

    expect(first).toBe(second)
    expect(isPortableOutputName(first)).toBe(true)
    expect(isSafeComponentName(first)).toBe(true)
    expect(first).not.toBe(name)
    expect(first).not.toMatch(/\.(cfg|lnk|url|local)$/iu)
  })

  it('differentiates entry kind, logical path, and collision index', async () => {
    const fileA = await deterministicSafeComponent({ logicalPath: ['a', 'CON'], entryKind: 'file', collisionIndex: 0 })
    const fileACollision = await deterministicSafeComponent({ logicalPath: ['a', 'CON'], entryKind: 'file', collisionIndex: 1 })
    const dirA = await deterministicSafeComponent({ logicalPath: ['a', 'CON'], entryKind: 'directory', collisionIndex: 0 })
    const fileB = await deterministicSafeComponent({ logicalPath: ['b', 'CON'], entryKind: 'file', collisionIndex: 0 })

    expect(new Set([fileA, fileACollision, dirA, fileB]).size).toBe(4)
  })

  it('bounds SafeNameRegistry samples while keeping exact total count', () => {
    const registry = new SafeNameRegistry()
    expect(registry.summary()).toBeUndefined()
    for (let index = 0; index < MAX_SAFE_RENAMED_SAMPLE + 4; index += 1) {
      registry.record(
        ['dir', `file-${index}.cfg`],
        'file',
        `file-${index}-cfg.windshare-0000000${index.toString(16)}`,
      )
    }
    // Re-recording the same logical path is idempotent.
    registry.record(['dir', 'file-0.cfg'], 'file', 'file-0-cfg.windshare-00000000')

    const summary = registry.summary()
    expect(summary?.count).toBe(MAX_SAFE_RENAMED_SAMPLE + 4)
    expect(summary?.sample).toHaveLength(MAX_SAFE_RENAMED_SAMPLE)
  })
})

describe('FSA safe-name end-to-end lifecycle, reopen, and fresh-page discard', () => {
  it('advances safe root collisionIndex when the first safe name is already occupied', async () => {
    const parent = new MemoryDirectory('downloads')
    const firstSafe = await deterministicSafeComponent({
      logicalPath: ['pyvenv.cfg'],
      entryKind: 'file',
      collisionIndex: 0,
    })
    await parent.getFileHandle(firstSafe, { create: true })
    parent.onEntryLookup = lookup => {
      if (lookup.name.endsWith('.cfg') && !lookup.create) throw new TypeError('blocked suffix')
    }
    const locks = new MemoryLockManager()
    const rootLease = await acquireFSARootMutationLease(parent as unknown as FileSystemDirectoryHandle, locks)
    const offer = fsaParentOffer()
    try {
      const reserved = await reserveNewFileSystemAccessOutput({
        authority: {
          kind: 'fsa-parent-directory-authority',
          targetRouteId: offer.routeId,
          offer,
          parent: parent as unknown as FileSystemDirectoryHandle,
        },
        artifact: await singleFileArtifact('pyvenv.cfg'),
        rootLease,
        operationId: identity(10),
        reservationId: identity(11),
        authorityRef: identity(12, 32),
      })

      expect(reserved.reservation.logicalReservedName).toMatch(/^pyvenv-.*\.cfg$/u)
      expect(reserved.reservation.collisionIndex).toBe(1)
      expect(reserved.reservation.physicalName).not.toBe(firstSafe)
      expect(isSafeComponentName(reserved.reservation.physicalName)).toBe(true)
    } finally {
      await rootLease.release()
    }
  })

  it('handles SecurityError on directory creation (.git/hooks) and reopens child files via persisted handle names', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 20,
    })
    const root = await parent.getDirectoryHandle(session.reservation.physicalName) as unknown as MemoryDirectory
    await session.ensureDirectory(['.git'])
    const gitDir = await root.getDirectoryHandle('.git') as unknown as MemoryDirectory
    gitDir.onEntryLookup = lookup => {
      if (lookup.kind === 'directory' && lookup.name === 'hooks' && lookup.create) {
        throw new DOMException('Chromium blocks .git/hooks', 'SecurityError')
      }
    }

    const hooksDir = await session.ensureDirectory(['.git', 'hooks'])
    const summary = session.safeRenamedSummary()
    expect(summary?.count).toBe(1)
    expect(summary?.sample[0]?.logicalPath).toEqual(['.git', 'hooks'])
    const hooksPhysical = summary!.sample[0]!.physicalName
    expect(isSafeComponentName(hooksPhysical)).toBe(true)
    expect(gitDir.directoryNames()).toEqual([hooksPhysical])

    const hookFile = await session.beginFile({
      materializationRelativePath: ['.git', 'hooks', 'pre-commit'],
      openRevision: async () => ({ fileId: identity(31), fileRevision: identity(32), exactSize: 3n }),
    })
    await hookFile.writeRange(0n, Uint8Array.of(1, 2, 3))
    await hookFile.commit()
    const savedHooks = await gitDir.getDirectoryHandle(hooksPhysical) as unknown as MemoryDirectory
    expect(await (await savedHooks.getFileHandle('pre-commit') as unknown as MemoryFile).bytes())
      .toEqual(Uint8Array.of(1, 2, 3))
    await session.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent: session.intent,
      operationRepository: repository,
      lockManager: locks,
      mutationIdentities: memoryFSAIdentities(locks),
      checkpointRepositoryFactory: checkpointFactory,
    })
    expect((await reopened.ensureDirectory(['.git', 'hooks'])).ownedObjectId).toBe(hooksDir.ownedObjectId)
    const reopenedFile = await reopened.beginFile({
      materializationRelativePath: ['.git', 'hooks', 'pre-commit'],
      openRevision: async () => ({ fileId: identity(31), fileRevision: identity(32), exactSize: 3n }),
    })
    expect(reopenedFile.ownedObjectId).toBe(hookFile.ownedObjectId)
    await reopenedFile.close()
    await reopened.close()
  })

  it.each([false, true])('fresh-page discard handles safe-renamed dirs and files (keepCompleted=%s)', async keepCompleted => {
    const seed = keepCompleted ? 40 : 60
    const parent = new MemoryDirectory(`downloads-${seed}`)
    const primaryRepository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    const checkpointFactory = memoryCheckpointFactory()
    const session = await bindTask({
      parent,
      repository: primaryRepository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: seed,
    })
    const root = await parent.getDirectoryHandle(session.reservation.physicalName) as unknown as MemoryDirectory
    root.onEntryLookup = lookup => {
      if (lookup.name === 'bad?' && !lookup.create) throw new TypeError('blocked dir')
    }
    const initialLeaseId = identity(seed + 3)
    await startReceiving(primaryRepository, session.intent, initialLeaseId)
    const execution = await fsaExecution(
      session,
      primaryRepository,
      initialLeaseId,
      identity(seed + 4),
      identity(seed + 5),
    )
    const rootAdmission = await execution.directories.admitDirectory(
      await rootDirectoryMaterializationRequest(session.intent, identity(seed + 6)),
      SIGNAL,
    )
    const contract = await createDirectTreeCoordinateContract(session.intent)
    const badProjection = contract.projectDirectory(['photos', 'bad?'])
    if (badProjection.kind !== 'materialize') throw new TypeError('expected materialize projection')
    const badAdmission = await execution.directories.admitDirectory({
      directory: Object.freeze({
        directoryId: identity(seed + 12),
        generation: identity(seed + 13),
        path: badProjection.relativePath,
        parentAdmission: rootAdmission,
      }),
      sourceAuthenticationPath: badProjection.sourceAuthenticationPath,
      logicalArtifactPath: badProjection.logicalArtifactPath,
    }, SIGNAL)
    const badPhysical = session.safeRenamedSummary()!.sample[0]!.physicalName
    const badDir = await root.getDirectoryHandle(badPhysical) as unknown as MemoryDirectory
    badDir.onEntryLookup = lookup => {
      if (lookup.name === 'pyvenv.cfg' && !lookup.create) throw new TypeError('blocked file')
    }

    if (keepCompleted) {
      const kept = await execution.output.beginFile(await outputFileRequest({
        intent: session.intent,
        fileId: identity(seed + 7),
        fileRevision: identity(seed + 8),
        sourceRelativePath: ['bad?', 'kept.bin'],
        exactSize: 2n,
        parentAdmission: badAdmission,
      }), SIGNAL)
      await kept.transaction.writeRange(0n, Uint8Array.of(9, 8), SIGNAL)
      await kept.transaction.commit(SIGNAL)
    }

    const unfinished = await execution.output.beginFile(await outputFileRequest({
      intent: session.intent,
      fileId: identity(seed + 9),
      fileRevision: identity(seed + 10),
      sourceRelativePath: ['bad?', 'pyvenv.cfg'],
      exactSize: 4n,
      parentAdmission: badAdmission,
    }), SIGNAL)
    await unfinished.transaction.writeRange(0n, Uint8Array.of(1, 2), SIGNAL)
    await unfinished.transaction.pause('discard-test')
    await execution.directories.finalizeDirectory(badAdmission, SIGNAL)
    await execution.directories.finalizeDirectory(rootAdmission, SIGNAL)
    await execution.pause({
      worker: PAUSED,
      materialization: keepCompleted
        ? { entryCount: 2n, fileCount: 1n, directoryCount: 1n, rawBytes: 2n }
        : { entryCount: 1n, fileCount: 0n, directoryCount: 1n, rawBytes: 0n },
      selectionFacts: Object.freeze({
        discoveredFileCount: keepCompleted ? 2n : 1n,
        discoveredBytes: keepCompleted ? 6n : 4n,
        discovery: 'complete' as const,
      }),
      reason: new DOMException('page closed', 'AbortError'),
    }, SIGNAL)
    await session.releaseRootLease()

    const repository = new MemoryOperationRepository({
      records: primaryRepository.records,
      handles: primaryRepository.handles,
      leases: primaryRepository.leases,
    })
    const lifecycle = decodeStoredReceiveLifecycleState(
      (await repository.readLifecycle(session.intent.operationId))!,
    )
    const leaseId = identity(seed + 11)
    await repository.commitTransition({
      operationId: session.intent.operationId,
      expectedLifecycleGeneration: lifecycle.generation,
      expectedLeaseId: initialLeaseId,
      lease: {
        kind: 'put',
        record: receiveOperationLeaseRecord({
          operationId: session.intent.operationId,
          leaseId,
          acquiredAt: 2_000,
        }),
      },
    })
    const binding = await verifyFSAOperationBinding({ repository, intent: session.intent })
    const operation: ReopenedFileSystemAccessDiscardOperation = Object.freeze({
      kind: 'direct-tree',
      intent: session.intent,
      lifecycle,
      binding,
      lease: Object.freeze({ operationId: session.intent.operationId, leaseId }),
      repository,
      close: async () => {
        await repository.commitTransition({
          operationId: session.intent.operationId,
          expectedLeaseId: leaseId,
          lease: { kind: 'delete', leaseId },
        })
        repository.close()
      },
    })

    const result = await discardReopenedFileSystemAccessOutput({
      operation,
      lockManager: locks,
      mutationIdentities: memoryFSAIdentities(locks),
      checkpointRepositoryFactory: checkpointFactory,
    })

    if (keepCompleted) {
      expect(result.lifecycle).toMatchObject({ kind: 'partial-directory', reason: 'stopped', successCount: 1n })
      expect(root.directoryNames()).toEqual([badPhysical])
      expect(badDir.fileNames()).toEqual(['kept.bin'])
    } else {
      expect(result.lifecycle).toMatchObject({ kind: 'discarded' })
      expect(parent.entryNames()).toEqual([])
    }
  })
})
