import { describe, expect, it } from 'vitest'

import { directoryId } from '../../src/catalog/model'
import {
  inspectFileSystemComponent,
  PathComponentRejectedError,
  type FileSystemComponentKind,
  type FileSystemComponentInspectionStage,
} from '../../src/output/browser/filesystem-component-inspection'
import {
  reopenFileSystemAccessOutput,
  type FSAOutputTraceEvent,
} from '../../src/output/file-system-access/session'
import {
  createFileSystemAccessSettlementAuthority,
  type FSASettlementTraceEvent,
} from '../../src/output/file-system-access/settlement'
import {
  DestinationCollisionError,
  TargetOwnershipUnknownError,
} from '../../src/output/persistent-tree/errors'
import {
  RECEIVE_RECORD_CLEANUP,
  RECEIVE_RECORD_RECEIPT,
} from '../../src/output/workspace/records'
import { classificationForTransferFailure } from '../../src/transfer/job/failures'
import {
  EMPTY_TRANSFER_FILE_OUTCOME_COUNTS,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { identity } from './planning/fixture'
import {
  MemoryDirectory,
  MemoryLockManager,
  MemoryOperationRepository,
  PAUSED,
  SIGNAL,
  SUCCESS,
  absentCompatibleNameLedgerFactory,
  bindTask,
  discardFreshFixture,
  freshDiscardFixture,
  fsaExecution,
  installIntentFrozen,
  memoryCheckpointFactory,
  outputFileRequest,
  persistFreshFixtureExpiry,
  resultRootArtifact,
  resumeReceiving,
  singleFileArtifact,
  startReceiving,
} from './file-system-access-lifecycle-fixture'

describe('native FSA component inspection boundary', () => {
  const rejectionCases: readonly Readonly<{
    expectedKind: FileSystemComponentKind
    stage: FileSystemComponentInspectionStage
  }>[] = [
    { expectedKind: 'file', stage: 'fsa.file.entry.inspect' },
    { expectedKind: 'directory', stage: 'fsa.directory.entry.inspect' },
    { expectedKind: 'directory', stage: 'fsa.root.entry.inspect' },
  ]

  it.each(rejectionCases)(
    'classifies only the awaited $stage expected-kind TypeError before mutation',
    async ({ expectedKind, stage }) => {
      const parent = new MemoryDirectory('downloads')
      const cause = new TypeError('opaque native refusal')
      const lookups: string[] = []
      parent.onEntryLookup = lookup => {
        lookups.push(`${lookup.kind}:${lookup.name}:${lookup.create}`)
        if (lookup.kind === expectedKind) throw cause
      }

      const rejected = inspectFileSystemComponent({
        verifiedParent: parent as unknown as FileSystemDirectoryHandle,
        component: 'portable-name.bin',
        expectedKind,
        stage,
        mode: 'classify-rejection',
      })

      await expect(rejected).rejects.toMatchObject({
        name: 'PathComponentRejectedError',
        cause,
        canonicalComponent: 'portable-name.bin',
        expectedKind,
        stage,
        preMutation: true,
      })
      await expect(rejected).rejects.toBeInstanceOf(PathComponentRejectedError)
      expect(lookups).toEqual([`${expectedKind}:portable-name.bin:false`])
      expect(parent.entryNames()).toEqual([])
    },
  )

  it('validates one canonical component before touching the native parent', async () => {
    const parent = new MemoryDirectory('downloads')
    const lookups: string[] = []
    parent.onEntryLookup = lookup => { lookups.push(lookup.name) }

    const rejected = inspectFileSystemComponent({
      verifiedParent: parent as unknown as FileSystemDirectoryHandle,
      component: 'nested/name.bin',
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'classify-rejection',
    })

    await expect(rejected).rejects.toBeInstanceOf(TypeError)
    await expect(rejected).rejects.not.toBeInstanceOf(PathComponentRejectedError)
    expect(lookups).toEqual([])
  })

  it('probes the expected kind first and reports both entry kinds as occupied', async () => {
    const fileParent = new MemoryDirectory('downloads')
    await fileParent.getDirectoryHandle('existing', { create: true })
    const fileLookups: string[] = []
    fileParent.onEntryLookup = lookup => { fileLookups.push(lookup.kind) }

    await expect(inspectFileSystemComponent({
      verifiedParent: fileParent as unknown as FileSystemDirectoryHandle,
      component: 'existing',
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'classify-rejection',
    })).resolves.toBe('occupied')
    expect(fileLookups).toEqual(['file'])

    const directoryParent = new MemoryDirectory('downloads')
    await directoryParent.getFileHandle('existing', { create: true })
    const directoryLookups: string[] = []
    directoryParent.onEntryLookup = lookup => { directoryLookups.push(lookup.kind) }

    await expect(inspectFileSystemComponent({
      verifiedParent: directoryParent as unknown as FileSystemDirectoryHandle,
      component: 'existing',
      expectedKind: 'directory',
      stage: 'fsa.directory.entry.inspect',
      mode: 'classify-rejection',
    })).resolves.toBe('occupied')
    expect(directoryLookups).toEqual(['directory'])
  })

  it('uses an opposite-kind lookup only after expected-kind NotFoundError', async () => {
    const parent = new MemoryDirectory('downloads')
    const lookups: string[] = []
    parent.onEntryLookup = lookup => { lookups.push(lookup.kind) }

    await expect(inspectFileSystemComponent({
      verifiedParent: parent as unknown as FileSystemDirectoryHandle,
      component: 'absent.bin',
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'classify-rejection',
    })).resolves.toBe('absent')
    expect(lookups).toEqual(['file', 'directory'])
  })

  it.each(['present', 'type-mismatch'] as const)(
    'treats an opposite-kind %s result as occupied after expected-kind NotFoundError',
    async oppositeResult => {
      const parent = new MemoryDirectory('downloads')
      if (oppositeResult === 'present') {
        await parent.getDirectoryHandle('occupied', { create: true })
      }
      const lookups: string[] = []
      parent.onEntryLookup = lookup => {
        lookups.push(lookup.kind)
        if (lookup.kind === 'file') throw new DOMException('missing', 'NotFoundError')
        if (oppositeResult === 'type-mismatch') {
          throw new DOMException('different entry kind', 'TypeMismatchError')
        }
      }

      await expect(inspectFileSystemComponent({
        verifiedParent: parent as unknown as FileSystemDirectoryHandle,
        component: 'occupied',
        expectedKind: 'file',
        stage: 'fsa.file.entry.inspect',
        mode: 'classify-rejection',
      })).resolves.toBe('occupied')
      expect(lookups).toEqual(['file', 'directory'])
    },
  )

  it('leaves expected-kind non-TypeError and every secondary failure unwrapped', async () => {
    const expectedFailure = new DOMException('permission denied', 'NotAllowedError')
    const expectedParent = new MemoryDirectory('downloads')
    expectedParent.onEntryLookup = () => { throw expectedFailure }

    await expect(inspectFileSystemComponent({
      verifiedParent: expectedParent as unknown as FileSystemDirectoryHandle,
      component: 'report.bin',
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'classify-rejection',
    })).rejects.toBe(expectedFailure)

    const secondaryFailure = new TypeError('opposite-kind lookup failed')
    const secondaryParent = new MemoryDirectory('downloads')
    const lookups: string[] = []
    secondaryParent.onEntryLookup = lookup => {
      lookups.push(lookup.kind)
      if (lookup.kind === 'directory') throw secondaryFailure
    }

    await expect(inspectFileSystemComponent({
      verifiedParent: secondaryParent as unknown as FileSystemDirectoryHandle,
      component: 'report.bin',
      expectedKind: 'file',
      stage: 'fsa.file.entry.inspect',
      mode: 'classify-rejection',
    })).rejects.toBe(secondaryFailure)
    expect(lookups).toEqual(['file', 'directory'])
  })

  it('keeps diagnostic call ordering without manufacturing a rejection trigger', async () => {
    const cause = new TypeError('diagnostic native refusal')
    const parent = new MemoryDirectory('downloads')
    const lookups: string[] = []
    parent.onEntryLookup = lookup => {
      lookups.push(lookup.kind)
      throw cause
    }

    await expect(inspectFileSystemComponent({
      verifiedParent: parent as unknown as FileSystemDirectoryHandle,
      component: 'report.bin',
      expectedKind: 'directory',
      stage: 'fsa.directory.entry.inspect',
      mode: 'diagnostic',
    })).rejects.toBe(cause)
    expect(lookups).toEqual(['directory'])
  })
})

describe('File System Access DirectTree lifecycle', () => {
  it.each([
    { label: 'single-file', artifact: singleFileArtifact, expected: ['file:report.bin', 'directory:report.bin'] },
    { label: 'result-root', artifact: resultRootArtifact, expected: ['directory:photos', 'file:photos'] },
  ])('reserves an ordinary $label root by expected kind without activating repair', async ({
    artifact,
    expected,
  }) => {
    const parent = new MemoryDirectory('downloads')
    const lookups: string[] = []
    let repairFactoryCalls = 0
    parent.onEntryLookup = (lookup) => {
      if (!lookup.create) lookups.push(`${lookup.kind}:${lookup.name}`)
    }
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await artifact(),
      operationSeed: 9,
      activate: false,
      prepareCompatibleNameRootRepair: async () => {
        repairFactoryCalls += 1
        throw new Error('ordinary reservation must not activate repair')
      },
    })

    expect(lookups).toEqual(expected)
    expect(repairFactoryCalls).toBe(0)
    expect(session.reservation.logicalReservedName).toBe(session.reservation.physicalName)
    expect(parent.entryNames()).toEqual([])
    await session.close()
  })

  it('persists a collision suffix, creates one task root, and reopens it after restart', async () => {
    const parent = new MemoryDirectory('downloads')
    const unrelated = await parent.getDirectoryHandle('photos', { create: true })
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const artifact = await resultRootArtifact()
    const first = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 10,
    })

    expect(first.reservation.collisionIndex).toBe(1)
    expect(first.reservation.logicalReservedName).not.toBe('photos')
    const taskRoot = await parent.getDirectoryHandle(first.reservation.physicalName)
    expect(await parent.getDirectoryHandle('photos')).toBe(unrelated)
    expect(parent.directoryNames()).toEqual(['photos', first.reservation.physicalName].sort())
    await first.ensureDirectory(['nested'])
    const firstFile = await first.beginFile({
      artifactPath: ['nested', 'photo.bin'],
      openRevision: async () => ({
        fileId: identity(80),
        fileRevision: identity(81),
        exactSize: 2n,
      }),
    })
    await firstFile.writeRange(0n, Uint8Array.of(7, 8))
    await firstFile.commit()
    const nested = await taskRoot.getDirectoryHandle('nested')
    const visibleFile = await (await nested.getFileHandle('photo.bin')).getFile()
    expect(new Uint8Array(await visibleFile.arrayBuffer())).toEqual(Uint8Array.of(7, 8))
    const frozenIntent = first.intent
    await first.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent: frozenIntent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
    })
    expect(reopened.reservation.logicalReservedName).toBe(first.reservation.logicalReservedName)
    expect(await parent.getDirectoryHandle(reopened.reservation.physicalName)).toBe(taskRoot)
    await expect(reopened.ensureDirectory(['nested'])).resolves.toMatchObject({ created: false })
    const reopenedFile = await reopened.beginFile({
      artifactPath: ['nested', 'photo.bin'],
      openRevision: async () => ({
        fileId: identity(80),
        fileRevision: identity(81),
        exactSize: 2n,
      }),
    })
    expect(reopenedFile.ownedObjectId).toBe(firstFile.ownedObjectId)
    expect(parent.directoryNames()).toHaveLength(2)
    await reopened.close()

    const second = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 20,
    })
    expect(second.reservation.logicalReservedName).not.toBe(first.reservation.logicalReservedName)
    expect(parent.directoryNames()).toHaveLength(3)
    await second.close()
  })

  it('uses single-file DirectoryTree directly below the parent and creates after revision open', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const artifact = await singleFileArtifact()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact,
      operationSeed: 30,
    })

    expect(parent.entryNames()).toEqual([])
    const ordering: string[] = []
    parent.onFileCreated = () => ordering.push('created')
    const transaction = await session.beginFile({
      artifactPath: [session.reservation.requestedName],
      openRevision: async () => {
        ordering.push('revision-opened')
        return {
          fileId: identity(3),
          fileRevision: identity(33),
          exactSize: 4n,
        }
      },
    })
    expect(ordering).toEqual(['revision-opened', 'created'])
    expect(parent.directoryNames()).toEqual([])
    expect(parent.fileNames()).toEqual([session.reservation.physicalName])

    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    expect(await parent.fileBytes(session.reservation.physicalName)).toEqual(Uint8Array.of(1, 2))
    await transaction.checkpoint()
    await transaction.writeRange(2n, Uint8Array.of(3, 4))
    await transaction.commit()
    const intent = session.intent
    await session.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
    })
    const resumed = await reopened.beginFile({
      artifactPath: [reopened.reservation.requestedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(33),
        exactSize: 4n,
      }),
    })
    expect(resumed.ownedObjectId).toBe(transaction.ownedObjectId)
    expect(parent.entryNames()).toEqual([session.reservation.physicalName])
    await reopened.close()
  })

  it('keeps a nonzero collision reservation out of the transfer artifact path', async () => {
    const parent = new MemoryDirectory('downloads')
    await parent.getFileHandle('report.bin', { create: true })
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 34,
    })

    expect(session.reservation).toMatchObject({
      collisionIndex: 1,
      requestedName: 'report.bin',
    })
    expect(session.reservation.logicalReservedName).not.toBe(session.reservation.requestedName)
    expect(session.reservation.physicalName).toBe(session.reservation.logicalReservedName)

    const transaction = await session.beginFile({
      artifactPath: [session.reservation.requestedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(34),
        exactSize: 1n,
      }),
    })
    await transaction.writeRange(0n, Uint8Array.of(7))
    await transaction.commit()
    expect(parent.fileNames()).toEqual([
      session.reservation.physicalName,
      session.reservation.requestedName,
    ].sort())
    await session.close()
  })

  it('reports a pre-existing file as a collision only while its lineage is absent', async () => {
    const parent = new MemoryDirectory('downloads')
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 35,
    })
    await parent.getFileHandle(session.reservation.physicalName, { create: true })

    await expect(session.beginFile({
      artifactPath: [session.reservation.requestedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(33),
        exactSize: 4n,
      }),
    })).rejects.toBeInstanceOf(DestinationCollisionError)
    expect(parent.fileNames()).toEqual([session.reservation.physicalName])
    await session.close()
  })

  it('turns an external task-root race into NeedsAttention without merging', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const artifact = await resultRootArtifact()
    const trace: FSAOutputTraceEvent[] = []
    let external: FileSystemDirectoryHandle | undefined
    repository.afterFirstCommit = async () => {
      external = await parent.getDirectoryHandle('photos', { create: true })
    }

    await expect(bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact,
      operationSeed: 40,
      trace: (event) => trace.push(event),
    })).rejects.toBeInstanceOf(TargetOwnershipUnknownError)
    expect(await parent.getDirectoryHandle('photos')).toBe(external)
    expect(parent.directoryNames()).toEqual(['photos'])
    expect(trace.at(-1)).toMatchObject({
      name: 'receive.operation.needs_attention',
      needs_attention_reason: 'target-ownership-unknown',
    })
  })

  it('keeps a new result root dormant until bound execution activation', async () => {
    const parent = new MemoryDirectory('downloads')
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 45,
      activate: false,
    })

    expect(parent.entryNames()).toEqual([])
    expect(() => session.ensureDirectory([])).toThrow(/not activated/u)
    await session.activate()
    expect(parent.directoryNames()).toEqual([session.reservation.physicalName])
    await session.close()
  })

  it('rechecks the file identity before writer acquisition and preserves the replacement', async () => {
    const parent = new MemoryDirectory('downloads')
    const trace: FSAOutputTraceEvent[] = []
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 50,
      trace: (event) => trace.push(event),
    })
    const transaction = await session.beginFile({
      artifactPath: [session.reservation.requestedName],
      openRevision: async () => ({
        fileId: identity(3),
        fileRevision: identity(34),
        exactSize: 1n,
      }),
    })
    const replacement = parent.replaceFile(session.reservation.physicalName, Uint8Array.of(9))
    await expect(transaction.writeRange(0n, Uint8Array.of(1))).rejects.toBeInstanceOf(
      TargetOwnershipUnknownError,
    )
    expect(await replacement.bytes()).toEqual(Uint8Array.of(9))
    expect(trace.at(-1)).toMatchObject({
      name: 'receive.operation.needs_attention',
      needs_attention_reason: 'target-ownership-unknown',
    })
    await session.close().catch(() => undefined)
  })
})


