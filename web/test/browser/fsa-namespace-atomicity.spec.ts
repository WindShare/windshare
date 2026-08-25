import { expect, test, type Page } from '@playwright/test'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { requireOriginPrivateStorage } from './browser-storage-support'
import type { FsaNamespaceFixture } from './fsa-namespace-atomicity-harness'

const HARNESS_PATH = '/test/browser/fsa-namespace-atomicity-harness.ts'
const INDEXEDDB_DATABASE_PATH = '/src/output/browser/indexeddb-database.ts'
const INDEXEDDB_LEDGER_PATH = '/src/output/browser/indexeddb-compatible-name-ledger.ts'
const INDEXEDDB_REPOSITORY_PATH = '/src/output/browser/indexeddb-repository.ts'
const COMPATIBLE_NAME_MODEL_PATH = '/src/output/file-system-access/compatible-name/model.ts'
const COMPATIBLE_NAME_RESOLVER_PATH = '/src/output/file-system-access/compatible-name/resolver.ts'
const WORKSPACE_RECORDS_PATH = '/src/output/workspace/records.ts'

interface NamespaceHarness {
  exerciseNativeMutationScheduling(fixture: FsaNamespaceFixture): Promise<{
    sameParentMutationStartedBeforeWriterClose: boolean
    independentParentCreatedWhileSameParentDrained: boolean
    laterWriterAdmittedDuringMutation: boolean
    eventOrder: readonly string[]
    sameParentFileBytes: readonly number[]
    independentParentFileBytes: readonly number[]
    peakActiveWriters: number
  }>
  exerciseNativeWriterFailureRelease(fixture: FsaNamespaceFixture): Promise<{
    closeFailureObserved: boolean
    abortFailureObserved: boolean
    successorBytes: readonly number[]
    acquiredWriterLeases: number
    releasedWriterLeases: number
  }>
  holdNativeRootThroughWriterDrain(fixture: FsaNamespaceFixture): Promise<void>
  beginHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<{
    schedulerState: string
    lateWriterRejection: string
    lateNamespaceRejection: string
    rootReleasePending: boolean
  }>
  probeCompetingNativeRoot(fixture: FsaNamespaceFixture): Promise<{
    busy: boolean
    scope: string | null
  }>
  finishHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void>
  cleanupHeldNativeRootDrain(fixture: FsaNamespaceFixture): Promise<void>
  exerciseTaskRootRestart(fixture: FsaNamespaceFixture): Promise<{
    firstCollisionIndex: number
    suffixPersisted: boolean
    rootIdentityPersisted: boolean
    newTaskIsolated: boolean
    directoryCount: number
  }>
  exerciseSingleFileLayout(fixture: FsaNamespaceFixture): Promise<{
    emptyBeforeRevision: boolean
    revisionOpenedBeforeCreation: boolean
    noExtraRoot: boolean
    prefixVisible: boolean
    restartReusedFile: boolean
    completedBytes: number
  }>
  holdTaskRoot(fixture: FsaNamespaceFixture): Promise<void>
  probeCompetingTask(fixture: FsaNamespaceFixture): Promise<{
    busy: boolean
    scope: string | null
  }>
  releaseHeldTask(fixture: FsaNamespaceFixture): Promise<void>
  exerciseFailedPreExecutionActivation(fixture: FsaNamespaceFixture): Promise<{
    rootAbsentBeforeExecution: boolean
    rootAbsentAfterDetach: boolean
  }>
  exerciseCompatibleNameScenarioClosure(fixture: FsaNamespaceFixture): Promise<{
    refusals: readonly {
      scope: 'file' | 'directory' | 'result-root'
      expectedKind: 'file' | 'directory'
      rejectionCallCount: number
      rejectedEntriesBefore: readonly string[]
      contentBlockRequestCountAtRefusal: number
      logicalEntryAbsent: boolean
      repairActive: boolean
      pairReadyBeforeTarget: boolean
      sidecarCommittedCountBeforeTarget: number
      sidecarCommittedCountAfterCommit: number
      committedMappingKinds: readonly ('file' | 'directory')[]
      rootCausePreserved: boolean | null
    }[]
    ordinary: {
      repairFactoryCalls: number
      ledgerOpens: number
      repairProjectionPresent: boolean
      projectionActivationCount: number
      unexpectedLookupCount: number
      outputNames: readonly string[]
      bothRevisionsAdmittedBeforeRelease: boolean
    }
  }>
}

