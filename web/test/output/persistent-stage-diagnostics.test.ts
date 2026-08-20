import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { FileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import type {
  FileCheckpointJournal,
  FileCheckpointScan,
} from '../../src/output/persistence/journal'
import type {
  PersistentFileRequest,
  PersistentMaterializationPort,
} from '../../src/output/persistent-tree/contracts'
import {
  PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS,
  captureCheckpointFailureFacts,
  createPersistentOutputStageAuthority,
  persistentOutputStageFailureRecord,
  type PersistentOutputCheckpointFacts,
  type PersistentOutputFSAFacts,
  type PersistentOutputStage,
  type PersistentOutputStageFailureMilestone,
  type PersistentOutputStageMilestone,
} from '../../src/output/persistent-tree/stage-diagnostics'
import { FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import type { TransferTraceEvent } from '../../src/transfer/v2-job'
import { outputSessionIdentity } from '../../src/transfer/output-session'
import { createPersistentDirectTreeExecution } from '../../src/transfer/settlement/persistent-execution'
import {
  createCompleteDirectoryResultRoot,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  type DirectTreePlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  MemoryDirectory,
  MemoryLockManager,
  MemoryOperationRepository,
  bindTask,
  memoryCheckpointFactory,
  resultRootArtifact,
  singleFileArtifact,
} from './file-system-access-lifecycle-fixture'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  planAuthorityFixture,
  readerFixture,
  transferJobFixture,
} from '../transfer/v2-job-fixture'

interface StageFaultCase {
  readonly stage: PersistentOutputStage
  readonly occurrence?: number
  readonly checkpointGeneration?: bigint
  readonly entry?: 'absent' | 'file'
  readonly committedBytes?: bigint
  readonly persistedHandle?: 'absent' | 'matches-entry'
  readonly writer?: 'not-created' | 'open' | 'closed'
  readonly candidateGenerations: readonly bigint[]
  readonly committedGenerations: readonly bigint[]
}

const STAGE_FAULT_CASES: readonly StageFaultCase[] = Object.freeze([
  {
    stage: 'indexeddb.checkpoint.lineage-read',
    candidateGenerations: [],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.entry.inspect',
    entry: 'absent',
    persistedHandle: 'absent',
    candidateGenerations: [],
    committedGenerations: [],
  },
  {
    stage: 'indexeddb.checkpoint.candidate-install',
    entry: 'absent',
    persistedHandle: 'absent',
    candidateGenerations: [],
    committedGenerations: [],
  },
  {
    stage: 'indexeddb.file-handle.read',
    entry: 'absent',
    persistedHandle: 'absent',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.entry.create',
    entry: 'absent',
    persistedHandle: 'absent',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'indexeddb.file-handle.persist',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'absent',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.entry.open',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.handle.verify',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.committed-bytes.read',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'indexeddb.checkpoint.commit',
    checkpointGeneration: 0n,
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'indexeddb.checkpoint.committed-read',
    checkpointGeneration: 0n,
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [0n],
    committedGenerations: [],
  },
  {
    stage: 'fsa.file.writer.create',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'not-created',
    candidateGenerations: [],
    committedGenerations: [0n],
  },
  {
    stage: 'fsa.file.writer.write',
    entry: 'file',
    committedBytes: 0n,
    persistedHandle: 'matches-entry',
    writer: 'open',
    candidateGenerations: [],
    committedGenerations: [0n],
  },
  {
    stage: 'fsa.file.writer.close',
    entry: 'file',
    committedBytes: 2n,
    persistedHandle: 'matches-entry',
    writer: 'open',
    candidateGenerations: [],
    committedGenerations: [0n],
  },
  {
    stage: 'fsa.file.committed-bytes.read',
    occurrence: 2,
    entry: 'file',
    committedBytes: 2n,
    persistedHandle: 'matches-entry',
    writer: 'closed',
    candidateGenerations: [],
    committedGenerations: [0n],
  },
  {
    stage: 'indexeddb.checkpoint.candidate-stage',
    entry: 'file',
    committedBytes: 2n,
    persistedHandle: 'matches-entry',
    writer: 'closed',
    candidateGenerations: [],
    committedGenerations: [0n],
  },
  {
    stage: 'indexeddb.checkpoint.commit',
    checkpointGeneration: 1n,
    entry: 'file',
    committedBytes: 2n,
    persistedHandle: 'matches-entry',
    writer: 'closed',
    candidateGenerations: [1n],
    committedGenerations: [0n],
  },
  {
    stage: 'indexeddb.checkpoint.committed-read',
    checkpointGeneration: 1n,
    entry: 'file',
    committedBytes: 2n,
    persistedHandle: 'matches-entry',
    writer: 'closed',
    candidateGenerations: [],
    committedGenerations: [1n],
  },
])

interface TreeStageFaultCase {
  readonly stage: PersistentOutputStage
  readonly target: 'root' | 'directory'
  readonly artifactPathSuffix: readonly string[]
  readonly queryPermissionState?: PermissionState
}

const TREE_STAGE_FAULT_CASES: readonly TreeStageFaultCase[] = Object.freeze([
  {
    stage: 'fsa.root.permission.query',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.root.permission.request',
    target: 'root',
    artifactPathSuffix: [],
    queryPermissionState: 'prompt',
  },
  {
    stage: 'indexeddb.binding.operation.read',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'indexeddb.binding.reservation.read',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'indexeddb.binding.parent-handle.read',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.binding.parent-handle.verify',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'indexeddb.root-handle.read',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.root.entry.inspect',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.root.entry.create',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'indexeddb.root-handle.persist',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'indexeddb.root-handle.committed-read',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.root.entry.open',
    target: 'root',
    artifactPathSuffix: [],
  },
  {
    stage: 'fsa.root.handle.verify',
    target: 'root',
    artifactPathSuffix: [],
  },
  ...([
    'indexeddb.directory-handle.read',
    'fsa.directory.entry.inspect',
    'fsa.directory.entry.create',
    'indexeddb.directory-handle.persist',
    'indexeddb.directory-handle.committed-read',
    'fsa.directory.entry.open',
    'fsa.directory.handle.verify',
  ] as const).map(stage => Object.freeze({
    stage,
    target: 'directory' as const,
    artifactPathSuffix: Object.freeze(['.git', 'hooks']),
  })),
])

describe('persistent DirectTree native stage diagnostics', () => {
  it.each(STAGE_FAULT_CASES.map((faultCase, caseIndex) => ({ faultCase, caseIndex })))(
    'preserves $faultCase.stage failure and pauses before acknowledging its range',
    async ({ faultCase, caseIndex }) => {
      const outputSessionId = `stage-diagnostic-session-${caseIndex}`
      const milestones: PersistentOutputStageMilestone[] = []
      const raw = new DOMException(`injected ${faultCase.stage}`, 'UnknownError')
      let matchingOccurrence = 0
      const parent = new MemoryDirectory(`downloads-${caseIndex}`)
      const repository = new MemoryOperationRepository()
      const file = fileEntry(identity(3), 'report.bin', 2n)
      const selectionSpec = await createSelectionSpec({
        shareInstance: identityText(1),
        syntheticRoot: identityText(2),
        rules: { mode: 'node-id', defaultSelected: true, rules: [] },
      })
      const session = await bindTask({
        parent,
        repository,
        checkpointFactory: memoryCheckpointFactory(),
        locks: new MemoryLockManager(),
        artifact: await createResultRootDirectoryTreeArtifact(
          createCompleteDirectoryResultRoot(identityText(70), 'photos'),
        ),
        operationSeed: 120 + caseIndex * 4,
        selection: selectionSpec,
        stageDiagnostics: {
          outputSessionId,
          beforeStage: (stage, correlation) => {
            if (stage !== faultCase.stage ||
                (faultCase.checkpointGeneration !== undefined &&
                 correlation.checkpointGeneration !== faultCase.checkpointGeneration)) {
              return
            }
            matchingOccurrence += 1
            if (matchingOccurrence === (faultCase.occurrence ?? 1)) throw raw
          },
          observe: milestone => milestones.push(milestone),
        },
      })

      try {
        const selection = new V2SelectionPolicy(true)
        const catalog = catalogFixture([
          { id: identity(2), entries: [directoryEntry(identity(70), 'photos')] },
          { id: identity(70), entries: [file] },
        ])
        const readers = readerFixture([file])
        const plans = planAuthorityFixture()
        const lifecycleExecution = await plans.openDirectTree(
          directTreeIntent(session.intent),
          new AbortController().signal,
        )
        const persistentExecution = await createPersistentDirectTreeExecution({
          intent: directTreeIntent(session.intent),
          materialization: session,
          outputIdentity: outputSessionIdentity({
            backend: 'stage-diagnostic-test',
            outputSessionId,
          }),
          settlement: {
            pause: async (request, cut, signal) => {
              await cut.closeMaterialization()
              return lifecycleExecution.pause(request, signal)
            },
            settle: async (request, cut, signal) => {
              await cut.closeMaterialization()
              return lifecycleExecution.settle(request, signal)
            },
          },
        })
        Object.assign(plans, {
          openDirectTree: async () => persistentExecution,
        })
        const trace: TransferTraceEvent[] = []
        const result = await transferJobFixture({
          catalog: catalog.catalog,
          selection,
          intent: session.intent,
          plans,
          revisions: readers.revisions,
          broker: readers.broker,
          trace: { current: event => { trace.push(event) } },
        }).run()

        const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
          milestone.transition === 'failed' && milestone.exception.raw === raw)
        expect(failure, `missing failure milestone for ${faultCase.stage}`).toBeDefined()
        if (failure === undefined) return
        expect(failure.stage).toBe(faultCase.stage)
        expect(failure.correlation).toMatchObject({
          operationId: session.intent.operationId,
          outputSessionId,
          target: 'file',
          artifactId: file.idText,
          artifactPath: ['photos', 'report.bin'],
        })
        expect(failure.exception).toMatchObject({
          raw,
          valueType: 'object',
          constructorName: 'DOMException',
          name: 'UnknownError',
          message: `injected ${faultCase.stage}`,
        })
        expectCheckpointFacts(
          failure.facts.checkpoint,
          faultCase.candidateGenerations,
          faultCase.committedGenerations,
        )
        if (faultCase.entry !== undefined) {
          expectFSAFacts(failure.facts.fsa, faultCase)
        } else {
          expect(failure.facts.fsa).toBeUndefined()
        }

        const record = persistentOutputStageFailureRecord(failure)
        expect(record?.local.exception.raw).toBe(raw)
        expect(record?.projection).toMatchObject({
          schemaVersion: 1,
          stage: faultCase.stage,
          correlation: {
            operationId: session.intent.operationId,
            outputSessionId,
            target: 'file',
            artifactId: file.idText,
            artifactPath: ['photos', 'report.bin'],
          },
          exception: {
            valueType: 'object',
            constructorName: 'DOMException',
            name: 'UnknownError',
          },
        })
        expect('raw' in (record?.projection.exception ?? {})).toBe(false)

        expect(result.worker.status).toBe('Paused')
        expect(result.failureTrigger?.fault).toEqual(
          outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
        )
        expect(plans.pauses).toEqual(['direct-tree'])
        const progress = trace.filter((event): event is Extract<
          TransferTraceEvent,
          { readonly name: 'transfer_progress' }
        > => event.name === 'transfer_progress')
        expect(progress.length).toBeGreaterThan(0)
        expect(progress.every(event => event.writtenBytes === 0n)).toBe(true)
      } finally {
        await session.close().catch(() => undefined)
      }
    },
    20_000,
  )

  it.each(TREE_STAGE_FAULT_CASES.map((faultCase, caseIndex) => ({ faultCase, caseIndex })))(
    'correlates $faultCase.stage to its exact root or .git/hooks operation before any range acknowledgement',
    async ({ faultCase, caseIndex }) => {
      await expectTreeStageFailure(faultCase, caseIndex)
    },
    20_000,
  )

  it('keeps rejected fact providers and throwing observers advisory to the raw failure', async () => {
    const milestones: PersistentOutputStageMilestone[] = []
    const raw = new DOMException('native failure', 'UnknownError')
    const probeFailure = new Error('probe failed')
    const observerFailure = new Error('observer failed')
    const authority = createPersistentOutputStageAuthority({
      outputSessionId: 'advisory-observer-session',
      observe: milestone => {
        milestones.push(milestone)
        throw observerFailure
      },
    }, {
      operationId: 'advisory-observer-operation',
      artifactId: 'advisory-observer-artifact',
    })
    const scope = authority?.fileScope('advisory-file', ['advisory.bin'])
    expect(scope).toBeDefined()
    if (scope === undefined) return
    scope.addFailureFacts('fsa', async () => { throw probeFailure })

    await expect(scope.run(
      'fsa.file.entry.inspect',
      async () => { throw raw },
    )).rejects.toBe(raw)

    const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
      milestone.transition === 'failed')
    expect(failure?.exception.raw).toBe(raw)
    expect(failure?.facts.probeFailures?.[0]?.raw).toBe(probeFailure)
    expect(failure?.facts.observation.unavailableProviders).toMatchObject([
      { provider: 'fsa', reason: 'rejected', exception: { raw: probeFailure } },
    ])
  })

  it('rethrows the raw failure at the named fact deadline when a provider never settles', async () => {
    const milestones: PersistentOutputStageMilestone[] = []
    const raw = new DOMException('native timeout cut', 'UnknownError')
    const authority = createPersistentOutputStageAuthority({
      outputSessionId: 'deadline-session',
      observe: milestone => milestones.push(milestone),
    }, {
      operationId: 'deadline-operation',
      artifactId: 'deadline-artifact',
    })
    const scope = authority?.fileScope('deadline-file', ['deadline.bin'])
    expect(scope).toBeDefined()
    if (scope === undefined) return
    scope.addFailureFacts('checkpoint', () => new Promise(() => undefined))

    const startedAt = performance.now()
    await expect(scope.run(
      'indexeddb.checkpoint.lineage-read',
      async () => { throw raw },
    )).rejects.toBe(raw)
    const elapsed = performance.now() - startedAt

    const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
      milestone.transition === 'failed')
    expect(elapsed).toBeGreaterThanOrEqual(
      PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.deadlineMilliseconds - 5,
    )
    expect(elapsed).toBeLessThan(1_000)
    expect(failure?.exception.raw).toBe(raw)
    expect(failure?.facts.observation).toMatchObject({
      deadlineMilliseconds: PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.deadlineMilliseconds,
      timedOut: true,
      providerCount: 1,
      completedProviderCount: 0,
      unavailableProviders: [{ provider: 'checkpoint', reason: 'timeout' }],
    })
  })

  it('bounds checkpoint pages, records, strings, and total retained bytes', async () => {
    const pageFailure = await captureBoundedCheckpointFailure(
      checkpointJournalWithOneRecordPages(8),
      'page-bound',
    )
    expect(pageFailure.facts.observation).toMatchObject({
      checkpointPagesRead: PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointPages,
      checkpointRecordsRetained: PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointPages,
    })
    expect(pageFailure.facts.observation.truncation).toContain('checkpoint-pages')

    const recordFailure = await captureBoundedCheckpointFailure(
      checkpointJournalWithOversizedPage(
        PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointRecords * 2,
      ),
      'record-bound',
    )
    expect(recordFailure.facts.observation.checkpointRecordsRetained)
      .toBeLessThanOrEqual(PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.checkpointRecords)
    expect(recordFailure.facts.observation.retainedBytes)
      .toBeLessThanOrEqual(PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.totalBytes)
    expect(recordFailure.facts.observation.truncation).toEqual(
      expect.arrayContaining(['checkpoint-records', 'string-bytes', 'total-bytes']),
    )
    const retained = observedValue(recordFailure.facts.checkpoint!.candidates)
    expect(retained).toHaveLength(
      recordFailure.facts.observation.checkpointRecordsRetained,
    )
    expect(retained.every(record =>
      new TextEncoder().encode(record.recordId).byteLength <=
        PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.stringBytes &&
      new TextEncoder().encode(record.checksum).byteLength <=
        PERSISTENT_OUTPUT_FAILURE_FACT_LIMITS.stringBytes)).toBe(true)
  })

  it('retires completed file evidence instead of retaining every browser file object', async () => {
    const milestones: PersistentOutputStageMilestone[] = []
    const raw = new DOMException('inspect active evidence', 'UnknownError')
    let armed = false
    const session = await bindTask({
      parent: new MemoryDirectory('many-file-evidence'),
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await resultRootArtifact(),
      operationSeed: 248,
      stageDiagnostics: {
        outputSessionId: 'many-file-evidence-session',
        beforeStage: stage => {
          if (armed && stage === 'indexeddb.checkpoint.lineage-read') throw raw
        },
        observe: milestone => milestones.push(milestone),
      },
    })

    try {
      for (let index = 0; index < 24; index += 1) {
        const transaction = await session.beginFile({
          artifactPath: [`completed-${index}.bin`],
          openRevision: async () => ({
            fileId: identityText(100 + index),
            fileRevision: identityText(140 + index),
            exactSize: 0n,
          }),
        })
        await transaction.close()
      }

      armed = true
      await expect(session.beginFile({
        artifactPath: ['failing.bin'],
        openRevision: async () => ({
          fileId: identityText(200),
          fileRevision: identityText(201),
          exactSize: 0n,
        }),
      })).rejects.toBe(raw)

      const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
        milestone.transition === 'failed' && milestone.exception.raw === raw)
      expect(failure?.facts.observation.activeFileEvidenceCount).toBe(1)
    } finally {
      await session.close().catch(() => undefined)
    }
  })

  it('does not invoke fault or fact ports when stage diagnostics are absent', async () => {
    const parent = new MemoryDirectory('ordinary-success')
    const session = await bindTask({
      parent,
      repository: new MemoryOperationRepository(),
      checkpointFactory: memoryCheckpointFactory(),
      locks: new MemoryLockManager(),
      artifact: await singleFileArtifact(),
      operationSeed: 220,
    })

    const transaction = await session.beginFile({
      artifactPath: [session.reservation.reservedName],
      openRevision: async () => ({
        fileId: identityText(3),
        fileRevision: identityText(4),
        exactSize: 2n,
      }),
    })
    await transaction.writeRange(0n, Uint8Array.of(1, 2))
    await transaction.checkpoint()
    await transaction.close()
    await session.close()

    expect(await parent.fileBytes(session.reservation.reservedName)).toEqual(Uint8Array.of(1, 2))
  })
})