describe('File System Access settlement authority', () => {
  it('publishes only after final directory and checkpoint ownership observation', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    let retired = 0
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(() => { retired += 1 }),
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 60,
    })
    const leaseId = identity(61)
    const transferJobId = identity(62)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(63))
    const root = await execution.directories.admitDirectory({
      source: {
        directoryId: directoryId(session.intent.syntheticRoot),
        generation: identity(64),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(65),
      fileRevision: identity(66),
      artifactPath: ['payload.bin'],
      exactSize: 2n,
      parentAdmission: root,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(4, 5), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    await execution.directories.finalizeDirectory(root, SIGNAL)

    await expect(execution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 2n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'published' })
    expect(retired).toBe(1)
    expect(locks.releaseCount).toBe(1)
    expect(repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(1)
  })

  it('persists PartialDirectory instead of publishing worker failures', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 70,
    })
    const leaseId = identity(71)
    const transferJobId = identity(72)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(73))
    const root = await execution.directories.admitDirectory({
      source: {
        directoryId: session.intent.syntheticRoot,
        generation: identity(74),
        path: Object.freeze([]),
      },
      artifactPath: Object.freeze([]),
    }, SIGNAL)
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(75),
      fileRevision: identity(76),
      artifactPath: ['kept.bin'],
      exactSize: 1n,
      parentAdmission: root,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(8), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    const classification = classificationForTransferFailure(
      new Error('directory metadata failed'),
      {
        stage: 'output_commit',
        relation: 'contributor',
        materializationFailureReason: 'directory-finalize-failed',
      },
    )
    if (classification === undefined) throw new TypeError('test failure must be classified')
    const worker = transferWorkerSettlement('CompletedWithErrors', {
      failures: Object.freeze([{
        kind: 'directory' as const,
        directoryId: directoryId(session.intent.syntheticRoot),
        classification,
      }]),
      failureCount: 1,
      fileFailureCount: 0,
      omittedFailureCount: 0,
      fileOutcomes: EMPTY_TRANSFER_FILE_OUTCOME_COUNTS,
      trigger: classification,
    })

    await expect(execution.settle({
      transferJobId,
      worker,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 1n },
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'partial-directory',
      successCount: 1n,
      failureCount: 1n,
    })
  })

  it('reduces a replacement at final observation to NeedsAttention without retiring proof', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    let retired = 0
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(() => { retired += 1 }),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 80,
    })
    const leaseId = identity(81)
    const transferJobId = identity(82)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(83))
    const opened = await execution.output.beginFile(outputFileRequest({
      intent: session.intent,
      fileId: identity(3),
      fileRevision: identity(84),
      artifactPath: [session.reservation.requestedName],
      exactSize: 1n,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(1), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    parent.replaceFile(session.reservation.physicalName, Uint8Array.of(9))

    await expect(execution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 1n },
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
    })
    expect(retired).toBe(0)
  })

  it('preserves paused ranges, releases once, and publishes a reopened single file', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    let retired = 0
    const checkpointFactory = memoryCheckpointFactory(() => { retired += 1 })
    const first = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await singleFileArtifact(),
      operationSeed: 90,
    })
    const leaseId = identity(91)
    const transferJobId = identity(92)
    await startReceiving(repository, first.intent, leaseId)
    const firstExecution = await fsaExecution(first, repository, leaseId, transferJobId, identity(93))
    const opened = await firstExecution.output.beginFile(outputFileRequest({
      intent: first.intent,
      fileId: identity(3),
      fileRevision: identity(94),
      artifactPath: [first.reservation.requestedName],
      exactSize: 4n,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, Uint8Array.of(1, 2), SIGNAL)
    await opened.transaction.pause('cancelled')

    await expect(firstExecution.pause({
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new DOMException('cancelled', 'AbortError'),
    }, SIGNAL)).resolves.toMatchObject({
      kind: 'resumable-receive',
      completedFileCount: 0n,
      completedBytes: 0n,
    })
    expect(locks.releaseCount).toBe(1)
    expect(retired).toBe(0)

    await resumeReceiving(repository, first.intent, leaseId)
    const reopened = await reopenFileSystemAccessOutput({
      intent: first.intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: absentCompatibleNameLedgerFactory,
    })
    const reopenedExecution = await fsaExecution(
      reopened,
      repository,
      leaseId,
      transferJobId,
      identity(95),
    )
    const resumed = await reopenedExecution.output.beginFile(outputFileRequest({
      intent: reopened.intent,
      fileId: identity(3),
      fileRevision: identity(94),
      artifactPath: [reopened.reservation.requestedName],
      exactSize: 4n,
    }), SIGNAL)
    expect(resumed.durableRanges.ranges).toEqual([{ start: 0n, end: 2n }])
    await resumed.transaction.writeRange(2n, Uint8Array.of(3, 4), SIGNAL)
    await resumed.transaction.commit(SIGNAL)
    await expect(reopenedExecution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 1n, fileCount: 1n, directoryCount: 0n, rawBytes: 4n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'published' })
    expect(locks.releaseCount).toBe(2)
    expect(retired).toBe(1)
  })

  it('restores the exact continuation when a resumed execution fails before output binding', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 103,
    })
    const leaseId = identity(104)
    await startReceiving(repository, session.intent, leaseId)
    const firstExecution = await fsaExecution(
      session,
      repository,
      leaseId,
      identity(105),
      identity(106),
    )
    const fallback = await firstExecution.pause({
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new Error('first attempt paused'),
    }, SIGNAL)
    if (fallback.kind !== 'resumable-receive' || fallback.payloadKind !== 'file-set') {
      throw new Error('test did not create a file-set continuation')
    }
    await resumeReceiving(repository, session.intent, leaseId)
    const trace: FSASettlementTraceEvent[] = []
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: session.intent,
      repository,
      lifecycleLeaseId: leaseId,
      transferJobId: identity(107),
      admissionFallback: fallback,
      clock: () => 1_600,
      trace: event => trace.push(event),
    })

    const restored = await settlement.settleExecutionAdmissionFailure(
      session.intent,
      new Error('revision lease expired before execution opened'),
      SIGNAL,
    )

    expect(restored).toMatchObject({
      kind: 'resumable-receive',
      checkpointSetDigest: fallback.checkpointSetDigest,
      completedFileCount: fallback.completedFileCount,
      completedBytes: fallback.completedBytes,
      expiresAt: fallback.expiresAt,
      partialReceiptDigest: fallback.partialReceiptDigest,
    })
    expect(restored.generation).toBe(fallback.generation + 2n)
    expect(trace).toEqual([expect.objectContaining({
      name: 'receive.fsa.continuation.admission_failed',
      restored_checkpoint_set_digest: fallback.checkpointSetDigest,
      restored_expires_at_ms: fallback.expiresAt,
    })])
    if (restored.kind !== 'resumable-receive' || restored.payloadKind !== 'file-set') {
      throw new Error('file-set continuation was not restored')
    }
    await resumeReceiving(repository, session.intent, leaseId)
    const boundSettlement = await createFileSystemAccessSettlementAuthority({
      intent: session.intent,
      repository,
      lifecycleLeaseId: leaseId,
      transferJobId: identity(108),
      admissionFallback: restored,
      clock: () => 1_700,
    })
    boundSettlement.bindMaterialization(session)
    await expect(boundSettlement.settleExecutionAdmissionFailure(
      session.intent,
      new Error('execution failed after output binding'),
      SIGNAL,
    )).resolves.toMatchObject({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
    })
    await session.close()
  })
})