test('native FSA scheduler drains same-parent writers without blocking independent parents', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseNativeMutationScheduling',
    testFixture('native-scheduler'),
  )).toEqual({
    sameParentMutationStartedBeforeWriterClose: false,
    independentParentCreatedWhileSameParentDrained: true,
    laterWriterAdmittedDuringMutation: false,
    eventOrder: [
      'independent-parent-mutation',
      'same-parent-mutation',
      'later-writer',
    ],
    sameParentFileBytes: [4, 5],
    independentParentFileBytes: [8, 9],
    peakActiveWriters: 1,
  })
})

test('native writable close and abort failures release scheduler capacity', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseNativeWriterFailureRelease',
    testFixture('native-writer-failure'),
  )).toEqual({
    closeFailureObserved: true,
    abortFailureObserved: true,
    successorBytes: [21, 22, 23],
    acquiredWriterLeases: 3,
    releasedWriterLeases: 3,
  })
})

test('FSA parent Web Lock remains held through writer drain and rejects late admission', async ({
  browserName,
  context,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto('/')
  await requireOriginPrivateStorage(competitor, browserName)
  const fixture = testFixture('native-cross-tab-drain')
  try {
    await callHarness(page, 'holdNativeRootThroughWriterDrain', fixture)
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: true,
      scope: 'fsa-parent',
    })
    expect(await callHarness(page, 'beginHeldNativeRootDrain', fixture)).toEqual({
      schedulerState: 'draining',
      lateWriterRejection: 'InvalidStateError',
      lateNamespaceRejection: 'InvalidStateError',
      rootReleasePending: true,
    })
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: true,
      scope: 'fsa-parent',
    })

    await callHarness(page, 'finishHeldNativeRootDrain', fixture)
    expect(await callHarness(competitor, 'probeCompetingNativeRoot', fixture)).toEqual({
      busy: false,
      scope: null,
    })
  } finally {
    await callHarness(page, 'cleanupHeldNativeRootDrain', fixture).catch(() => undefined)
    await competitor.close()
  }
})

test('FSA task roots keep suffix and ownership across restart', async ({ browserName, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('task-root')
  expect(await callHarness(page, 'exerciseTaskRootRestart', fixture)).toEqual({
    firstCollisionIndex: 1,
    suffixPersisted: true,
    rootIdentityPersisted: true,
    newTaskIsolated: true,
    directoryCount: 3,
  })
})

test('single-file DirectoryTree has no extra root and resumes its visible prefix', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const fixture = testFixture('single-file')
  expect(await callHarness(page, 'exerciseSingleFileLayout', fixture)).toEqual({
    emptyBeforeRevision: true,
    revisionOpenedBeforeCreation: true,
    noExtraRoot: true,
    prefixVisible: true,
    restartReusedFile: true,
    completedBytes: 4,
  })
})

test('FSA parent Web Lock spans tabs', async ({ browserName, context, page }) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const competitor = await context.newPage()
  await competitor.goto('/')
  await requireOriginPrivateStorage(competitor, browserName)
  const fixture = testFixture('cross-tab')
  await callHarness(page, 'holdTaskRoot', fixture)
  expect(await callHarness(competitor, 'probeCompetingTask', fixture)).toEqual({
    busy: true,
    scope: 'fsa-parent',
  })
  await callHarness(page, 'releaseHeldTask', fixture)
  await competitor.close()
})

test('failed activation before DirectTree execution leaves no visible task root', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  expect(await callHarness(
    page,
    'exerciseFailedPreExecutionActivation',
    testFixture('pre-execution-failure'),
  )).toEqual({ rootAbsentBeforeExecution: true, rootAbsentAfterDetach: true })
})

