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
import { fileCheckpointIsComplete } from '../persistence/checkpoint'
import { PersistentTreeOutputSession } from '../persistent-tree/session'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageDiagnostics,
} from '../persistent-tree/stage-diagnostics'
import {
  openFSAFileCheckpointRepository,
  scanAllFSAFileCheckpoints,
  type FSAFileCheckpointRepository,
  type FSAFileCheckpointRepositoryFactory,
} from './checkpoint-repository'
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
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  readonly compatibleNamePreparation?: CompatibleNameRootRepairPreparationOptions
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly stageDiagnostics?: PersistentOutputStageDiagnostics
  readonly trace?: FSAOutputTrace
}

export interface OpenFileSystemAccessPendingOutcomeCatchUpOptions {
  readonly intent: ReceiveIntent
  readonly operationRepository: FSAOperationBindingRepository
  readonly lockManager?: BrowserLockManagerRuntime
  readonly checkpointRepositoryFactory?: FSAFileCheckpointRepositoryFactory
  readonly databaseName?: string
  readonly openCompatibleNameLedger?: () => Promise<CompatibleNameActivationLedger>
  readonly compatibleNamePreparation?: CompatibleNameRootRepairPreparationOptions
}

/** Local-only authority: no catalog, revision, content, block, or sender capability is accepted. */
export interface FileSystemAccessPendingOutcomeCatchUpSession {
  readonly pendingOutcome: CompatibleNamePendingTerminalOutcomeV1
  drainTerminalProjector(): Promise<CompatibleNameRepairSummary>
  clearPendingOutcome(): Promise<void>
  retireCheckpoints(): Promise<void>
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
  let checkpoints: FSAFileCheckpointRepository | undefined
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
      ...(options.diagnostics === undefined
        ? {}
        : { diagnostics: options.diagnostics }),
      ...(stageAuthority === undefined ? {} : { stageAuthority }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
    await reconcileCommittedFileMappings(checkpoints, compatibleNames)
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

export async function openFileSystemAccessPendingOutcomeCatchUp(
  options: OpenFileSystemAccessPendingOutcomeCatchUpOptions,
): Promise<FileSystemAccessPendingOutcomeCatchUpSession> {
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
    if (!compatibleNames.active || pendingOutcome === undefined) {
      throw new DOMException('Compatible-name terminal catch-up is not pending', 'InvalidStateError')
    }
    const pairRoot = compatibleNames.pairPlacement === 'inside-logical-root'
      ? await binding.parent.getDirectoryHandle(binding.reservation.physicalName)
      : undefined
    await compatibleNames.ensurePairReady(pairRoot)
    checkpoints = await openFSAFileCheckpointRepository(options, intent, binding.reservation)
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
      drainTerminalProjector: async () => {
        const summary = await compatibleNames!.drainTerminalProjector(pendingOutcome.footerState)
        if (summary === undefined) {
          throw new DOMException('Compatible-name terminal projector is unavailable', 'InvalidStateError')
        }
        return summary
      },
      clearPendingOutcome: () => compatibleNames!.clearPendingTerminalOutcome(),
      retireCheckpoints: () => checkpoints!.retireOperation(),
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
      ? await acquireFSARootMutationLease(parent)
      : await acquireFSARootMutationLease(parent, options.lockManager)
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
  checkpoints: FSAFileCheckpointRepository,
  compatibleNames: CompatibleNamePathAuthority,
): Promise<void> {
  if (!compatibleNames.active) return
  const committed = await scanAllFSAFileCheckpoints(checkpoints, 'committed')
  for (const checkpoint of committed) {
    if (!fileCheckpointIsComplete(checkpoint)) continue
    await compatibleNames.commitFinalFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
  }
}
