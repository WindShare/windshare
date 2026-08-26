import { describe, expect, it, vi } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { browserHandoffOffer } from '../../src/output/capability/acquisition'
import type { FileSystemAccessOutputSession } from '../../src/output/file-system-access/session'
import {
  createFileSystemAccessSettlementAuthority,
  type FileSystemAccessOperationSettlementAuthority,
  type FSASettlementRepository,
} from '../../src/output/file-system-access/settlement'
import { createMaterializationLedgerBinding } from '../../src/output/materialization-ledger/codec'
import {
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
} from '../../src/output/materialization-ledger/journal'
import type { BrowserHandoffPublisher } from '../../src/output/portable/browser-download'
import {
  createPortableExecutionRoutes,
  type PortableExecutionEnvironment,
  type PortableExecutionLifecycleAuthority,
  type PortableExecutionRoutes,
} from '../../src/output/portable/preparation'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import {
  disabledOutputExecutionProfile,
  outputSessionIdentity,
  type DirectAtomicExecution,
  type DirectTreeExecution,
  type ExactPreparationEvidence,
  type OutputSessionIdentity,
  type PlanPauseRequest,
  type PortableExecution,
  type WorkspaceExecution,
} from '../../src/transfer/output-session'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import {
  V2PlanRouteUnavailableError,
  createV2PlanExecutionAuthority,
  type V2PlanExecutionRouteRegistry,
} from '../../src/transfer/settlement/v2-plan-authority'
import {
  digestIdentity,
  fileEntry,
  identity,
  identityText,
  receiveIntentFixture,
  selectOnlyFile,
  testOutput,
} from './v2-job-fixture'
import { createTestCheckpointAuthorities } from '../output/persistent-tree-file-fixture'

