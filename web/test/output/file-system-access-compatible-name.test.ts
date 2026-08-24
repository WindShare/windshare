import { describe, expect, it } from 'vitest'

import { directoryId } from '../../src/catalog/model'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { fsaDirectoryHandleId } from '../../src/output/browser/filesystem-directory-authority'
import { fsaOwnedDirectoryHandleId } from '../../src/output/browser/indexeddb-root-binding'
import {
  openFileSystemAccessPendingOutcomeCatchUp,
  reopenFileSystemAccessOutput,
} from '../../src/output/file-system-access/session'
import type { CompatibleNameActivationLedger } from '../../src/output/file-system-access/compatible-name/coordinator'
import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import { decodeCompatibleNameSidecar } from '../../src/output/file-system-access/compatible-name/sidecar-codec'
import {
  catchUpFileSystemAccessPendingOutcome,
} from '../../src/output/file-system-access/settlement'
import type { ReopenedDirectTreeOperation } from '../../src/output/resume/reopen-authority'
import {
  RECEIVE_RECORD_RECEIPT,
  receiveOperationLeaseRecord,
} from '../../src/output/workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { classificationForTransferFailure } from '../../src/transfer/job/failures'
import {
  createDirectTreeCoordinateContract,
} from '../../src/transfer/job/coordinate/direct-tree'
import {
  EMPTY_TRANSFER_FILE_OUTCOME_COUNTS,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { TransferStopRequestedError } from '../../src/transfer/output-session'
import { identity } from './planning/fixture'
import {
  MemoryDirectory,
  MemoryFile,
  MemoryCompatibleNameLedger,
  MemoryLockManager,
  MemoryOperationRepository,
  PAUSED,
  SIGNAL,
  SUCCESS,
  bindTask,
  fsaExecution,
  memoryCheckpointFactory,
  resultRootArtifact,
  startReceiving,
} from './file-system-access-lifecycle-fixture'

describe('File System Access compatible-name traversal', () => {
  it('keeps new ordinary sessions ledger-free and reopens them with one header point-read', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    let opens = 0
    let headerReads = 0
    let snapshotLoads = 0
    let closes = 0
    const unexpectedMutation = async (): Promise<never> => {
      throw new Error('ordinary compatible-name ledger was mutated')
    }
    const ledger: CompatibleNameActivationLedger = {
      readHeader: async () => {
        headerReads += 1
        return undefined
      },
      loadOperation: async () => {
        snapshotLoads += 1
        return undefined
      },
      bootstrapOperation: unexpectedMutation,
      claimMapping: unexpectedMutation,
      recordPairOwnership: unexpectedMutation,
      recordCompatibleTargetCreated: unexpectedMutation,
      recordVerifiedDirectoryOwnership: unexpectedMutation,
      commitMapping: unexpectedMutation,
      scanCommittedMappings: unexpectedMutation,
      persistRepairSummary: unexpectedMutation,
      persistPendingTerminalOutcome: unexpectedMutation,
      readPendingTerminalOutcome: unexpectedMutation,
      clearPendingTerminalOutcome: unexpectedMutation,
      readRepairSummary: unexpectedMutation,
      removeVerifiedEmptyOperation: unexpectedMutation,
      close: () => { closes += 1 },
    }
    const openLedger = async () => {
      opens += 1
      return ledger
    }
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 31,
      openCompatibleNameLedger: openLedger,
    })
    expect(opens).toBe(0)
    const currentRootHandleId = await fsaDirectoryHandleId(session.reservation, [])
    const legacyRootHandleId = await legacyFSADirectoryHandleId(session.reservation)
    expect(currentRootHandleId).not.toBe(legacyRootHandleId)
    expect(await repository.readHandle(currentRootHandleId)).toBeDefined()
    expect(await repository.readHandle(legacyRootHandleId)).toBeUndefined()
    const intent = session.intent
    await session.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: openLedger,
    })
    expect({ opens, headerReads, snapshotLoads, closes }).toEqual({
      opens: 1,
      headerReads: 1,
      snapshotLoads: 0,
      closes: 1,
    })
    await reopened.close()
  })

  it('reopens a pair-ready cut without publishing a compatible-name notice', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 33,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await parent.getDirectoryHandle(
      session.reservation.physicalName,
    ) as unknown as MemoryDirectory
    const createFailure = new Error('target creation held before the durable activation cut')
    root.onEntryLookup = lookup => {
      if (lookup.kind !== 'file') return
      if (lookup.name === 'blocked.txt' && !lookup.create) {
        throw new TypeError('simulated native refusal')
      }
      if (lookup.create && lookup.name.startsWith('blocked')) throw createFailure
    }

    await expect(session.beginFile({
      materializationRelativePath: ['blocked.txt'],
      openRevision: async () => ({
        fileId: identity(137),
        fileRevision: identity(138),
        exactSize: 1n,
      }),
    })).rejects.toBe(createFailure)
    expect(ledger.header).toMatchObject({ activationState: 'pair-ready' })
    expect(ledger.header?.repairSummary).toBeUndefined()
    const liveSummaries: CompatibleNameRepairSummary[] = []
    const liveProjection = session.repairProjection
    if (liveProjection === undefined) throw new Error('pair-ready repair source is unavailable')
    const unsubscribeLive = liveProjection.subscribe(summary => liveSummaries.push(summary))
    expect(liveSummaries).toEqual([])
    unsubscribeLive()
    root.onEntryLookup = undefined
    const intent = session.intent
    await session.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: async () => ledger,
      compatibleNamePreparation: {
        platform: 'windows',
        templateProvider: {
          select: () => Object.freeze({
            id: 'windows-powershell-v1',
            scriptFileExtension: '.ps1',
            source: '# test restoration script\n',
          }),
        },
        randomBits: () => 0,
      },
    })
    const reopenedSummaries: CompatibleNameRepairSummary[] = []
    const reopenedProjection = reopened.repairProjection
    if (reopenedProjection === undefined) throw new Error('pair-ready reopen lost repair authority')
    const unsubscribeReopened = reopenedProjection.subscribe(summary => reopenedSummaries.push(summary))
    expect(reopenedSummaries).toEqual([])
    expect(ledger.header).toMatchObject({ activationState: 'pair-ready' })
    unsubscribeReopened()
    await reopened.close()
  })

  it('persists descendant physical translation while checkpoint lineage stays logical across reload', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const openLedger = async () => ledger
    const compatibleNamePreparation = {
      platform: 'windows',
      templateProvider: {
        select: () => Object.freeze({
          id: 'windows-powershell-v1',
          scriptFileExtension: '.ps1',
          source: '# test restoration script\n',
        }),
      },
      randomBits: () => 0,
    } as const
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 32,
      openCompatibleNameLedger: openLedger,
    })
    const root = await parent.getDirectoryHandle(
      session.reservation.physicalName,
    ) as unknown as MemoryDirectory
    root.onEntryLookup = lookup => {
      if (lookup.name === 'blocked.txt' && lookup.create === false) {
        throw new TypeError('simulated native component refusal')
      }
    }

    const first = await session.beginFile({
      materializationRelativePath: ['blocked.txt'],
      openRevision: async () => ({
        fileId: identity(132),
        fileRevision: identity(133),
        exactSize: 2n,
      }),
    })
    const selected = [...ledger.mappings.values()].find(mapping => mapping.entryKind === 'file')
    expect(selected).toMatchObject({
      logicalPath: ['blocked.txt'],
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    expect(ledger.header).toMatchObject({
      activationState: 'active',
      repairSummary: { committedCount: 0, logicalPathSample: [] },
    })
    const liveSummaries: CompatibleNameRepairSummary[] = []
    const liveProjection = session.repairProjection
    if (liveProjection === undefined) throw new Error('created compatible target omitted its projection')
    const unsubscribeLive = liveProjection.subscribe(summary => liveSummaries.push(summary))
    expect(liveSummaries.at(-1)).toMatchObject({ committedCount: 0, logicalPathSample: [] })
    expect(root.entryNames()).not.toContain('blocked.txt')
    await first.writeRange(0n, Uint8Array.of(4, 2))
    await first.commit()
    expect(ledger.mappings.get(selected!.id)).toMatchObject({
      logicalPath: ['blocked.txt'],
      ownedObjectId: first.ownedObjectId,
      commitState: 'committed',
    })
    expect(root.entryNames()).toContain(selected!.physicalComponent)
    unsubscribeLive()
    await compatibleProjectionCaughtUp(session)
    const sidecar = await compatibleSidecar(root, ledger)
    const intent = session.intent
    await session.close()
    const tornTailWriter = await sidecar.createWritable({ keepExistingData: true })
    await tornTailWriter.seek((await sidecar.getFile()).size)
    await tornTailWriter.write(new TextEncoder().encode('M\\t2\\tfile\\t'))
    await tornTailWriter.close()

    const reopened = await reopenFileSystemAccessOutput({
      intent,
      operationRepository: repository,
      lockManager: locks,
      checkpointRepositoryFactory: checkpointFactory,
      openCompatibleNameLedger: openLedger,
      compatibleNamePreparation,
    })
    const reopenedSummaries: CompatibleNameRepairSummary[] = []
    const reopenedProjection = reopened.repairProjection
    if (reopenedProjection === undefined) throw new Error('active repair was not reconstructed')
    const unsubscribeReopened = reopenedProjection.subscribe(summary => reopenedSummaries.push(summary))
    expect(reopenedSummaries.at(-1)).toMatchObject({ committedCount: 1 })
    const resumed = await reopened.beginFile({
      materializationRelativePath: ['blocked.txt'],
      openRevision: async () => ({
        fileId: identity(132),
        fileRevision: identity(133),
        exactSize: 2n,
      }),
    })
    expect(resumed.ownedObjectId).toBe(first.ownedObjectId)
    expect(root.entryNames()).not.toContain('blocked.txt')
    expect(decodeCompatibleNameSidecar(await sidecar.bytes())).toMatchObject({
      trailingByteLength: 0,
      footer: { committedCount: 1, state: 'active' },
    })
    await resumed.close()
    unsubscribeReopened()
    await reopened.close()
  })

})