async function expectTreeStageFailure(
  faultCase: TreeStageFaultCase,
  caseIndex: number,
): Promise<void> {
  const milestones: PersistentOutputStageMilestone[] = []
  const raw = new DOMException(`injected ${faultCase.stage}`, 'UnknownError')
  const outputSessionId = `tree-stage-session-${caseIndex}`
  const parent = new MemoryDirectory(`tree-stage-parent-${caseIndex}`)
  parent.queryPermissionState = faultCase.queryPermissionState ?? 'granted'
  let armed = false
  const selectionSpec = await createSelectionSpec({
    shareInstance: identityText(1),
    syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const session = await bindTask({
    parent,
    repository: new MemoryOperationRepository(),
    checkpointFactory: memoryCheckpointFactory(),
    locks: new MemoryLockManager(),
    artifact: await resultRootArtifact(),
    operationSeed: 24 + caseIndex * 3,
    selection: selectionSpec,
    activate: false,
    stageDiagnostics: {
      outputSessionId,
      beforeStage: (stage, correlation) => {
        if (!armed || stage !== faultCase.stage || correlation.target !== faultCase.target ||
            !pathHasSuffix(correlation.artifactPath, faultCase.artifactPathSuffix)) {
          return
        }
        throw raw
      },
      observe: milestone => milestones.push(milestone),
    },
  })
  armed = true

  try {
    const selection = new V2SelectionPolicy(true)
    const file = fileEntry(identity(73), 'report.bin', 2n)
    const catalog = catalogFixture([
      { id: identity(2), entries: [directoryEntry(identity(70), 'photos')] },
      { id: identity(70), entries: [directoryEntry(identity(71), '.git')] },
      { id: identity(71), entries: [directoryEntry(identity(72), 'hooks')] },
      { id: identity(72), entries: [file] },
    ])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture()
    const lifecycleExecution = await plans.openDirectTree(
      directTreeIntent(session.intent),
      new AbortController().signal,
    )
    const materialization: PersistentMaterializationPort = Object.freeze({
      beginFile: async (request: PersistentFileRequest) => {
        await session.activate()
        return session.beginFile(request)
      },
      ensureDirectory: async (path: readonly string[]) => {
        await session.activate()
        return session.ensureDirectory(path)
      },
      close: () => session.close(),
    })
    const persistentExecution = await createPersistentDirectTreeExecution({
      intent: directTreeIntent(session.intent),
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'tree-stage-diagnostic-test',
        outputSessionId,
      }),
      settlement: {
        pause: async (request, cut, signal) => {
          await cut.closeMaterialization()
          return lifecycleExecution.pause(request, signal)
        },
        settle: async (request, cut, signal) => {
          await cut.closeMaterialization()
          return lifecycleExecution.settle(request, signal)
        },
      },
    })
    Object.assign(plans, { openDirectTree: async () => persistentExecution })
    const trace: TransferTraceEvent[] = []
    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent: session.intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      trace: { current: event => { trace.push(event) } },
    }).run()

    const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
      milestone.transition === 'failed' && milestone.exception.raw === raw)
    expect(failure, `missing failure milestone for ${faultCase.stage}`).toBeDefined()
    if (failure === undefined) return
    expect(failure.stage).toBe(faultCase.stage)
    expect(failure.correlation).toMatchObject({
      operationId: session.intent.operationId,
      outputSessionId,
      target: faultCase.target,
      artifactId: session.intent.artifact.digest,
    })
    expect(failure.correlation.artifactPath).toEqual(
      faultCase.target === 'root'
        ? []
        : ['photos', '.git', 'hooks'],
    )
    expect(failure.exception.raw).toBe(raw)
    expect(failure.facts.fsa).toBeDefined()
    expect(failure.facts.observation.providerCount).toBeGreaterThan(0)

    expect(result.worker.status).toBe('Paused')
    expect(result.failureTrigger?.fault).toEqual(
      outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
    )
    expect(plans.pauses).toEqual(['direct-tree'])
    const progress = trace.filter((event): event is Extract<
      TransferTraceEvent,
      { readonly name: 'transfer_progress' }
    > => event.name === 'transfer_progress')
    expect(progress.length).toBeGreaterThan(0)
    expect(progress.every(event => event.writtenBytes === 0n)).toBe(true)
  } finally {
    await session.close().catch(() => undefined)
  }
}