describe('production plan execution authority', () => {
  it('composes the FSA settlement owner into the real DirectTree execution route', async () => {
    const intent = await fsaDirectTreeIntent()
    const repository = inertFSASettlementRepository()
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent,
      repository,
      lifecycleLeaseId: identityText(101),
      transferJobId: identityText(102),
    })
    const session = fsaMaterializationPort(intent, repository)
    const admittedRoot = await session.materializeDirectory({
      relativePath: [],
      directoryId: identityText(104),
      generation: identityText(105),
    })
    const repeatedRoot = await session.materializeDirectory({
      relativePath: [],
      directoryId: identityText(104),
      generation: identityText(105),
    })
    const finalizedRoot = await session.finalizeDirectory(
      admittedRoot.ledgerAdmission,
      { kind: 'finalized' },
    )
    const outputIdentity = outputSessionIdentity({
      backend: 'file-system-access',
      outputSessionId: identityText(103),
    })
    const authority = await createV2PlanExecutionAuthority({
      intent,
      routes: fileSystemAccessRouteRegistry({ session, settlement, outputIdentity }),
    })

    const execution = await authority.openDirectTree(
      asDirectTree(intent),
      new AbortController().signal,
    )

    expect(execution).toMatchObject({ planKind: 'direct-tree' })
    expect(execution.output.identity).toEqual(outputIdentity)
    expect(execution.directories).toEqual(expect.objectContaining({
      admitDirectory: expect.any(Function),
      finalizeDirectory: expect.any(Function),
    }))
    expect(repeatedRoot).toMatchObject({
      ownedObjectId: admittedRoot.ownedObjectId,
      created: false,
      ledgerAdmission: admittedRoot.ledgerAdmission,
    })
    expect(finalizedRoot).toMatchObject({
      kind: 'directory-finalized',
      admissionEntryId: admittedRoot.ledgerAdmission.entryId,
      admissionEntryDigest: admittedRoot.ledgerAdmission.entryDigest,
      outcome: { kind: 'finalized' },
    })
  })

  it('composes real portable Original and ZIP routes into operation-bound authorities', async () => {
    const file = fileEntry(identity(12), 'portable.bin', 4n)
    const selection = selectOnlyFile(file)
    const originalIntent = await receiveIntentFixture({
      planKind: 'portable-handoff',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const zipIntent = await receiveIntentFixture({
      planKind: 'portable-handoff',
      artifactKind: 'zip-archive',
      selection,
      file,
    })
    const publisher: BrowserHandoffPublisher & { handoff: ReturnType<typeof vi.fn> } = {
      handoff: vi.fn(() => { throw new Error('paused composition proof must not publish') }),
    }
    const portableLifecycle = portableExecutionLifecycle()
    const originalRoutes = createPortableExecutionRoutes({
      environment: portableEnvironment(),
      attemptId: identityText(104),
      publisher,
      assembly: { Blob, WritableStream },
      lifecycle: portableLifecycle,
    })
    const zipRoutes = createPortableExecutionRoutes({
      environment: portableEnvironment(),
      attemptId: identityText(105),
      publisher,
      assembly: { Blob, WritableStream },
      lifecycle: portableLifecycle,
      createZipSpool: () => { throw new Error('paused ZIP proof must not open a spool') },
    })
    const originalAuthority = await createV2PlanExecutionAuthority({
      intent: originalIntent,
      routes: portableRouteRegistry(originalIntent, originalRoutes),
    })
    const zipAuthority = await createV2PlanExecutionAuthority({
      intent: zipIntent,
      routes: portableRouteRegistry(zipIntent, zipRoutes),
    })

    const original = await originalAuthority.preparePortable(
      asPortable(originalIntent),
      portableOriginalEvidence(originalIntent, file),
      new AbortController().signal,
    )
    const zip = await zipAuthority.preparePortable(
      asPortable(zipIntent),
      portableZipEvidence(zipIntent, file),
      new AbortController().signal,
    )

    expect(original.kind).toBe('accepted')
    expect(zip.kind).toBe('accepted')
    if (original.kind !== 'accepted' || zip.kind !== 'accepted') return
    await expect(original.execution.pause(
      portablePauseRequest(),
      new AbortController().signal,
    )).resolves.toMatchObject({ kind: 'restart-required' })
    await expect(zip.execution.pause(
      portablePauseRequest(),
      new AbortController().signal,
    )).resolves.toMatchObject({ kind: 'restart-required' })
    expect(portableLifecycle.recordAbort).toHaveBeenCalledTimes(2)
    expect(publisher.handoff).not.toHaveBeenCalled()
  })

  it('routes each installed production plan explicitly and keeps DirectAtomic absent', async () => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const directTreeIntent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    })
    const workspaceOriginalIntent = await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const workspaceZipIntent = await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'zip-archive',
      selection,
      file,
    })
    const portableOriginalIntent = await receiveIntentFixture({
      planKind: 'portable-handoff',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const portableZipIntent = await receiveIntentFixture({
      planKind: 'portable-handoff',
      artifactKind: 'zip-archive',
      selection,
      file,
    })
    const routes: string[] = []

    await (await createV2PlanExecutionAuthority({
      intent: directTreeIntent,
      routes: routeRegistry({
        directTree: {
          open: async (intent) => {
            routes.push('direct-tree')
            return directTreeExecution(intent)
          },
        },
      }),
    })).openDirectTree(asDirectTree(directTreeIntent), new AbortController().signal)

    await (await createV2PlanExecutionAuthority({
      intent: workspaceOriginalIntent,
      routes: routeRegistry({
        workspaceOriginal: {
          admit: async (intent, evidence) => {
            routes.push(`workspace-original:${evidence.generation}`)
            return Object.freeze({ kind: 'accepted', execution: workspaceExecution(intent) })
          },
        },
      }),
    })).openWorkspaceOriginal(
      asWorkspaceOriginal(workspaceOriginalIntent),
      singleFileEvidence(file),
      new AbortController().signal,
    )

    await (await createV2PlanExecutionAuthority({
      intent: workspaceZipIntent,
      routes: routeRegistry({
        workspaceZip: {
          prepare: async (intent, evidence) => {
            routes.push(`workspace-zip:${evidence.entryCount.toString()}`)
            return Object.freeze({ kind: 'accepted', execution: workspaceExecution(intent) })
          },
        },
      }),
    })).prepareWorkspaceZip(
      asWorkspaceZip(workspaceZipIntent),
      exactPreparationEvidence(file),
      new AbortController().signal,
    )

    await (await createV2PlanExecutionAuthority({
      intent: portableOriginalIntent,
      routes: routeRegistry({
        portableOriginal: {
          prepare: async (intent) => {
            routes.push('portable-original')
            return Object.freeze({ kind: 'accepted', execution: portableExecution(intent) })
          },
        },
      }),
    })).preparePortable(
      asPortable(portableOriginalIntent),
      exactPreparationEvidence(file),
      new AbortController().signal,
    )

    await (await createV2PlanExecutionAuthority({
      intent: portableZipIntent,
      routes: routeRegistry({
        portableZip: {
          prepare: async (intent) => {
            routes.push('portable-zip')
            return Object.freeze({ kind: 'accepted', execution: portableExecution(intent) })
          },
        },
      }),
    })).preparePortable(
      asPortable(portableZipIntent),
      exactPreparationEvidence(file),
      new AbortController().signal,
    )

    const directAtomicIntent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const atomicAuthority = await createV2PlanExecutionAuthority({
      intent: directAtomicIntent,
      routes: routeRegistry(),
    })
    await expect(atomicAuthority.openDirectAtomic(
      asDirectAtomic(directAtomicIntent),
      new AbortController().signal,
    )).rejects.toBeInstanceOf(V2PlanRouteUnavailableError)

    expect(routes).toEqual([
      'direct-tree',
      `workspace-original:${identityText(90)}`,
      'workspace-zip:2',
      'portable-original',
      'portable-zip',
    ])
  })

  it('snapshots evidence and delegates rejection, abort, and unknown settlement to lifecycle owners', async () => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const admittedEvidence: string[][] = []
    const settleExecutionAdmissionFailure = vi.fn(async () => discardedState(intent))
    const recordSettlementUnknown = vi.fn(async () => needsAttentionState(intent))
    const authority = await createV2PlanExecutionAuthority({
      intent,
      routes: {
        workspaceOriginal: {
          admit: async (_bound, evidence) => {
            admittedEvidence.push([...evidence.sourcePath])
            expect(Object.isFrozen(evidence)).toBe(true)
            expect(Object.isFrozen(evidence.sourcePath)).toBe(true)
            return Object.freeze({ kind: 'rejected', state: discardedState(intent) })
          },
        },
        lifecycle: { settleExecutionAdmissionFailure, recordSettlementUnknown },
      },
    })
    const mutablePath = ['payload.bin']
    const result = await authority.openWorkspaceOriginal(
      asWorkspaceOriginal(intent),
      { ...singleFileEvidence(file), sourcePath: mutablePath },
      new AbortController().signal,
    )
    mutablePath[0] = 'changed.bin'

    expect(result).toEqual({ kind: 'rejected', state: discardedState(intent) })
    expect(admittedEvidence).toEqual([['payload.bin']])
    await expect(authority.settleExecutionAdmissionFailure(
      intent,
      new Error('admission failed'),
      new AbortController().signal,
    )).resolves.toMatchObject({ kind: 'discarded' })
    await expect(authority.recordSettlementUnknown(
      intent,
      new AbortController().signal,
    )).resolves.toMatchObject({ kind: 'needs-attention' })
    expect(settleExecutionAdmissionFailure).toHaveBeenCalledOnce()
    expect(recordSettlementUnknown).toHaveBeenCalledOnce()
  })

  it('rejects a foreign operation before invoking an acquired route', async () => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const bound = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const foreign = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const open = vi.fn(async (
      intent: Parameters<NonNullable<V2PlanExecutionRouteRegistry['directAtomic']>['open']>[0],
    ) => directAtomicExecution(intent))
    const authority = await createV2PlanExecutionAuthority({
      intent: bound,
      routes: routeRegistry({ directAtomic: { open } }),
    })

    await expect(authority.openDirectAtomic(
      asDirectAtomic(foreign),
      new AbortController().signal,
    )).rejects.toThrow('another receive operation')
    expect(open).not.toHaveBeenCalled()
  })

  it('snapshots the acquired callback instead of following later registry mutation', async () => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection: selectOnlyFile(file),
      file,
    })
    const original = vi.fn(async () => directAtomicExecution(intent))
    const replacement = vi.fn(async () => directAtomicExecution(intent))
    const route = { open: original }
    const authority = await createV2PlanExecutionAuthority({
      intent,
      routes: routeRegistry({ directAtomic: route }),
    })
    route.open = replacement

    await authority.openDirectAtomic(asDirectAtomic(intent), new AbortController().signal)
    expect(original).toHaveBeenCalledOnce()
    expect(replacement).not.toHaveBeenCalled()
  })
})