describe('File System Access compatible-name sibling claims', () => {
  it('scopes compatible and pair claims to their physical siblings', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 34,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await parent.getDirectoryHandle(
      session.reservation.physicalName,
    ) as unknown as MemoryDirectory
    await session.ensureDirectory(['left'])
    await session.ensureDirectory(['right'])
    await session.ensureDirectory(['ordinary'])
    const left = await root.getDirectoryHandle('left') as unknown as MemoryDirectory
    const right = await root.getDirectoryHandle('right') as unknown as MemoryDirectory
    for (const directory of [left, right]) {
      directory.onEntryLookup = lookup => {
        if (lookup.kind === 'file' && lookup.name === 'blocked.txt' && !lookup.create) {
          throw new TypeError('simulated sibling-local native refusal')
        }
      }
    }

    for (const [index, artifactPath] of [['left', 'blocked.txt'], ['right', 'blocked.txt']]
      .entries()) {
      const transaction = await session.beginFile({
        materializationRelativePath: artifactPath,
        openRevision: async () => ({
          fileId: identity(140 + index),
          fileRevision: identity(142 + index),
          exactSize: 1n,
        }),
      })
      await transaction.writeRange(0n, Uint8Array.of(index + 1))
      await transaction.commit()
    }

    const blockedMappings = [...ledger.mappings.values()]
      .filter(mapping => mapping.logicalPath.at(-1) === 'blocked.txt')
    expect(blockedMappings).toHaveLength(2)
    expect(blockedMappings.map(mapping => ({
      parent: mapping.logicalPath.at(-2),
      attempt: mapping.attempt,
      token: mapping.token,
      physical: mapping.physicalComponent,
    }))).toEqual([
      { parent: 'left', attempt: 0, token: ledger.header?.primaryToken, physical: blockedMappings[0]!.physicalComponent },
      { parent: 'right', attempt: 0, token: ledger.header?.primaryToken, physical: blockedMappings[0]!.physicalComponent },
    ])

    const unrelatedPhysicalName = blockedMappings[0]!.physicalComponent
    const ordinaryPhysicalTwin = await session.beginFile({
      materializationRelativePath: ['ordinary', unrelatedPhysicalName],
      openRevision: async () => ({
        fileId: identity(146),
        fileRevision: identity(147),
        exactSize: 1n,
      }),
    })
    await ordinaryPhysicalTwin.writeRange(0n, Uint8Array.of(9))
    await ordinaryPhysicalTwin.commit()

    const pairName = ledger.header!.pair.script.physicalName
    const ordinaryPairTwin = await session.beginFile({
      materializationRelativePath: ['ordinary', pairName],
      openRevision: async () => ({
        fileId: identity(148),
        fileRevision: identity(149),
        exactSize: 1n,
      }),
    })
    await ordinaryPairTwin.writeRange(0n, Uint8Array.of(8))
    await ordinaryPairTwin.commit()
    const ordinary = await root.getDirectoryHandle('ordinary') as unknown as MemoryDirectory
    expect(ordinary.fileNames()).toEqual([pairName, unrelatedPhysicalName].sort())
    await session.close()
  })
})

