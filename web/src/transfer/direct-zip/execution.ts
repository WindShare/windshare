import type { DirectZipDiagnosticsObserver } from '../../output/direct-zip/diagnostics'
import {
  DirectZipEpochWriterV1,
  type DirectZipAutomaticEpochBudgetV1,
  type DirectZipTargetVerificationPort,
  type DirectZipWriterContextV1,
  type DirectZipWriterCutSink,
  type DirectZipWriterIdentityPort,
  type DirectZipWriterPageSink,
  type DirectZipWriterCheckpointV1,
} from '../../output/direct-zip/writer'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import { decodeBase64Url, equalBytes } from '../../crypto/bytes'
import { validateReceiveIntent } from '../intent'
import type {
  DirectResumableZipExecution,
  DurabilityLevel,
  OutputSessionIdentity,
} from '../output-session'
import { DirectZipTransferDiagnosticsV1 } from './diagnostics'
import type { DirectZipIntent, DirectZipSettlementAuthorityV1 } from './model'
import {
  DirectZipTransferOutputV1,
  type DirectZipMemberRollbackAuthorityV1,
  type DirectZipReplayAuthorityV1,
} from './output-session'

export interface DirectZipRuntimeSupportV1 {
  readonly enabled: true
  readonly durability: Exclude<DurabilityLevel, 'None'>
}

export interface DirectZipWriterJournalPortV1 extends
  DirectZipWriterPageSink, DirectZipWriterCutSink {}

export class DirectZipRuntimeUnsupportedError extends Error {
  constructor() {
    super('Direct resumable ZIP is unavailable without reviewed runtime support evidence')
    this.name = 'DirectZipRuntimeUnsupportedError'
  }
}

export interface DirectZipExecutionOptionsV1 {
  readonly intent: DirectZipIntent
  readonly outputIdentity: OutputSessionIdentity
  readonly support?: DirectZipRuntimeSupportV1
  readonly writer: Readonly<{
    context: DirectZipWriterContextV1
    checkpoint: DirectZipWriterCheckpointV1
    journal: DirectZipWriterJournalPortV1
    target: DirectZipTargetVerificationPort
    identities: DirectZipWriterIdentityPort
    automaticBudget?: DirectZipAutomaticEpochBudgetV1
    cumulativePrefixCopyBytes?: bigint
  }>
  readonly replay: DirectZipReplayAuthorityV1
  readonly rollback: DirectZipMemberRollbackAuthorityV1
  readonly settlement: DirectZipSettlementAuthorityV1
  readonly diagnostics?: DirectZipDiagnosticsObserver
}

/** Browser route assembly supplies acquired target and durable journal ports; this factory never picks either. */
export async function createDirectZipExecutionV1(
  input: DirectZipExecutionOptionsV1,
): Promise<DirectResumableZipExecution> {
  if (input.support?.enabled !== true) throw new DirectZipRuntimeUnsupportedError()
  const validated = await validateReceiveIntent(input.intent)
  if (validated.plan.kind !== 'direct-resumable-zip' || validated.artifact.kind !== 'zip-archive') {
    throw new TypeError('direct ZIP execution does not match the frozen receive intent')
  }
  const intent = validated as DirectZipIntent
  if (input.writer.checkpoint.operationId !== intent.operationId) {
    throw new TypeError('direct ZIP writer checkpoint belongs to another operation')
  }
  const intentDigest = decodeBase64Url(intent.digest)
  if (intentDigest === undefined || !equalBytes(input.writer.checkpoint.intentDigest, intentDigest)) {
    throw new TypeError('direct ZIP writer checkpoint belongs to another frozen intent')
  }
  requireWriterContextBinding(intent, input.writer.context)
  const diagnostics = new DirectZipTransferDiagnosticsV1({
    operationId: intent.operationId,
    sessionId: input.outputIdentity.outputSessionId,
    ...(input.diagnostics === undefined ? {} : { observer: input.diagnostics }),
  })
  const writer = new DirectZipEpochWriterV1({
    context: input.writer.context,
    checkpoint: input.writer.checkpoint,
    pages: input.writer.journal,
    cuts: input.writer.journal,
    target: input.writer.target,
    identities: input.writer.identities,
    ...(input.writer.automaticBudget === undefined
      ? {}
      : { automaticBudget: input.writer.automaticBudget }),
    ...(input.writer.cumulativePrefixCopyBytes === undefined
      ? {}
      : { cumulativePrefixCopyBytes: input.writer.cumulativePrefixCopyBytes }),
    observe: diagnostics.writerObserver(),
  })
  const output = new DirectZipTransferOutputV1({
    identity: input.outputIdentity,
    capabilities: {
      durability: input.support.durability,
      randomWrite: false,
      fileFailureIsolation: false,
      modificationTime: true,
    },
    writer,
    pages: input.writer.journal,
    replay: input.replay,
    rollback: input.rollback,
  })
  diagnostics.session('session-started', writer.committedCheckpoint.phase)
  const execution: DirectResumableZipExecution = {
    planKind: 'direct-resumable-zip',
    output,
    ordered: output,
    pause: async (request, signal) => {
      const evidence = await output.pause()
      requireMatchingSummary(request.materialization, evidence.materialization)
      const state = await input.settlement.pause(intent, request, evidence, signal)
      requireLifecycleBinding(intent, state)
      diagnostics.session('session-paused', evidence.checkpoint.phase)
      return state
    },
    settle: async (request, signal) => {
      requireMatchingSummary(request.materialization, output.materializationSummary())
      const evidence = await output.publish()
      const state = await input.settlement.settle(intent, request, evidence, signal)
      requireLifecycleBinding(intent, state)
      diagnostics.session('session-settled', evidence.checkpoint.phase)
      return state
    },
  }
  return Object.freeze(execution)
}

function requireMatchingSummary(
  expected: ReturnType<DirectZipTransferOutputV1['materializationSummary']>,
  actual: ReturnType<DirectZipTransferOutputV1['materializationSummary']>,
): void {
  if (expected.entryCount !== actual.entryCount || expected.fileCount !== actual.fileCount ||
      expected.directoryCount !== actual.directoryCount || expected.rawBytes !== actual.rawBytes) {
    throw new TypeError('direct ZIP worker summary cannot substitute for ordered archive evidence')
  }
}

function requireLifecycleBinding(intent: DirectZipIntent, state: ReceiveLifecycleState): void {
  if (state.operationId !== intent.operationId || state.receiveIntentDigest !== intent.digest) {
    throw new TypeError('direct ZIP settlement returned lifecycle authority for another operation')
  }
}

function requireWriterContextBinding(
  intent: DirectZipIntent,
  context: DirectZipWriterContextV1,
): void {
  const operationId = decodeBase64Url(intent.operationId)
  const bindingDigest = decodeBase64Url(intent.plan.binding.digest)
  if (operationId === undefined || bindingDigest === undefined ||
      !equalBytes(context.ownershipMarker.operationId, operationId) ||
      !equalBytes(context.ownershipMarker.bindingDigest, bindingDigest) ||
      context.rootComponent !== intent.artifact.layout.name) {
    throw new TypeError('direct ZIP writer context belongs to another owned target binding')
  }
}