function fileSystemAccessRouteRegistry(input: Readonly<{
  session: FileSystemAccessOutputSession
  settlement: FileSystemAccessOperationSettlementAuthority
  outputIdentity: OutputSessionIdentity
}>): V2PlanExecutionRouteRegistry {
  return Object.freeze({
    directTree: Object.freeze({
      open: (
        intent: Parameters<NonNullable<V2PlanExecutionRouteRegistry['directTree']>['open']>[0],
      ) => createPersistentDirectTreeExecution({
        ...createTestCheckpointAuthorities(),
        intent,
        materialization: input.session,
        executionProfile: disabledOutputExecutionProfile(1),
        outputIdentity: input.outputIdentity,
        settlement: input.settlement.bindMaterialization(input.session),
      }),
    }),
    lifecycle: input.settlement,
  })
}

async function fsaDirectTreeIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identityText(1),
    syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createResultRootDirectoryTreeArtifact(
    createSyntheticSelectionResultRoot(),
  )
  if (artifact.layout.kind !== 'result-root') {
    throw new TypeError('FSA composition proof requires a named result root')
  }
  const operationId = identityText(110)
  const reservation = await createFSANamedEntryReservation({
    operationId,
    reservationId: identityText(111),
    artifact,
    authorityRef: digestIdentity(112),
    logicalReservedName: artifact.layout.root.name,
    physicalName: artifact.layout.root.name,
    collisionIndex: 0,
  })
  const plan = await createDirectTreePlan(artifact, reservation)
  return createReceiveIntent({ selection, artifact, plan })
}