describe('File System Access compatible-name traversal recovery', () => {
  it('uses authenticated sibling authority lazily and isolates a late logical directory collision', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 35,
      openCompatibleNameLedger: async () => ledger,
    })
    let membershipQueries = 0
    session.bindDirectoryNamespace({
      materializationRelativePath: [],
      logicalSiblingMembership: {
        directoryId: identity(135),
        generation: identity(136),
        hasCommittedName: async () => {
          membershipQueries += 1
          return false
        },
      },
    })
    expect(membershipQueries).toBe(0)
    const root = await parent.getDirectoryHandle(
      session.reservation.physicalName,
    ) as unknown as MemoryDirectory
    root.onEntryLookup = lookup => {
      if (lookup.name === 'native-refusal' && lookup.create === false) {
        throw new TypeError('simulated native directory refusal')
      }
    }

    const created = await session.ensureDirectory(['native-refusal'])
    expect(created.created).toBe(true)
    expect(membershipQueries).toBeGreaterThan(0)
    const mapping = [...ledger.mappings.values()].find(value => value.entryKind === 'directory')
    expect(mapping).toMatchObject({
      logicalPath: ['native-refusal'],
      ownedObjectId: created.ownedObjectId,
      commitState: 'committed',
    })
    await expect(session.ensureDirectory([mapping!.physicalComponent])).rejects.toMatchObject({
      name: 'OutputDirectoryMutationError',
      sessionCompromised: false,
    })
    expect(root.directoryNames()).toContain(mapping!.physicalComponent)
    await session.close()
  })
})