test('compatible-name repair is exact-call-locus, pair-first, and dormant for ordinary output', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const proof = await callHarness<Awaited<ReturnType<
    NamespaceHarness['exerciseCompatibleNameScenarioClosure']
  >>>(page, 'exerciseCompatibleNameScenarioClosure', testFixture('compatible-scenarios'))
  expect(proof).toEqual({
    refusals: [
      {
        scope: 'file',
        expectedKind: 'file',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        repairActive: true,
        pairReadyBeforeTarget: true,
        sidecarCommittedCountBeforeTarget: 0,
        sidecarCommittedCountAfterCommit: 1,
        committedMappingKinds: ['file'],
        rootCausePreserved: null,
      },
      {
        scope: 'directory',
        expectedKind: 'directory',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        repairActive: true,
        pairReadyBeforeTarget: true,
        sidecarCommittedCountBeforeTarget: 0,
        sidecarCommittedCountAfterCommit: 1,
        committedMappingKinds: ['directory'],
        rootCausePreserved: null,
      },
      {
        scope: 'result-root',
        expectedKind: 'directory',
        rejectionCallCount: 1,
        rejectedEntriesBefore: [],
        contentBlockRequestCountAtRefusal: 0,
        logicalEntryAbsent: true,
        repairActive: true,
        pairReadyBeforeTarget: true,
        sidecarCommittedCountBeforeTarget: 0,
        sidecarCommittedCountAfterCommit: 1,
        committedMappingKinds: ['directory'],
        rootCausePreserved: true,
      },
    ],
    ordinary: {
      repairFactoryCalls: 0,
      ledgerOpens: 0,
      repairProjectionPresent: false,
      projectionActivationCount: 0,
      unexpectedLookupCount: 0,
      outputNames: ['ordinary-a.bin', 'ordinary-b.bin'],
      bothRevisionsAdmittedBeforeRelease: true,
    },
  })
})

