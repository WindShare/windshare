import { validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import { recordOutputException, type OutputDiagnosticsPorts } from '../diagnostics'
import { BrowserFileSystemTree } from '../browser/filesystem-tree'
import { IndexedDbCompatibleNameLedger } from '../browser/indexeddb-compatible-name-ledger'
import {
  verifyFSAOperationBinding,
  type FSAOperationBindingRepository,
  type PersistedFSAOperationBinding,
} from '../browser/indexeddb-root-binding'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSARootMutationLease,
} from '../browser/namespace-mutation'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
  type FSASemanticOutputRepository,
} from './checkpoint-repository'
import { createMaterializationLedgerBinding } from '../materialization-ledger/codec'
import {
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MaterializationLedgerEntryKind,
  type MaterializationLedgerBindingV1,
} from '../materialization-ledger/model'
import {
  CompatibleNamePathAuthority,
  type CompatibleNameActivationLedger,
  type CompatibleNameRootRepairPreparationOptions,
} from './compatible-name/coordinator'
import type {
  CompatibleNamePendingTerminalOutcomeV1,
  CompatibleNameRepairSummary,
} from './compatible-name/model'
import {
  defaultCompatibleNamePreparation,
  emitFSAOutputTrace,
  needsAttention,
  outputTrace,
} from './session-diagnostics'
import {
  FileSystemAccessOutputSession,
  type FSAOutputTrace,
} from './session'

export interface ReopenFileSystemAccessOutputOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly maximumConcurrentInitialClaimInspections?: number
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  readonly compatibleNamePreparation?: CompatibleNameRootRepairPreparationOptions
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageDiagnostics?: PersistentOutputStageDiagnostics
  readonly trace?: FSAOutputTrace
}

export interface OpenFileSystemAccessCompatibleNameCatchUpOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  readonly compatibleNamePreparation?: CompatibleNameRootRepairPreparationOptions
}

/** Local-only authority: no catalog, revision, content, block, or sender capability is accepted. */
export interface FileSystemAccessCompatibleNameCatchUpSession {
  readonly pendingOutcome: CompatibleNamePendingTerminalOutcomeV1 | undefined
  synchronizeActiveProjector(): Promise<CompatibleNameRepairSummary>
  drainTerminalProjector(): Promise<CompatibleNameRepairSummary>
  clearPendingOutcome(): Promise<void>
  retireRecoveryMetadata(): Promise<void>
  runExclusive<T>(operation: () => Promise<T>): Promise<T>
  close(): Promise<void>
}

export async function reopenFileSystemAccessOutput(
  options: ReopenFileSystemAccessOutputOptions,
): Promise<FileSystemAccessOutputSession> {
  const intent = await validateReceiveIntent(options.intent)
  const stageAuthority = createPersistentOutputStageAuthority(
    options.stageDiagnostics,
    {
      operationId: intent.operationId,
      artifactId: intent.artifact.digest,
    },
  )
  let firstBinding: PersistedFSAOperationBinding
  try {
    firstBinding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      ...(stageAuthority === undefined
        ? {}
        : { stageScope: stageAuthority.bindingScope() }),
    })
  } catch (error) {
    if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
      emitFSAOutputTrace(options.trace, needsAttention(intent.operationId))
    }
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  }
  const rootLease = await acquireReopenRootLease(firstBinding.parent, options)
  let checkpoints: FSASemanticOutputRepository | undefined
  let compatibleNames: CompatibleNamePathAuthority | undefined
  let materializationOpening = false
  try {
    const binding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      expectedParent: firstBinding.parent,
      ...(stageAuthority === undefined
        ? {}
        : { stageScope: stageAuthority.bindingScope() }),
    })
    compatibleNames = await CompatibleNamePathAuthority.openForReopen({
      binding,
      mutations: rootLease.authority,
      pairHandles: options.operationRepository,
      openLedger: options.openCompatibleNameLedger ??
        (() => IndexedDbCompatibleNameLedger.open(options.databaseName)),
      preparation: options.compatibleNamePreparation ?? defaultCompatibleNamePreparation(),
    })
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
    const tree = new BrowserFileSystemTree({
      binding,
      operationRepository: options.operationRepository,
      fileHandles: checkpoints,
      mutations: rootLease.authority,
      compatibleNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
    })
    materializationOpening = true
    const materialization = await PersistentTreeOutputSession.open({
      tree,
      checkpoints,
      semantic: checkpoints,
      ...initialClaimInspectionSessionOption(options.maximumConcurrentInitialClaimInspections),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    await reconcileCommittedFileMappings(
      semanticRepository(checkpoints),
      await ledgerBinding(intent, binding),
      compatibleNames,
    )
    emitFSAOutputTrace(options.trace, Object.freeze({
      name: 'receive.reservation.reopened',
      operation_id: intent.operationId,
      receive_intent_digest: intent.digest,
      reservation_kind: 'named-container-entry',
    }))
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'authorized' })
    return new FileSystemAccessOutputSession({
      intent,
      reservation: binding.reservation,
      materialization,
      tree,
      binding,
      operationRepository: options.operationRepository,
      checkpoints,
      rootLease,
      compatibleNames,
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
    })
  } catch (error) {
    reportFSAReopenFailure(options, intent.operationId, error, materializationOpening)
    const cleanupFailures = await releaseFailedFSAOpen(
      checkpoints,
      compatibleNames,
      rootLease,
      options.diagnostics,
    )
    if (cleanupFailures.length !== 0) {
      throw new AggregateError(
        [error, ...cleanupFailures],
        'FSA output reopen failed and could not release all authorities',
        { cause: error },
      )
    }
    throw error
  }
}