function inertFSASettlementRepository(): FSASettlementRepository {
  return {
    commitTransition: async () => undefined,
    readLifecycle: async () => undefined,
    readLease: async () => undefined,
    readRecord: async () => undefined,
    readHandle: async () => undefined,
  }
}

function fsaMaterializationPort(
  intent: ReceiveIntent,
  repository: FSASettlementRepository,
): FileSystemAccessOutputSession {
  const directoryOwnedObjects = new Map<string, string>()
  let nextDirectoryIdentity = 120
  const ensureDirectory = async (path: readonly string[]) => {
    const relativePath = snapshotMaterializationRootRelativePath(path)
    const pathKey = JSON.stringify(relativePath)
    const existingOwnedObjectId = directoryOwnedObjects.get(pathKey)
    const ownedObjectId = existingOwnedObjectId ?? digestIdentity(nextDirectoryIdentity++)
    directoryOwnedObjects.set(pathKey, ownedObjectId)
    return Object.freeze({
      ownedObjectId,
      created: existingOwnedObjectId === undefined,
    })
  }
  const ledgerBinding = () => {
    if (intent.plan.kind !== 'direct-tree') {
      throw new TypeError('FSA materialization fixture requires a DirectTree intent')
    }
    return createMaterializationLedgerBinding({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      materializationBindingDigest: intent.plan.reservation.digest,
      authorityRef: intent.plan.reservation.authorityRef,
    })
  }
  return {
    intent,
    usesOperationRepository: (candidate: unknown) => candidate === repository,
    beginFile: async () => { throw new Error('composition proof does not open content') },
    ensureDirectory,
    materializeDirectory: async (
      request: Parameters<FileSystemAccessOutputSession['materializeDirectory']>[0],
    ) => {
      const relativePath = snapshotMaterializationRootRelativePath(request.relativePath)
      const materialized = await ensureDirectory(relativePath)
      const ledgerAdmission = await createMaterializationDirectoryAdmittedEntry(
        await ledgerBinding(),
        {
          relativePath,
          directoryId: request.directoryId,
          generation: request.generation,
          ownedObjectId: materialized.ownedObjectId,
          ...(request.parent === undefined ? {} : { parent: request.parent }),
          ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
        },
      )
      return Object.freeze({ ...materialized, ledgerAdmission })
    },
    finalizeDirectory: async (
      admission: Parameters<FileSystemAccessOutputSession['finalizeDirectory']>[0],
      outcome: Parameters<FileSystemAccessOutputSession['finalizeDirectory']>[1],
    ) =>
      createMaterializationDirectoryFinalizedEntry(await ledgerBinding(), admission, outcome),
    close: async () => undefined,
  } as unknown as FileSystemAccessOutputSession
}

function portableRouteRegistry(
  intent: ReceiveIntent,
  routes: PortableExecutionRoutes,
): V2PlanExecutionRouteRegistry {
  return Object.freeze({
    ...routes,
    lifecycle: Object.freeze({
      settleExecutionAdmissionFailure: async () => discardedState(intent),
      recordSettlementUnknown: async () => needsAttentionState(intent),
    }),
  })
}