test('compatible-name ledger keeps pair ownership and contiguous commits across reopen', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const operationId = identity(16, 31)
  const atomicOperationId = identity(16, 41)
  const databaseName = `fsa-compatible-ledger-${crypto.randomUUID()}`
  const result = await page.evaluate(async ({
    databaseName: name,
    operationId: operation,
    atomicOperationId: atomicOperation,
    authorityRef,
    pairScriptObjectId,
    pairSidecarObjectId,
    directoryObjectId,
    fileObjectId,
    leaseId,
    replacementLeaseId,
    receiveIntentDigest,
    receiptDigest,
    modulePaths,
  }) => {
    const { IndexedDbCompatibleNameLedger } = await import(modulePaths.ledger) as
      typeof import('../../src/output/browser/indexeddb-compatible-name-ledger')
    const { IndexedDbReceiveOperationRepository } = await import(modulePaths.repository) as
      typeof import('../../src/output/browser/indexeddb-repository')
    const model = await import(modulePaths.model) as
      typeof import('../../src/output/file-system-access/compatible-name/model')
    const records = await import(modulePaths.records) as
      typeof import('../../src/output/workspace/records')
    const { PhysicalPathResolver } = await import(modulePaths.resolver) as
      typeof import('../../src/output/file-system-access/compatible-name/resolver')
    const root = await navigator.storage.getDirectory()
    const pairDirectoryName = `pair-${operation}`
    const pairDirectory = await root.getDirectoryHandle(pairDirectoryName, { create: true })
    const scriptHandle = await pairDirectory.getFileHandle('restore.windshare-aaaaaa.ps1', {
      create: true,
    })
    const sidecarHandle = await pairDirectory.getFileHandle('restore.windshare-aaaaaa.data', {
      create: true,
    })
    const headerInput = (targetOperation: string) => ({
      operationId: targetOperation,
      primaryToken: 'aaaaaa',
      authorityRef,
      root: { logicalName: 'root', physicalName: 'root' },
      templateId: 'windows-powershell-v1',
      pairPlacement: 'inside-logical-root' as const,
      pair: {
        script: {
          physicalName: 'restore.windshare-aaaaaa.ps1',
          handleId: `script-${targetOperation}`,
          ownedObjectId: pairScriptObjectId,
          ownershipState: 'claimed' as const,
        },
        sidecar: {
          physicalName: 'restore.windshare-aaaaaa.data',
          handleId: `sidecar-${targetOperation}`,
          ownedObjectId: pairSidecarObjectId,
          ownershipState: 'claimed' as const,
        },
      },
      activationState: 'prepared' as const,
    })
    const bootstrapFor = (targetOperation: string) => model.compatibleNameOperationBootstrapV1({
      header: model.compatibleNameOperationHeaderV1(headerInput(targetOperation)),
      initialMapping: model.compatibleNameMappingV1({
        operationId: targetOperation,
        logicalPath: ['root', 'rejected-directory'],
        entryKind: 'directory',
        physicalComponent: 'rejected-directory.windshare-aaaaaa',
        attempt: 0,
        token: 'aaaaaa',
        ownershipState: 'selected',
        commitState: 'uncommitted',
      }),
    })

    const ledger = await IndexedDbCompatibleNameLedger.open(name)
    await ledger.bootstrapOperation(bootstrapFor(operation))
    await ledger.recordPairOwnership({ operationId: operation, pairKind: 'script', handle: scriptHandle })
    await ledger.recordPairOwnership({
      operationId: operation,
      pairKind: 'sidecar',
      handle: sidecarHandle,
    })
    await ledger.recordCompatibleTargetCreated({
      operationId: operation,
      logicalPath: ['root', 'rejected-directory'],
      entryKind: 'directory',
      repairSummary: model.compatibleNameRepairSummary({
        committedCount: 0,
        logicalPathSample: [],
        pairDisplayNames: {
          script: 'restore.windshare-aaaaaa.ps1',
          sidecar: 'restore.windshare-aaaaaa.data',
        },
        placement: 'inside-logical-root',
        runCommand: 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File restore.windshare-aaaaaa.ps1 -SidecarPath restore.windshare-aaaaaa.data',
        pendingCatchUp: false,
      }),
    })
    await ledger.recordVerifiedDirectoryOwnership({
      operationId: operation,
      logicalPath: ['root', 'rejected-directory'],
      entryKind: 'directory',
      ownedObjectId: directoryObjectId,
    })
    const directory = await ledger.commitMapping({
      operationId: operation,
      logicalPath: ['root', 'rejected-directory'],
      entryKind: 'directory',
      ownedObjectId: directoryObjectId,
    })
    await ledger.claimMapping({
      operationId: operation,
      logicalPath: ['root', 'rejected-file.cfg'],
      entryKind: 'file',
      physicalComponent: 'rejected-file.windshare-aaaaaa',
      attempt: 0,
      token: 'aaaaaa',
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    const file = await ledger.commitMapping({
      operationId: operation,
      logicalPath: ['root', 'rejected-file.cfg'],
      entryKind: 'file',
      ownedObjectId: fileObjectId,
    })
    const committed = await ledger.scanCommittedMappings(operation)
    const summaryInput = (state: 'active' | 'completed', pendingCatchUp: boolean) => ({
      committedCount: 2,
      logicalPathSample: [
        ['root', 'rejected-directory'],
        ['root', 'rejected-file.cfg'],
      ],
      pairDisplayNames: {
        script: 'restore.windshare-aaaaaa.ps1',
        sidecar: 'restore.windshare-aaaaaa.data',
      },
      placement: 'inside-logical-root' as const,
      runCommand: 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File restore.windshare-aaaaaa.ps1 -SidecarPath restore.windshare-aaaaaa.data',
      latestObservedFooter: { committedCount: 2, state },
      pendingCatchUp,
    })
    await ledger.persistRepairSummary(
      operation,
      model.compatibleNameRepairSummary(summaryInput('active', true)),
    )
    await ledger.persistPendingTerminalOutcome({
      operationId: operation,
      outcome: model.compatibleNamePendingTerminalOutcomeV1({
        formatVersion: model.COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION,
        footerState: 'completed',
        ordinaryLifecycle: {
          kind: 'partial-directory',
          operationId: operation,
          receiveIntentDigest,
          generation: 1n,
          reason: 'failures',
          successCount: 2n,
          failureCount: 1n,
          receiptDigest,
        },
        terminalReceipt: {
          id: records.operationRecordId(operation, records.RECEIVE_RECORD_RECEIPT, receiptDigest),
          schemaVersion: records.RECEIVE_OPERATION_SCHEMA_VERSION,
          operationId: operation,
          kind: records.RECEIVE_RECORD_RECEIPT,
          canonicalBytes: Uint8Array.of(1),
          digest: receiptDigest,
        },
      }),
      repairSummary: model.compatibleNameRepairSummary(summaryInput('active', true)),
    })
    const pendingWasReadable = await ledger.readPendingTerminalOutcome(operation) !== undefined
    await ledger.clearPendingTerminalOutcome({
      operationId: operation,
      repairSummary: model.compatibleNameRepairSummary(summaryInput('completed', false)),
    })
    const pendingWasCleared = await ledger.readPendingTerminalOutcome(operation) === undefined
    let terminalSummaryImmutable = false
    try {
      await ledger.persistRepairSummary(
        operation,
        model.compatibleNameRepairSummary(summaryInput('active', true)),
      )
    } catch {
      terminalSummaryImmutable = true
    }
    ledger.close()

    const reopened = await IndexedDbCompatibleNameLedger.open(name)
    const snapshot = await reopened.loadOperation(operation)
    const resolver = snapshot === undefined ? undefined : new PhysicalPathResolver(snapshot)
    reopened.close()

    const repository = await IndexedDbReceiveOperationRepository.open(name)
    const firstLease = records.receiveOperationLeaseRecord({
      operationId: atomicOperation,
      leaseId,
      acquiredAt: 1,
    })
    await repository.commitFSACompatibleNameBootstrap({
      transition: {
        operationId: atomicOperation,
        lease: { kind: 'put', record: firstLease },
      },
      bootstrap: bootstrapFor(atomicOperation),
    })
    let conflictRejected = false
    try {
      const replacement = records.receiveOperationLeaseRecord({
        operationId: atomicOperation,
        leaseId: replacementLeaseId,
        acquiredAt: 2,
      })
      await repository.commitFSACompatibleNameBootstrap({
        transition: {
          operationId: atomicOperation,
          expectedLeaseId: leaseId,
          lease: { kind: 'put', record: replacement },
        },
        bootstrap: model.compatibleNameOperationBootstrapV1({
          ...bootstrapFor(atomicOperation),
          header: model.compatibleNameOperationHeaderV1({
            ...headerInput(atomicOperation),
            templateId: 'changed-template-v1',
          }),
        }),
      })
    } catch {
      conflictRejected = true
    }
    const leaseAfterConflict = await repository.readLease(atomicOperation)
    repository.close()

    const atomicLedger = await IndexedDbCompatibleNameLedger.open(name)
    const atomicHeader = await atomicLedger.readHeader(atomicOperation)
    atomicLedger.close()
    await deleteDatabase(name)
    await root.removeEntry(pairDirectoryName, { recursive: true })
    return {
      directoryOrdinal: directory.commitOrdinal,
      fileOrdinal: file.commitOrdinal,
      scanOrdinals: committed.map(mapping => mapping.commitOrdinal),
      reopenedCount: snapshot?.mappings.length,
      reopenedPairReady: snapshot?.header.pair.script.ownershipState === 'owned' &&
        snapshot.header.pair.sidecar.ownershipState === 'owned',
      reopenedPhysicalFile: resolver?.physicalComponent(['root', 'rejected-file.cfg'], 'file'),
      logicalPathPreserved: resolver?.mapping(['root', 'rejected-file.cfg'], 'file')
        ?.logicalPath.join('/'),
      atomicHeaderPersisted: atomicHeader !== undefined,
      conflictRejected,
      leaseRolledBack: leaseAfterConflict?.leaseId === leaseId,
      pendingWasReadable,
      pendingWasCleared,
      terminalSummaryCount: snapshot?.header.repairSummary?.committedCount,
      terminalSummaryImmutable,
    }

    function deleteDatabase(targetName: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(targetName)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
        request.addEventListener('blocked', () => reject(
          new DOMException('test database deletion was blocked', 'InvalidStateError'),
        ), { once: true })
      })
    }
  }, {
    databaseName,
    operationId,
    atomicOperationId,
    authorityRef: identity(32, 32),
    pairScriptObjectId: identity(32, 33),
    pairSidecarObjectId: identity(32, 34),
    directoryObjectId: identity(32, 35),
    fileObjectId: identity(32, 36),
    leaseId: identity(16, 42),
    replacementLeaseId: identity(16, 43),
    receiveIntentDigest: identity(32, 44),
    receiptDigest: identity(32, 45),
    modulePaths: {
      ledger: INDEXEDDB_LEDGER_PATH,
      repository: INDEXEDDB_REPOSITORY_PATH,
      model: COMPATIBLE_NAME_MODEL_PATH,
      resolver: COMPATIBLE_NAME_RESOLVER_PATH,
      records: WORKSPACE_RECORDS_PATH,
    },
  })

  expect(result).toEqual({
    directoryOrdinal: 1,
    fileOrdinal: 2,
    scanOrdinals: [1, 2],
    reopenedCount: 2,
    reopenedPairReady: true,
    reopenedPhysicalFile: 'rejected-file.windshare-aaaaaa',
    logicalPathPreserved: 'root/rejected-file.cfg',
    atomicHeaderPersisted: true,
    conflictRejected: true,
    leaseRolledBack: true,
    pendingWasReadable: true,
    pendingWasCleared: true,
    terminalSummaryCount: 2,
    terminalSummaryImmutable: true,
  })
})