function initialClaimInspectionSessionOption(
  maximumConcurrentInitialClaimInspections: number | undefined,
): Readonly<{ maximumConcurrentInitialClaimInspections?: number }> {
  return maximumConcurrentInitialClaimInspections === undefined
    ? {}
    : { maximumConcurrentInitialClaimInspections }
}

export async function openFileSystemAccessCompatibleNameCatchUp(
  options: OpenFileSystemAccessCompatibleNameCatchUpOptions,
): Promise<FileSystemAccessCompatibleNameCatchUpSession> {
  const intent = await validateReceiveIntent(options.intent)
  const firstBinding = await verifyFSAOperationBinding({
    repository: options.operationRepository,
    intent,
  })
  const rootLease = await acquireReopenRootLease(firstBinding.parent, options)
  let compatibleNames: CompatibleNamePathAuthority | undefined
  let checkpoints: FSAFileCheckpointRepository | undefined
  try {
    const binding = await verifyFSAOperationBinding({
      repository: options.operationRepository,
      intent,
      expectedParent: firstBinding.parent,
    })
    compatibleNames = await CompatibleNamePathAuthority.openForReopen({
      binding,
      mutations: rootLease.authority,
      pairHandles: options.operationRepository,
      openLedger: options.openCompatibleNameLedger ??
        (() => IndexedDbCompatibleNameLedger.open(options.databaseName)),
      preparation: options.compatibleNamePreparation ?? defaultCompatibleNamePreparation(),
    })
    const pendingOutcome = compatibleNames.pendingTerminalOutcome()
    if (!compatibleNames.active) {
      throw new DOMException('Compatible-name local catch-up is unavailable', 'InvalidStateError')
    }
    const pairRoot = compatibleNames.pairPlacement === 'inside-logical-root'
      ? await binding.parent.getDirectoryHandle(binding.reservation.physicalName)
      : undefined
    await compatibleNames.ensurePairReady(pairRoot)
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
    const semantic = semanticRepository(checkpoints)
    const materializationBinding = await ledgerBinding(intent, binding)
    const terminalDrain = rootLease.scheduler.beginTerminal('repair-operation')
    const runExclusive = async <T>(operation: () => Promise<T>): Promise<T> => {
      await terminalDrain.drained
      return terminalDrain.runExclusive(() => operation())
    }
    let closed = false
    const close = async () => {
      if (closed) return
      closed = true
      const failures: unknown[] = []
      try { checkpoints?.close() } catch (error) { failures.push(error) }
      try { compatibleNames?.close() } catch (error) { failures.push(error) }
      try { await rootLease.release() } catch (error) { failures.push(error) }
      if (failures.length === 1) throw failures[0]
      if (failures.length > 1) {
        throw new AggregateError(failures, 'FSA terminal catch-up authorities did not close cleanly')
      }
    }
    return Object.freeze({
      pendingOutcome,
      synchronizeActiveProjector: async () => {
        if (pendingOutcome !== undefined) {
          throw new DOMException('Active catch-up cannot cross a terminal outcome', 'InvalidStateError')
        }
        const summary = await compatibleNames!.synchronizeActiveProjector(
          () => reconcileCommittedFileMappings(semantic, materializationBinding, compatibleNames!),
        )
        if (summary === undefined) {
          throw new DOMException('Compatible-name active projector is unavailable', 'InvalidStateError')
        }
        return summary
      },
      drainTerminalProjector: async () => {
        if (pendingOutcome === undefined) {
          throw new DOMException('Compatible-name terminal outcome is unavailable', 'InvalidStateError')
        }
        const summary = await compatibleNames!.drainTerminalProjector(pendingOutcome.footerState)
        if (summary === undefined) {
          throw new DOMException('Compatible-name terminal projector is unavailable', 'InvalidStateError')
        }
        return summary
      },
      clearPendingOutcome: async () => {
        if (pendingOutcome === undefined) {
          throw new DOMException('Active catch-up has no terminal outcome to clear', 'InvalidStateError')
        }
        await compatibleNames!.clearPendingTerminalOutcome()
      },
      retireRecoveryMetadata: async () => {
        if (pendingOutcome === undefined) {
          throw new DOMException('Active catch-up must preserve receive metadata', 'InvalidStateError')
        }
        await retireRecoveryMetadata(semantic, materializationBinding)
      },
      runExclusive,
      close,
    })
  } catch (error) {
    const failures = await releaseFailedFSAOpen(
      checkpoints,
      compatibleNames,
      rootLease,
      undefined,
    )
    if (failures.length !== 0) {
      throw new AggregateError(
        [error, ...failures],
        'FSA terminal catch-up failed to acquire local authority cleanly',
        { cause: error },
      )
    }
    throw error
  }
}