async function captureBoundedCheckpointFailure(
  checkpoints: FileCheckpointJournal,
  name: string,
): Promise<PersistentOutputStageFailureMilestone> {
  const milestones: PersistentOutputStageMilestone[] = []
  const raw = new DOMException(`${name} native failure`, 'UnknownError')
  const authority = createPersistentOutputStageAuthority({
    outputSessionId: `${name}-session`,
    observe: milestone => milestones.push(milestone),
  }, {
    operationId: `${name}-operation`,
    artifactId: `${name}-artifact`,
  })
  const scope = authority?.fileScope(`${name}-file`, [`${name}.bin`])
  if (scope === undefined) throw new Error('diagnostic authority was not created')
  scope.addFailureFacts(
    'checkpoint',
    context => captureCheckpointFailureFacts(checkpoints, `${name}-file`, context),
  )
  await expect(scope.run(
    'indexeddb.checkpoint.lineage-read',
    async () => { throw raw },
  )).rejects.toBe(raw)
  const failure = milestones.find((milestone): milestone is PersistentOutputStageFailureMilestone =>
    milestone.transition === 'failed' && milestone.exception.raw === raw)
  if (failure === undefined) throw new Error('bounded checkpoint failure milestone is missing')
  return failure
}

function checkpointJournalWithOneRecordPages(pageCount: number): FileCheckpointJournal {
  return {
    scanCandidates: async (request: FileCheckpointScan) => {
      const page = request.cursor === undefined ? 0 : Number(request.cursor)
      return Object.freeze({
        records: Object.freeze([diagnosticCheckpointRecord(page, false)]),
        ...(page + 1 >= pageCount ? {} : { nextCursor: String(page + 1) }),
      })
    },
    scanCommitted: async () => Object.freeze({ records: Object.freeze([]) }),
  } as unknown as FileCheckpointJournal
}