function portableEnvironment(): PortableExecutionEnvironment {
  return Object.freeze({
    portable: Object.freeze({
      routeId: 'portable-memory',
      kind: 'portable-memory',
      persistence: 'none',
      maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
      assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
      maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    }),
    handoffTarget: browserHandoffOffer({
      supportsWorkspacePackage: true,
      supportsPortableArtifact: true,
    }),
  })
}

function portableExecutionLifecycle(): PortableExecutionLifecycleAuthority & {
  recordAbort: ReturnType<typeof vi.fn>
} {
  return {
    rejectAdmission: vi.fn(async ({ intent }) => discardedState(intent)),
    recordDownloadStarted: vi.fn(async ({ intent, attemptId }) => Object.freeze({
      kind: 'download-started' as const,
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      generation: 4n,
      attemptKind: 'portable' as const,
      attemptId,
    })),
    recordAbort: vi.fn(async ({ intent, reason }) => Object.freeze({
      kind: 'restart-required' as const,
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      generation: 4n,
      reason,
      receiptDigest: digestIdentity(79),
    })),
  }
}

function portablePauseRequest(): PlanPauseRequest {
  return Object.freeze({
    worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
    materialization: Object.freeze({
      entryCount: 0n,
      fileCount: 0n,
      directoryCount: 0n,
      rawBytes: 0n,
    }),
    selectionFacts: Object.freeze({
      discoveredFileCount: 1n,
      discoveredBytes: 4n,
      discovery: 'complete',
    }),
    reason: new DOMException('composition proof complete', 'AbortError'),
  })
}

function portableOriginalEvidence(
  intent: ReceiveIntent,
  file: ReturnType<typeof fileEntry>,
): ExactPreparationEvidence {
  if (intent.artifact.kind !== 'original-file') {
    throw new TypeError('portable Original proof requires an original-file intent')
  }
  return Object.freeze({
    generations: Object.freeze([Object.freeze({
      directoryId: intent.syntheticRoot,
      generation: identityText(90),
    })]),
    entries: Object.freeze([Object.freeze({
      kind: 'file' as const,
      sourcePath: Object.freeze([file.name]),
      artifactPath: Object.freeze([intent.artifact.suggestedName]),
      fileId: file.idText,
      containingDirectoryId: intent.syntheticRoot,
      generation: identityText(90),
      exactSize: file.expectedSize,
    })]),
    entryCount: 1n,
    fileCount: 1n,
    directoryCount: 0n,
    selectedRawBytes: file.expectedSize,
  })
}

function portableZipEvidence(
  intent: ReceiveIntent,
  file: ReturnType<typeof fileEntry>,
): ExactPreparationEvidence {
  if (intent.artifact.kind !== 'zip-archive') {
    throw new TypeError('portable ZIP proof requires a ZIP intent')
  }
  const resultRoot = intent.artifact.layout.name
  return Object.freeze({
    generations: Object.freeze([Object.freeze({
      directoryId: intent.syntheticRoot,
      generation: identityText(90),
    })]),
    entries: Object.freeze([
      Object.freeze({
        kind: 'directory' as const,
        sourcePath: Object.freeze([]),
        artifactPath: Object.freeze([resultRoot]),
        directoryId: intent.syntheticRoot,
        generation: identityText(90),
        role: 'result-root' as const,
      }),
      Object.freeze({
        kind: 'file' as const,
        sourcePath: Object.freeze([file.name]),
        artifactPath: Object.freeze([resultRoot, file.name]),
        fileId: file.idText,
        containingDirectoryId: intent.syntheticRoot,
        generation: identityText(90),
        exactSize: file.expectedSize,
      }),
    ]),
    entryCount: 2n,
    fileCount: 1n,
    directoryCount: 1n,
    selectedRawBytes: file.expectedSize,
  })
}

function routeRegistry(
  routes: Partial<Omit<V2PlanExecutionRouteRegistry, 'lifecycle'>> = {},
): V2PlanExecutionRouteRegistry {
  return {
    ...routes,
    lifecycle: {
      settleExecutionAdmissionFailure: async (intent) => discardedState(intent),
      recordSettlementUnknown: async (intent) => needsAttentionState(intent),
    },
  }
}

function singleFileEvidence(file: ReturnType<typeof fileEntry>) {
  return Object.freeze({
    fileId: file.idText,
    containingDirectoryId: identityText(2),
    generation: identityText(90),
    catalogSize: file.expectedSize,
    sourcePath: Object.freeze([file.name]),
  })
}