describe('File System Access compatible-name terminal cut', () => {
  it('uses the terminal cut for Stop and closes a validated stopped footer', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 55,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-stop')
    const leaseId = identity(152)
    const transferJobId = identity(153)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(154))
    await admitAndFinalizeRoot(execution, session.intent, identity(155))

    await expect(execution.stop!({
      transferJobId,
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new TransferStopRequestedError(),
    }, SIGNAL)).resolves.toMatchObject({ kind: 'partial-directory', reason: 'stopped' })

    expect(ledger.header?.pendingTerminalOutcome).toBeUndefined()
    expect(ledger.header?.repairSummary).toMatchObject({
      pendingCatchUp: false,
      latestObservedFooter: { state: 'stopped', committedCount: 1 },
    })
    expect(decodeCompatibleNameSidecar(await (await compatibleSidecar(root, ledger)).bytes()).footer)
      .toEqual({ state: 'stopped', committedCount: 1 })
  })

  it('uses failed footer for a repaired partial completion', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 66,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-failure')
    const leaseId = identity(196)
    const transferJobId = identity(197)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(198))
    await admitAndFinalizeRoot(execution, session.intent, identity(199))
    const worker = completedWithDirectoryError(session.intent.syntheticRoot)

    await expect(execution.settle({
      transferJobId,
      worker,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'partial-directory', reason: 'failures' })

    expect(ledger.header?.pendingTerminalOutcome).toBeUndefined()
    expect(ledger.header?.repairSummary).toMatchObject({
      pendingCatchUp: false,
      latestObservedFooter: { state: 'failed', committedCount: 1 },
    })
    expect(decodeCompatibleNameSidecar(await (await compatibleSidecar(root, ledger)).bytes()).footer)
      .toEqual({ state: 'failed', committedCount: 1 })
  })

  it('removes a verified empty repair pair and publishes an ordinary partial result', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 67,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await parent.getDirectoryHandle(
      session.reservation.physicalName,
    ) as unknown as MemoryDirectory
    root.onEntryLookup = lookup => {
      if (lookup.name === 'blocked-empty' && lookup.create === false) {
        throw new TypeError('simulated native directory refusal')
      }
      if (lookup.name.startsWith('blocked-empty.windshare-') && lookup.create === true) {
        throw new TypeError('simulated compatible target refusal')
      }
    }
    await root.getFileHandle('user-kept.bin', { create: true })
    await expect(session.ensureDirectory(['blocked-empty']))
      .rejects.toThrow('simulated compatible target refusal')
    const pair = ledger.header?.pair
    if (pair === undefined) {
      throw new Error('Compatible-name fallback did not create its owned restoration pair')
    }
    const leaseId = identity(200)
    const transferJobId = identity(201)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(202))
    await admitAndFinalizeRoot(execution, session.intent, identity(203))

    const worker = completedWithDirectoryError(session.intent.syntheticRoot)
    await expect(execution.settle({
      transferJobId,
      worker,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'partial-directory', reason: 'failures' })

    expect(ledger.header).toBeUndefined()
    expect(ledger.mappings.size).toBe(0)
    expect(session.repairSummary()).toBeUndefined()
    expect(root.fileNames()).toContain('user-kept.bin')
    expect(root.fileNames()).not.toContain(pair.script.physicalName)
    expect(root.fileNames()).not.toContain(pair.sidecar.physicalName)
    await expect(repository.readHandle(pair.script.handleId)).resolves.toBeUndefined()
    await expect(repository.readHandle(pair.sidecar.handleId)).resolves.toBeUndefined()
  })

  it('persists the compatible pending outcome before the terminal footer and evidence cut', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const pendingGate = deferred<void>()
    const pendingPersisted = deferred<void>()
    const ledger = new class extends MemoryCompatibleNameLedger {
      override async persistPendingTerminalOutcome(
        input: Parameters<MemoryCompatibleNameLedger['persistPendingTerminalOutcome']>[0],
      ) {
        const header = await super.persistPendingTerminalOutcome(input)
        pendingPersisted.resolve()
        await pendingGate.promise
        return header
      }
    }(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 56,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-terminal')
    const leaseId = identity(156)
    const transferJobId = identity(157)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(158))
    const rootAdmission = await admitAndFinalizeRoot(execution, session.intent, identity(159))
    expect(rootAdmission).toBeDefined()

    const settling = execution.settle({
      transferJobId,
      worker: SUCCESS,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    }, SIGNAL)
    await pendingPersisted.promise

    const lifecycle = await repository.readLifecycle(session.intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle).kind)
      .toBe('receiving')
    expect(ledger.header?.pendingTerminalOutcome?.ordinaryLifecycle.kind).toBe('published')
    expect(decodeCompatibleNameSidecar(await (await compatibleSidecar(root, ledger)).bytes()).footer.state)
      .toBe('active')
    expect(repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(0)

    pendingGate.resolve()
    await expect(settling).resolves.toMatchObject({ kind: 'published' })
    expect(ledger.header?.pendingTerminalOutcome).toBeUndefined()
    expect(ledger.header?.repairSummary).toMatchObject({
      pendingCatchUp: false,
      latestObservedFooter: { state: 'completed', committedCount: 1 },
    })
    const terminalSidecar = await compatibleSidecar(root, ledger)
    expect(decodeCompatibleNameSidecar(await terminalSidecar.bytes()).footer.state).toBe('completed')
    const terminalBytes = await terminalSidecar.bytes()
    const terminalNamespace = root.entryNames()
    await Promise.resolve()
    expect(await terminalSidecar.bytes()).toEqual(terminalBytes)
    expect(root.entryNames()).toEqual(terminalNamespace)
  })

  it('keeps the latest compatible sidecar checkpoint active on Pause', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 57,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-pause')
    const leaseId = identity(160)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, identity(161), identity(162))
    await admitAndFinalizeRoot(execution, session.intent, identity(163))

    await expect(execution.pause({
      worker: PAUSED,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      reason: new Error('requested pause'),
    }, SIGNAL)).resolves.toMatchObject({ kind: 'resumable-receive' })

    expect(ledger.header?.pendingTerminalOutcome).toBeUndefined()
    expect(decodeCompatibleNameSidecar(await (await compatibleSidecar(root, ledger)).bytes()).footer.state)
      .toBe('active')
  })

  it('retains the exact projector failure for a failed footer without terminal publication', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 58,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-projector')
    await compatibleProjectionCaughtUp(session)
    const projectorFailure = new Error('sidecar writer failed')
    ;(await compatibleSidecar(root, ledger)).setWritableFailure(projectorFailure)
    const leaseId = identity(164)
    const transferJobId = identity(165)
    await startReceiving(repository, session.intent, leaseId)
    const execution = await fsaExecution(session, repository, leaseId, transferJobId, identity(166))
    await admitAndFinalizeRoot(execution, session.intent, identity(167))
    const worker = completedWithDirectoryError(session.intent.syntheticRoot)

    let failure: unknown
    try {
      await execution.settle({
        transferJobId,
        worker,
        materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      }, SIGNAL)
    } catch (error) {
      failure = error
    }
    expect(failure).toBe(projectorFailure)
    const lifecycle = await repository.readLifecycle(session.intent.operationId)
    expect(lifecycle === undefined ? undefined : decodeStoredReceiveLifecycleState(lifecycle).kind)
      .toBe('receiving')
    expect(ledger.header?.pendingTerminalOutcome).toMatchObject({
      footerState: 'failed',
      ordinaryLifecycle: { kind: 'partial-directory', reason: 'failures' },
    })
    expect(ledger.header?.repairSummary).toMatchObject({
      pendingCatchUp: true,
      latestObservedFooter: { state: 'active', committedCount: 1 },
    })
    expect(repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(0)
  })

  it('reconciles a pending outcome through local-only catch-up authority', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const ledger = new MemoryCompatibleNameLedger(repository)
    let retireCount = 0
    const checkpointFactory = memoryCheckpointFactory(() => { retireCount += 1 })
    const locks = new MemoryLockManager()
    const session = await bindTask({
      parent,
      repository,
      checkpointFactory,
      locks,
      artifact: await resultRootArtifact(),
      operationSeed: 59,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await activateCompatibleDirectory(session, parent, 'blocked-catch-up')
    await compatibleProjectionCaughtUp(session)
    ;(await compatibleSidecar(root, ledger)).setWritableFailure(new Error('terminal write interrupted'))
    const priorLeaseId = identity(168)
    const transferJobId = identity(169)
    await startReceiving(repository, session.intent, priorLeaseId)
    const execution = await fsaExecution(session, repository, priorLeaseId, transferJobId, identity(170))
    await admitAndFinalizeRoot(execution, session.intent, identity(171))
    const worker = completedWithDirectoryError(session.intent.syntheticRoot)
    await expect(execution.settle({
      transferJobId,
      worker,
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
    }, SIGNAL)).rejects.toThrow('terminal write interrupted')

    const pending = ledger.header?.pendingTerminalOutcome
    expect(pending).toBeDefined()
    ;(await compatibleSidecar(root, ledger)).setWritableFailure(undefined)
    const newLeaseId = identity(172)
    await repository.commitTransition({
      operationId: session.intent.operationId,
      expectedLeaseId: priorLeaseId,
      lease: {
        kind: 'put',
        record: receiveOperationLeaseRecord({
          operationId: session.intent.operationId,
          leaseId: newLeaseId,
          acquiredAt: 2_000,
          heartbeatAt: 2_000,
        }),
      },
    })
    const lifecycleRecord = await repository.readLifecycle(session.intent.operationId)
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord!)
    const operation = {
      kind: 'direct-tree',
      intent: session.intent,
      lifecycle,
      lease: { leaseId: newLeaseId },
      repository,
    } as unknown as ReopenedDirectTreeOperation

    const result = await catchUpFileSystemAccessPendingOutcome({
      operation,
      signal: SIGNAL,
      clock: () => 2_000,
      openSession: caughtUpOperation => openFileSystemAccessPendingOutcomeCatchUp({
        intent: caughtUpOperation.intent,
        operationRepository: caughtUpOperation.repository,
        lockManager: locks,
        checkpointRepositoryFactory: checkpointFactory,
        openCompatibleNameLedger: async () => ledger,
        compatibleNamePreparation: {
          platform: 'windows',
          templateProvider: {
            select: () => Object.freeze({
              id: 'windows-powershell-v1',
              scriptFileExtension: '.ps1',
              source: '# test restoration script\n',
            }),
          },
          randomBits: () => 0,
        },
      }),
    })

    expect(result.lifecycle).toMatchObject({ kind: 'partial-directory', reason: 'failures' })
    expect(result.repairSummary).toMatchObject({
      pendingCatchUp: false,
      latestObservedFooter: { state: 'failed', committedCount: 1 },
    })
    expect(ledger.header?.pendingTerminalOutcome).toBeUndefined()
    expect(retireCount).toBe(1)
    expect(repository.recordsOfKind(RECEIVE_RECORD_RECEIPT)).toHaveLength(1)
  })

})