async function acquireReopenRootLease(
  parent: FileSystemDirectoryHandle,
  options: ReopenFileSystemAccessOutputOptions,
): Promise<FSARootMutationLease> {
  try {
    return options.lockManager === undefined
      ? await acquireFSARootMutationLease(parent, undefined, undefined, options.diagnostics?.performance)
      : await acquireFSARootMutationLease(
          parent,
          options.lockManager,
          undefined,
          options.diagnostics?.performance,
        )
  } catch (error) {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
    outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
    throw error
  }
}

function reportFSAReopenFailure(
  options: ReopenFileSystemAccessOutputOptions,
  operationId: string,
  error: unknown,
  materializationOpening: boolean,
): void {
  if (error instanceof TargetOwnershipUnknownError && error.stage !== 'checkpoint') {
    emitFSAOutputTrace(options.trace, needsAttention(operationId))
  }
  if (!materializationOpening) {
    recordOutputException(options.diagnostics?.failures?.reopen, error)
  }
  outputTrace(options.diagnostics, { eventName: 'reopen', transition: 'failed' })
}

async function releaseFailedFSAOpen(
  checkpoints: FSAFileCheckpointRepository | undefined,
  compatibleNames: CompatibleNamePathAuthority | undefined,
  rootLease: FSARootMutationLease,
  diagnostics: OutputDiagnosticsPorts | undefined,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  try {
    checkpoints?.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  try {
    compatibleNames?.close()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  try {
    await rootLease.release()
  } catch (error) {
    failures.push(error)
    recordOutputException(diagnostics?.failures?.cleanup, error)
  }
  if (failures.length !== 0) {
    outputTrace(diagnostics, { eventName: 'cleanup', transition: 'failed' })
  }
  return Object.freeze(failures)
}

async function reconcileCommittedFileMappings(
  repository: FSASemanticOutputRepository,
  binding: MaterializationLedgerBindingV1,
  compatibleNames: CompatibleNamePathAuthority,
): Promise<void> {
  if (!compatibleNames.active) return
  let after: Parameters<FSASemanticOutputRepository['scanMaterializationLedgerEntries']>[1]['after']
  do {
    const page = await repository.scanMaterializationLedgerEntries(binding, {
      ...(after === undefined ? {} : { after }),
      limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    })
    for (const entry of page.entries) {
      if (entry.kind !== MaterializationLedgerEntryKind.FileFinalized) continue
      await compatibleNames.commitFinalFile(entry.relativePath, entry.ownedFileIdentity)
    }
    after = page.continuation
  } while (after !== undefined)
}

async function ledgerBinding(
  intent: ReceiveIntent,
  binding: PersistedFSAOperationBinding,
): Promise<MaterializationLedgerBindingV1> {
  return createMaterializationLedgerBinding({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    materializationBindingDigest: binding.reservation.digest,
    authorityRef: binding.reservation.authorityRef,
  })
}

function semanticRepository(
  repository: FSAFileCheckpointRepository,
): FSASemanticOutputRepository {
  const candidate = repository as Partial<FSASemanticOutputRepository>
  if (typeof candidate.scanMaterializationLedgerEntries !== 'function' ||
      typeof candidate.retireMaterializationLedgerBatch !== 'function') {
    throw new DOMException(
      'DirectTree recovery requires semantic ledger repository authority',
      'InvalidStateError',
    )
  }
  return candidate as FSASemanticOutputRepository
}

async function retireRecoveryMetadata(
  repository: FSASemanticOutputRepository,
  binding: MaterializationLedgerBindingV1,
): Promise<void> {
  for (;;) {
    const result = await repository.retireMaterializationLedgerBatch(
      binding,
      MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    )
    if (result.state === 'complete') return
    if (result.deletedRows === 0) {
      throw new DOMException('FSA recovery metadata retirement made no progress', 'OperationError')
    }
  }
}