function checkpointJournalWithOversizedPage(recordCount: number): FileCheckpointJournal {
  const records = Object.freeze(Array.from(
    { length: recordCount },
    (_, index) => diagnosticCheckpointRecord(index, true),
  ))
  return {
    scanCandidates: async () => Object.freeze({
      records,
      nextCursor: 'another-page',
    }),
    scanCommitted: async () => Object.freeze({ records: Object.freeze([]) }),
  } as unknown as FileCheckpointJournal
}

function diagnosticCheckpointRecord(index: number, oversized: boolean): FileCheckpointV2 {
  const suffix = oversized ? 'x'.repeat(2_048) : String(index)
  return {
    recordId: `record-${index}-${suffix}`,
    checkpointGeneration: BigInt(index),
    commitState: 1,
    checksum: `checksum-${index}-${suffix}`,
    verifiedRanges: Object.freeze([{ start: 0n, end: BigInt(index) }]),
  } as unknown as FileCheckpointV2
}

function pathHasSuffix(path: readonly string[], suffix: readonly string[]): boolean {
  return suffix.length <= path.length &&
    suffix.every((segment, index) => path[path.length - suffix.length + index] === segment)
}

function expectFSAFacts(
  facts: PersistentOutputFSAFacts | undefined,
  expected: StageFaultCase,
): void {
  expect(facts).toBeDefined()
  if (facts === undefined) return
  expect(observedValue(facts.entry)).toBe(expected.entry)
  if (expected.committedBytes !== undefined) {
    expect(facts.committedBytes).toBeDefined()
    if (facts.committedBytes !== undefined) {
      expect(observedValue(facts.committedBytes)).toBe(expected.committedBytes)
    }
  }
  if (expected.persistedHandle !== undefined) {
    expect(observedValue(facts.persistedHandle)).toBe(expected.persistedHandle)
  }
  if (expected.writer !== undefined) {
    expect(facts.writer?.state).toBe(expected.writer)
  }
  expect(observedValue(facts.permissions.read)).toMatch(/granted|unsupported/)
  expect(observedValue(facts.permissions.readwrite)).toMatch(/granted|unsupported/)
}

function expectCheckpointFacts(
  facts: PersistentOutputCheckpointFacts | undefined,
  candidateGenerations: readonly bigint[],
  committedGenerations: readonly bigint[],
): void {
  expect(facts).toBeDefined()
  if (facts === undefined) return
  expect(observedValue(facts.candidates).map(record => record.checkpointGeneration))
    .toEqual(candidateGenerations)
  expect(observedValue(facts.committed).map(record => record.checkpointGeneration))
    .toEqual(committedGenerations)
}

function observedValue<Value>(
  fact: { readonly status: 'observed'; readonly value: Value } |
    { readonly status: 'unavailable' },
): Value {
  expect(fact.status).toBe('observed')
  if (fact.status !== 'observed') throw new Error('diagnostic fact was unavailable')
  return fact.value
}

function directTreeIntent(
  intent: ReceiveIntent,
): ReceiveIntent & Readonly<{ readonly plan: DirectTreePlan }> {
  if (intent.plan.kind !== 'direct-tree') throw new Error('fixture intent is not DirectTree')
  return intent as ReceiveIntent & Readonly<{ readonly plan: DirectTreePlan }>
}