function completedWithDirectoryError(directoryIdentity: string) {
  const classification = classificationForTransferFailure(new Error('directory metadata failed'), {
    stage: 'output_commit',
    relation: 'contributor',
    materializationFailureReason: 'directory-finalize-failed',
  })
  if (classification === undefined) throw new TypeError('test failure must be classified')
  return transferWorkerSettlement('CompletedWithErrors', {
    failures: Object.freeze([{
      kind: 'directory' as const,
      directoryId: directoryId(directoryIdentity),
      classification,
    }]),
    failureCount: 1,
    fileFailureCount: 0,
    omittedFailureCount: 0,
    fileOutcomes: EMPTY_TRANSFER_FILE_OUTCOME_COUNTS,
    trigger: classification,
  })
}

async function activateCompatibleDirectory(
  session: Awaited<ReturnType<typeof bindTask>>,
  parent: MemoryDirectory,
  logicalName: string,
): Promise<MemoryDirectory> {
  const root = await parent.getDirectoryHandle(
    session.reservation.physicalName,
  ) as unknown as MemoryDirectory
  root.onEntryLookup = lookup => {
    if (lookup.name === logicalName && lookup.create === false) {
      throw new TypeError('simulated native component refusal')
    }
  }
  await session.ensureDirectory([logicalName])
  return root
}