function exactPreparationEvidence(file: ReturnType<typeof fileEntry>): ExactPreparationEvidence {
  return Object.freeze({
    generations: Object.freeze([Object.freeze({
      directoryId: identityText(2),
      generation: identityText(90),
    })]),
    entries: Object.freeze([
      Object.freeze({
        kind: 'directory' as const,
        sourcePath: Object.freeze([]),
        artifactPath: Object.freeze(['windshare']),
        directoryId: identityText(2),
        generation: identityText(90),
        role: 'result-root' as const,
      }),
      Object.freeze({
        kind: 'file' as const,
        sourcePath: Object.freeze([file.name]),
        artifactPath: Object.freeze(['windshare', file.name]),
        fileId: file.idText,
        containingDirectoryId: identityText(2),
        generation: identityText(90),
        exactSize: file.expectedSize,
      }),
    ]),
    entryCount: 2n,
    fileCount: 1n,
    directoryCount: 1n,
    selectedRawBytes: file.expectedSize,
  })
}

function directTreeExecution(intent: ReceiveIntent): DirectTreeExecution {
  return {
    planKind: 'direct-tree',
    output: testOutput(),
    directories: {
      admitDirectory: async () => { throw new Error('not used') },
      finalizeDirectory: async () => { throw new Error('not used') },
    },
    beginTerminal: () => undefined,
    pause: async () => discardedState(intent),
    settle: async () => publishedState(intent),
  }
}

function directAtomicExecution(intent: ReceiveIntent): DirectAtomicExecution {
  return {
    planKind: 'direct-atomic',
    output: testOutput(),
    pause: async () => discardedState(intent),
    settle: async () => publishedState(intent),
  }
}

function workspaceExecution(intent: ReceiveIntent): WorkspaceExecution {
  return {
    planKind: 'workspace-then-publish',
    output: testOutput(),
    pause: async () => discardedState(intent),
    settle: async () => materializationSealedState(intent),
  }
}

function portableExecution(intent: ReceiveIntent): PortableExecution {
  return {
    planKind: 'portable-handoff',
    output: testOutput(),
    pause: async () => discardedState(intent),
    settle: async () => downloadStartedState(intent),
  }
}

function publishedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return state(intent, {
    kind: 'published',
    receiptDigest: digestIdentity(70),
    cleanupState: 'clean',
  })
}

function materializationSealedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return state(intent, {
    kind: 'materialization-sealed',
    sealedMaterializationDigest: digestIdentity(71),
  })
}

function downloadStartedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return state(intent, {
    kind: 'download-started',
    attemptKind: 'portable',
    attemptId: identityText(72),
  })
}

function discardedState(
  intent: ReceiveIntent,
): Extract<ReceiveLifecycleState, { readonly kind: 'discarded' }> {
  return state(intent, {
    kind: 'discarded',
    cleanupReceiptDigest: digestIdentity(73),
  }) as Extract<ReceiveLifecycleState, { readonly kind: 'discarded' }>
}

function needsAttentionState(
  intent: ReceiveIntent,
): Extract<ReceiveLifecycleState, { kind: 'needs-attention' }> {
  return state(intent, {
    kind: 'needs-attention',
    reason: 'cleanup-unknown',
    lastVerifiedRecordDigest: digestIdentity(74),
  }) as Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
}

function state(
  intent: ReceiveIntent,
  payload: Record<string, unknown>,
): ReceiveLifecycleState {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 1n,
    ...payload,
  }) as unknown as ReceiveLifecycleState
}

function asDirectTree(intent: ReceiveIntent): Parameters<
  Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openDirectTree']
>[0] {
  return intent as Parameters<
    Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openDirectTree']
  >[0]
}

function asDirectAtomic(intent: ReceiveIntent): Parameters<
  Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openDirectAtomic']
>[0] {
  return intent as Parameters<
    Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openDirectAtomic']
  >[0]
}

function asWorkspaceOriginal(intent: ReceiveIntent): Parameters<
  Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openWorkspaceOriginal']
>[0] {
  return intent as Parameters<
    Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['openWorkspaceOriginal']
  >[0]
}

function asWorkspaceZip(intent: ReceiveIntent): Parameters<
  Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['prepareWorkspaceZip']
>[0] {
  return intent as Parameters<
    Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['prepareWorkspaceZip']
  >[0]
}

function asPortable(intent: ReceiveIntent): Parameters<
  Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['preparePortable']
>[0] {
  return intent as Parameters<
    Awaited<ReturnType<typeof createV2PlanExecutionAuthority>>['preparePortable']
  >[0]
}