test('compatible-name ledger scopes mapping and pair claims to physical siblings', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const databaseName = `fsa-compatible-claims-${crypto.randomUUID()}`
  const operationId = identity(16, 46)
  const result = await page.evaluate(async ({
    databaseName: name,
    operationId: operation,
    authorityRef,
    pairScriptObjectId,
    pairSidecarObjectId,
    modulePaths,
  }) => {
    const { IndexedDbCompatibleNameLedger } = await import(modulePaths.ledger) as
      typeof import('../../src/output/browser/indexeddb-compatible-name-ledger')
    const model = await import(modulePaths.model) as
      typeof import('../../src/output/file-system-access/compatible-name/model')
    const { PhysicalPathResolver } = await import(modulePaths.resolver) as
      typeof import('../../src/output/file-system-access/compatible-name/resolver')
    const ledger = await IndexedDbCompatibleNameLedger.open(name)
    await ledger.bootstrapOperation(model.compatibleNameOperationBootstrapV1({
      header: model.compatibleNameOperationHeaderV1({
        operationId: operation,
        primaryToken: 'aaaaaa',
        authorityRef,
        root: { logicalName: 'root', physicalName: 'root' },
        templateId: 'windows-powershell-v1',
        pairPlacement: 'inside-logical-root',
        pair: {
          script: {
            physicalName: 'restore.windshare-aaaaaa.ps1',
            handleId: `script-${operation}`,
            ownedObjectId: pairScriptObjectId,
            ownershipState: 'claimed',
          },
          sidecar: {
            physicalName: 'restore.windshare-aaaaaa.data',
            handleId: `sidecar-${operation}`,
            ownedObjectId: pairSidecarObjectId,
            ownershipState: 'claimed',
          },
        },
        activationState: 'prepared',
      }),
      initialMapping: model.compatibleNameMappingV1({
        operationId: operation,
        logicalPath: ['parent-a', 'mapped'],
        entryKind: 'file',
        physicalComponent: 'shared.windshare-aaaaaa',
        attempt: 0,
        token: 'aaaaaa',
        ownershipState: 'selected',
        commitState: 'uncommitted',
      }),
    }))
    const claim = (logicalPath: readonly string[], physicalComponent: string) => ledger.claimMapping({
      operationId: operation,
      logicalPath,
      entryKind: 'file',
      physicalComponent,
      attempt: 0,
      token: 'aaaaaa',
      ownershipState: 'selected',
      commitState: 'uncommitted',
    })
    const crossParent = await claim(['parent-b', 'mapped'], 'shared.windshare-aaaaaa')
    let sameSiblingRejected = false
    try {
      await claim(['parent-a', 'conflict'], 'shared.windshare-aaaaaa')
    } catch {
      sameSiblingRejected = true
    }
    let pairSiblingRejected = false
    try {
      await claim(['pair-conflict'], 'restore.windshare-aaaaaa.ps1')
    } catch {
      pairSiblingRejected = true
    }
    const nestedPairTwin = await claim(
      ['nested', 'ordinary-pair-twin'],
      'restore.windshare-aaaaaa.ps1',
    )
    const snapshot = await ledger.loadOperation(operation)
    const resolver = snapshot === undefined ? undefined : new PhysicalPathResolver(snapshot)
    ledger.close()
    await new Promise<void>((resolve, reject) => {
      const request = indexedDB.deleteDatabase(name)
      request.addEventListener('success', () => resolve(), { once: true })
      request.addEventListener('error', () => reject(request.error), { once: true })
    })
    return {
      crossParentAttempt: crossParent.attempt,
      sameSiblingRejected,
      pairSiblingRejected,
      nestedPairTwinAttempt: nestedPairTwin.attempt,
      reopenedCount: snapshot?.mappings.length,
      unrelatedReverseChild: resolver?.physicalChild(
        ['unrelated'],
        'shared.windshare-aaaaaa',
        'file',
      ),
    }
  }, {
    databaseName,
    operationId,
    authorityRef: identity(32, 47),
    pairScriptObjectId: identity(32, 48),
    pairSidecarObjectId: identity(32, 49),
    modulePaths: {
      ledger: INDEXEDDB_LEDGER_PATH,
      model: COMPATIBLE_NAME_MODEL_PATH,
      resolver: COMPATIBLE_NAME_RESOLVER_PATH,
    },
  })

  expect(result).toEqual({
    crossParentAttempt: 0,
    sameSiblingRejected: true,
    pairSiblingRejected: true,
    nestedPairTwinAttempt: 0,
    reopenedCount: 3,
    unrelatedReverseChild: {
      kind: 'logical',
      logicalComponent: 'shared.windshare-aaaaaa',
    },
  })
})