async function legacyFSADirectoryHandleId(
  reservation: Awaited<ReturnType<typeof bindTask>>['reservation'],
): Promise<string> {
  const material = new TextEncoder().encode(
    `windshare/fsa-directory-locator/v1\0${reservation.digest}\0`,
  )
  const digest = encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', material)))
  return fsaOwnedDirectoryHandleId(reservation.operationId, digest)
}

async function admitAndFinalizeRoot(
  execution: Awaited<ReturnType<typeof fsaExecution>>,
  intent: Awaited<ReturnType<typeof bindTask>>['intent'],
  generation: string,
) {
  const coordinates = await createDirectTreeCoordinateContract(intent)
  const layout = coordinates.intent.artifact.layout
  const sourcePath = layout.kind === 'result-root' && layout.root.anchor.kind === 'directory'
    ? layout.root.anchor.sourcePath.split('/')
    : []
  const directory = coordinates.projectDirectory(sourcePath)
  if (directory.kind !== 'materialize') {
    throw new TypeError('test root must be materialized')
  }
  const admission = await execution.directories.admitDirectory({
    directory: {
      directoryId: directoryId(coordinates.rootExpectation.kind === 'materialized-directory'
        ? coordinates.rootExpectation.directoryId
        : intent.syntheticRoot),
      generation,
      path: directory.relativePath,
    },
    sourceAuthenticationPath: directory.sourceAuthenticationPath,
    logicalArtifactPath: directory.logicalArtifactPath,
  }, SIGNAL)
  await execution.directories.finalizeDirectory(admission, SIGNAL)
  return admission
}

async function compatibleSidecar(
  root: MemoryDirectory,
  ledger: MemoryCompatibleNameLedger,
): Promise<MemoryFile> {
  const name = ledger.header?.pair.sidecar.physicalName
  if (name === undefined) throw new Error('compatible-name sidecar identity is unavailable')
  const file = await root.getFileHandle(name)
  if (!(file instanceof MemoryFile)) throw new Error('compatible-name sidecar is unavailable')
  return file
}

async function compatibleProjectionCaughtUp(
  session: Awaited<ReturnType<typeof bindTask>>,
): Promise<void> {
  const source = session.repairProjection
  if (source === undefined) throw new Error('compatible-name repair projection is unavailable')
  const caughtUp = deferred<void>()
  const unsubscribe = source.subscribe(summary => {
    if (!summary.pendingCatchUp && summary.latestObservedFooter?.state === 'active' &&
        summary.latestObservedFooter.committedCount === summary.committedCount) {
      caughtUp.resolve()
    }
  })
  try {
    await caughtUp.promise
  } finally {
    unsubscribe()
  }
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
}