describe('File System Access activation boundary settlement', () => {
  it('settles never-opened file and result-root failures without creating namespace entries', async () => {
    for (const [offset, artifact] of [
      [0, await singleFileArtifact()],
      [10, await resultRootArtifact()],
    ] as const) {
      const parent = new MemoryDirectory(`downloads-${offset}`)
      const repository = new MemoryOperationRepository()
      const session = await bindTask({
        parent,
        repository,
        checkpointFactory: memoryCheckpointFactory(),
        locks: new MemoryLockManager(),
        artifact,
        operationSeed: 100 + offset,
        activate: false,
      })
      const leaseId = identity(101 + offset)
      await installIntentFrozen(repository, session.intent, leaseId)
      const settlement = await createFileSystemAccessSettlementAuthority({
        intent: session.intent,
        repository,
        lifecycleLeaseId: leaseId,
        transferJobId: identity(102 + offset),
        clock: () => 1_000,
      })

      await expect(settlement.settleExecutionAdmissionFailure(session.intent, 'cancelled', SIGNAL))
        .resolves.toMatchObject({ kind: 'discarded' })
      expect(parent.entryNames()).toEqual([])
      await session.close()
    }
  })

  it('records NeedsAttention when root activation may have acquired namespace effects', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const locks = new MemoryLockManager()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 115,
      activate: false,
    })
    const leaseId = identity(116)
    await installIntentFrozen(repository, session.intent, leaseId)
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: session.intent,
      repository,
      lifecycleLeaseId: leaseId,
      transferJobId: identity(117),
      clock: () => 1_000,
    })
    settlement.bindMaterialization(session)
    await parent.getDirectoryHandle(session.reservation.physicalName, { create: true })
    const activationFailure = await session.activate().catch(error => error)

    await expect(settlement.settleExecutionAdmissionFailure(
      session.intent,
      activationFailure,
      SIGNAL,
    )).resolves.toMatchObject({ kind: 'needs-attention' })
    expect(locks.releaseCount).toBe(1)
    await session.close()
    expect(locks.releaseCount).toBe(1)
  })
})