test('IndexedDB v9 closes on versionchange and rejects blocked upgrades without leaking a connection', async ({
  browserName,
  page,
}) => {
  await page.goto('/')
  await requireOriginPrivateStorage(page, browserName)
  const result = await page.evaluate(async ({ upgradeName, blockedName, operationId, modulePaths }) => {
    const databaseModule = await import(modulePaths.database) as
      typeof import('../../src/output/browser/indexeddb-database')
    const ledgerModule = await import(modulePaths.ledger) as
      typeof import('../../src/output/browser/indexeddb-compatible-name-ledger')
    const legacyReceiveLeaseStore = 'receive-operation-v1-leases'
    const v8 = await openRaw(upgradeName, 8, (database, transaction) => {
      databaseModule.installIndexedDbV8Schema(database, transaction, 0)
    })
    const write = v8.transaction(legacyReceiveLeaseStore, 'readwrite')
    write.objectStore(legacyReceiveLeaseStore).put({
      id: `legacy-${operationId}`,
      operationId,
    })
    await transactionDone(write)
    v8.close()

    const upgraded = await ledgerModule.IndexedDbCompatibleNameLedger.open(upgradeName)
    upgraded.close()
    const reopened = await ledgerModule.IndexedDbCompatibleNameLedger.open(upgradeName)
    const rawV9 = await openRaw(upgradeName, databaseModule.CHECKPOINT_DATABASE_VERSION)
    const legacyLeaseStorePresent = rawV9.objectStoreNames.contains(legacyReceiveLeaseStore)
    const countTransaction = rawV9.transaction(
      databaseModule.INDEXEDDB_RECEIVE_LEASE_STORE,
      'readonly',
    )
    const currentLeaseCount = await requestValue<number>(
      countTransaction.objectStore(databaseModule.INDEXEDDB_RECEIVE_LEASE_STORE).count(),
    )
    await transactionDone(countTransaction)
    rawV9.close()
    const laterVersion = await openRaw(
      upgradeName,
      databaseModule.CHECKPOINT_DATABASE_VERSION + 1,
    )
    let closedAfterVersionchange = false
    try {
      await reopened.readHeader(operationId)
    } catch (error) {
      closedAfterVersionchange = error instanceof DOMException && error.name === 'InvalidStateError'
    }
    laterVersion.close()
    await deleteRaw(upgradeName)

    const blocker = await openRaw(blockedName, 8, (database, transaction) => {
      databaseModule.installIndexedDbV8Schema(database, transaction, 0)
    })
    let blockedRejected = false
    await ledgerModule.IndexedDbCompatibleNameLedger.open(blockedName).catch((error: unknown) => {
      blockedRejected = error instanceof DOMException && error.name === 'InvalidStateError'
    })
    blocker.close()
    // The rejected v9 request still completes later; its blocked flag must close that late connection
    // so a following upgrade can finish without timers or cross-tab cleanup heuristics.
    const afterBlocked = await openRaw(blockedName, databaseModule.CHECKPOINT_DATABASE_VERSION)
    afterBlocked.close()
    await deleteRaw(blockedName)
    return {
      legacyLeaseStorePresent,
      currentLeaseCount,
      closedAfterVersionchange,
      blockedRejected,
      lateConnectionClosed: true,
    }

    function openRaw(
      databaseName: string,
      version: number,
      upgrade?: (database: IDBDatabase, transaction: IDBTransaction) => void,
    ): Promise<IDBDatabase> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.open(databaseName, version)
        request.addEventListener('upgradeneeded', () => {
          if (request.transaction !== null) upgrade?.(request.result, request.transaction)
        })
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
        request.addEventListener('blocked', () => reject(
          new DOMException('raw test upgrade was blocked', 'InvalidStateError'),
        ), { once: true })
      })
    }

    function requestValue<T>(request: IDBRequest<T>): Promise<T> {
      return new Promise((resolve, reject) => {
        request.addEventListener('success', () => resolve(request.result), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
      })
    }

    function transactionDone(transaction: IDBTransaction): Promise<void> {
      return new Promise((resolve, reject) => {
        transaction.addEventListener('complete', () => resolve(), { once: true })
        transaction.addEventListener('abort', () => reject(transaction.error), { once: true })
        transaction.addEventListener('error', () => reject(transaction.error), { once: true })
      })
    }

    function deleteRaw(databaseName: string): Promise<void> {
      return new Promise((resolve, reject) => {
        const request = indexedDB.deleteDatabase(databaseName)
        request.addEventListener('success', () => resolve(), { once: true })
        request.addEventListener('error', () => reject(request.error), { once: true })
        request.addEventListener('blocked', () => reject(
          new DOMException('raw test deletion was blocked', 'InvalidStateError'),
        ), { once: true })
      })
    }
  }, {
    upgradeName: `fsa-v8-upgrade-${crypto.randomUUID()}`,
    blockedName: `fsa-v8-blocked-${crypto.randomUUID()}`,
    operationId: identity(16, 51),
    modulePaths: {
      database: INDEXEDDB_DATABASE_PATH,
      ledger: INDEXEDDB_LEDGER_PATH,
    },
  })

  expect(result).toEqual({
    legacyLeaseStorePresent: false,
    currentLeaseCount: 0,
    closedAfterVersionchange: true,
    blockedRejected: true,
    lateConnectionClosed: true,
  })
})

function testFixture(label: string): FsaNamespaceFixture {
  const nonce = crypto.randomUUID()
  return Object.freeze({
    databaseName: `fsa-${label}-${nonce}`,
    parentName: `fsa-${label}-${nonce}`,
  })
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}

async function callHarness<T>(
  page: Page,
  method: keyof NamespaceHarness,
  fixture: FsaNamespaceFixture,
): Promise<T> {
  return page.evaluate(async ({ path, operation, value }) => {
    const harness = (await import(path)) as NamespaceHarness
    return harness[operation](value) as Promise<T>
  }, { path: HARNESS_PATH, operation: method, value: fixture })
}