describe('File System Access admission ownership recovery', () => {
  it('turns unresolved checkpoint recovery into operation attention instead of continuation restore', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 109,
    })
    const leaseId = identity(110)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(
      session,
      repository,
      leaseId,
      identity(111),
      identity(112),
    )
    const fallback = await execution.pause({
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new Error('first attempt paused'),
    }, SIGNAL)
    if (fallback.kind !== 'resumable-receive' || fallback.payloadKind !== 'file-set') {
      throw new Error('test did not create a file-set continuation')
    }
    await resumeReceiving(repository, session.intent, leaseId)
    const trace: FSASettlementTraceEvent[] = []
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent: session.intent,
      repository,
      lifecycleLeaseId: leaseId,
      transferJobId: identity(113),
      admissionFallback: fallback,
      clock: () => 1_800,
      trace: event => trace.push(event),
    })

    await expect(settlement.settleExecutionAdmissionFailure(
      session.intent,
      new TargetOwnershipUnknownError('checkpoint', session.intent.operationId),
      SIGNAL,
    )).resolves.toMatchObject({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
    })
    expect(trace).toEqual([expect.objectContaining({
      name: 'receive.fsa.settlement.completed',
      outcome: 'needs-attention',
      ownership_stage: 'checkpoint',
    })])
  })
})

describe('File System Access fresh-page discard authority', () => {
  it('removes one proven unfinished object and persists Discarded from a fresh repository', async () => {
    const fixture = await freshDiscardFixture({ seed: 110, successfulFile: false })

    await expect(discardFreshFixture(fixture)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
      receiptDigest: expect.any(String),
    })
    expect(fixture.parent.entryNames()).toEqual([])
    expect(fixture.retired()).toBe(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(fixture.repository.leases.has(fixture.intent.operationId)).toBe(false)
    expect(fixture.closed()).toBe(1)
  })

  it('preserves complete checkpoint files and persists PartialDirectory(stopped)', async () => {
    const fixture = await freshDiscardFixture({ seed: 120, successfulFile: true })

    const result = await discardFreshFixture(fixture)
    expect(result).toMatchObject({
      lifecycle: {
        kind: 'partial-directory',
        reason: 'stopped',
        successCount: 1n,
        failureCount: 0n,
      },
      receiptDigest: expect.any(String),
    })
    const root = await fixture.parent.getDirectoryHandle(
      fixture.physicalName,
    ) as unknown as MemoryDirectory
    expect(root.fileNames()).toEqual(['kept.bin'])
    expect(await root.fileBytes('kept.bin')).toEqual(Uint8Array.of(7, 8))
    expect(fixture.retired()).toBe(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_CLEANUP)).toHaveLength(1)
    expect(fixture.repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(2)
  })

  it('persists target ownership attention for changed parent or file identity', async () => {
    const changedFile = await freshDiscardFixture({ seed: 130, successfulFile: false })
    const replacement = changedFile.parent.replaceFile(
      changedFile.physicalName,
      Uint8Array.of(9),
    )
    await expect(discardFreshFixture(changedFile)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    expect(await replacement.bytes()).toEqual(Uint8Array.of(9))
    expect(changedFile.retired()).toBe(0)

    const changedParent = await freshDiscardFixture({ seed: 140, successfulFile: false })
    const parentRecord = changedParent.repository.handles.get(
      changedParent.operation.binding.parentHandleId,
    )
    if (parentRecord === undefined) throw new Error('fresh discard parent fixture is missing')
    changedParent.repository.handles.set(parentRecord.id, Object.freeze({
      ...parentRecord,
      handle: new MemoryDirectory('replacement-downloads'),
    }))
    await expect(discardFreshFixture(changedParent)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    expect(changedParent.parent.fileNames()).toEqual([changedParent.physicalName])
    expect(changedParent.retired()).toBe(0)
  })

  it('persists cleanup attention when deletion has no conclusive result', async () => {
    const fixture = await freshDiscardFixture({ seed: 150, successfulFile: false })
    fixture.parent.onRemoveEntry = async (name) => {
      if (name === fixture.physicalName) throw new DOMException('delete unknown', 'OperationError')
    }

    await expect(discardFreshFixture(fixture)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'cleanup-unknown' },
    })
    expect(fixture.parent.fileNames()).toEqual([fixture.physicalName])
    expect(fixture.retired()).toBe(0)
  })

  it('fences stale lifecycle, foreign lease, reused authority, and concurrent discard', async () => {
    const stale = await freshDiscardFixture({ seed: 160, successfulFile: false })
    const staleOperation = Object.freeze({
      ...stale.operation,
      lifecycle: Object.freeze({
        ...stale.operation.lifecycle,
        generation: stale.operation.lifecycle.generation - 1n,
      }),
    })
    await expect(discardFreshFixture(stale, staleOperation)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    expect(stale.parent.fileNames()).toEqual([stale.physicalName])

    const foreign = await freshDiscardFixture({ seed: 170, successfulFile: false })
    const foreignOperation = Object.freeze({
      ...foreign.operation,
      lease: Object.freeze({
        operationId: foreign.intent.operationId,
        leaseId: identity(199),
      }),
    })
    await expect(discardFreshFixture(foreign, foreignOperation)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    expect(foreign.parent.fileNames()).toEqual([foreign.physicalName])

    const concurrent = await freshDiscardFixture({ seed: 180, successfulFile: false })
    const first = discardFreshFixture(concurrent)
    await expect(discardFreshFixture(concurrent)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
    await expect(first).resolves.toMatchObject({ lifecycle: { kind: 'discarded' } })
    await expect(discardFreshFixture(concurrent)).rejects.toMatchObject({
      name: 'InvalidStateError',
    })
  })

  it('cleans at the exact retention boundary only after Expired(cleanup-pending)', async () => {
    const fixture = await freshDiscardFixture({ seed: 190, successfulFile: true })
    const expired = await persistFreshFixtureExpiry(fixture)
    const operation = Object.freeze({ ...fixture.operation, lifecycle: expired })

    await expect(discardFreshFixture(fixture, operation, expired.expiresAt)).resolves.toMatchObject({
      lifecycle: {
        kind: 'expired',
        priorStableState: 'resumable-receive',
        cleanupState: 'clean',
      },
      receiptDigest: expect.any(String),
    })
    const root = await fixture.parent.getDirectoryHandle(
      fixture.physicalName,
    ) as unknown as MemoryDirectory
    expect(root.fileNames()).toEqual(['kept.bin'])
    expect(fixture.retired()).toBe(1)
  })
})
