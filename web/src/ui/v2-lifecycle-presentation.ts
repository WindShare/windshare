import { availableReceiveActions, receiveLifecycleCategory, newReceiveOperationKind, type ReceiveActionFacts } from '../application/receive/policy'
import type { LifecycleUserAction, PersistentWriterOpenPauseFact, V2ActiveReceiveControl, ReceiveSettlementWait, WorkspaceUsage } from '../application/receive/actions'
import { artifactRequestedName } from '../output/planning'
import type {
  CompatibleNameRepairSummary,
} from '../transfer/lifecycle/name-repair'
import type {
  RecoverySummary,
} from '../transfer/lifecycle/recovery-summary'
import {
  type ReceiveLifecycleState,
} from '../output/workspace'
import type {
  ArtifactSpec,
  MaterializationPlan,
} from '../transfer/intent'
import { resumableFileSetDescription } from './resumable-file-set-presentation'
import {
  presentCompatibleNameRepair,
  type CompatibleNameRepairPresentation,
} from './compatible-name-repair-presentation'
import { formatBytes } from './v2-progress-presentation'
import type { V2DirectZipProgressSnapshot } from '../application/receive/contracts'

export const SOURCE_INVALIDATED_DESCRIPTION =
  'The source file changed and this download cannot continue. Ask the sender to share it again and open the new link. You can discard this task to remove its retained data.'

export interface WorkspaceUsagePresentation {
  readonly ownedBytes: bigint
  readonly maximumBytes?: bigint
  readonly label: string
}

export interface LifecycleActionPresentation {
  readonly kind: LifecycleUserAction
  readonly label: string
  readonly destructive: boolean
}

export interface NewReceiveOperationPresentation {
  readonly kind: 'direct-tree-to-zip' | 'deleted-direct-zip-target'
  readonly ariaLabel: string
  readonly title: string
  readonly description: string
  readonly actionLabel: string
}

export function presentNewReceiveOperation(input: Readonly<{
  lifecycle: ReceiveLifecycleState | null
  plan: MaterializationPlan | null
}>): NewReceiveOperationPresentation | null {
  const { lifecycle, plan } = input
  if (lifecycle === null || plan === null) return null
  const kind = newReceiveOperationKind(lifecycle, plan)
  if (kind === 'direct-tree-to-zip') {
    return Object.freeze({
      kind: 'direct-tree-to-zip',
      ariaLabel: 'Receive again as ZIP',
      title: 'Receive this selection as a ZIP',
      description: 'This starts a new receive operation. Files already received in the folder stay where they are, but their bytes are not reused for the ZIP.',
      actionLabel: 'Choose a ZIP route for a new operation',
    })
  }
  if (kind === 'deleted-direct-zip-target') {
    return Object.freeze({
      kind: 'deleted-direct-zip-target',
      ariaLabel: 'Start a new receive operation',
      title: 'Choose a new save route',
      description: 'The previous ZIP target is gone and will not be recreated. Start a new receive operation for the same selection; bytes from the old operation are not reused.',
      actionLabel: 'Choose a route for a new operation',
    })
  }
  return null
}

export { presentCompatibleNameRepair } from './compatible-name-repair-presentation'
export { hasValidatedTerminalCompatibleNameRepair } from '../application/receive/repair'
export type { CompatibleNameRepairPresentation, CompatibleNameRepairPresentationContext } from './compatible-name-repair-presentation'
export type { CompatibleNameRepairActionMode } from '../application/receive/repair'

export interface ReceiveLifecyclePresentation {
  readonly stateKind: ReceiveLifecycleState['kind']
  readonly category: 'active' | 'retained' | 'terminal'
  readonly title: string
  readonly description: string
  readonly tone: 'neutral' | 'positive' | 'warning' | 'critical'
  readonly usage: WorkspaceUsagePresentation | null
  readonly actions: readonly LifecycleActionPresentation[]
  readonly compatibleNameRepair: CompatibleNameRepairPresentation | null
  readonly writerOpenPause: PersistentWriterOpenPausePresentation | null
}

export interface PersistentWriterOpenPausePresentation {
  readonly materializationPath: string
  readonly title: string
  readonly description: string
}

export function presentReceiveLifecycle(input: Readonly<{
  state: ReceiveLifecycleState
  artifact: ArtifactSpec
  plan: MaterializationPlan
  workspaceUsage?: WorkspaceUsage | null
  activeControls?: readonly V2ActiveReceiveControl[]
  interruption?: ReceiveSettlementWait | null
  repairSummary?: CompatibleNameRepairSummary | null
  recoverySummary?: RecoverySummary | null
  writerOpenPause?: PersistentWriterOpenPauseFact | null
  directZipProgress?: V2DirectZipProgressSnapshot | null
  browserDelivery?: import('../output/browser-delivery/retained').BrowserDeliveryResumeSummary | null
}>): ReceiveLifecyclePresentation {
  const compatibleNameRepair = input.repairSummary === undefined || input.repairSummary === null
    ? null
    : presentCompatibleNameRepair({ state: input.state, summary: input.repairSummary })
  const copy = input.interruption === undefined || input.interruption === null
    ? lifecycleCopy(
        input.state,
        input.artifact,
        input.plan,
        input.directZipProgress ?? null,
        compatibleNameRepair,
        input.recoverySummary ?? null,
      )
    : interruptionCopy(input.interruption)
  const actions = Object.freeze(availableReceiveActions(input).map(kind => presentLifecycleAction(kind, input)))
  const writerOpenPause = presentPersistentWriterOpenPause(input)
  return Object.freeze({
    stateKind: input.state.kind,
    category: receiveLifecycleCategory(input.state),
    title: copy.title,
    description: copy.description,
    tone: copy.tone,
    usage: workspaceUsagePresentation(input),
    actions,
    compatibleNameRepair,
    writerOpenPause,
  })
}

function presentPersistentWriterOpenPause(input: Readonly<{
  state: ReceiveLifecycleState
  plan: MaterializationPlan
  writerOpenPause?: PersistentWriterOpenPauseFact | null
}>): PersistentWriterOpenPausePresentation | null {
  const fact = input.writerOpenPause
  if (fact === undefined || fact === null || input.plan.kind !== 'direct-tree' ||
      input.state.kind !== 'resumable-receive' || input.state.payloadKind !== 'file-set') return null
  const materializationPath = fact.materializationRelativePath.join('/')
  const context = fact.purpose === 'automatic-checkpoint'
    ? 'The automatic checkpoint replacement failed.'
    : 'The preserving recovery writer failed to open.'
  return Object.freeze({
    materializationPath,
    title: `Could not reopen ${materializationPath}`,
    description: `${context} The durable checkpoint is safe. Reopening this file would copy ` +
      `${formatBytes(fact.cost.prefixCopyBytes)}, add ` +
      `${formatBytes(fact.cost.writeAmplificationBytes)} of write amplification, and may use up to ` +
      `${formatBytes(fact.cost.temporaryBytes)} of temporary destination space.`,
  })
}

function interruptionCopy(
  interruption: ReceiveSettlementWait,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> {
  if (interruption.operation === 'finish') {
    return copy(
      'Finishing is taking longer than expected',
      'WindShare is waiting for current save operations to finish safely. Keep this page open; you can continue browsing.',
      'warning',
    )
  }
  if (interruption.operation === 'pause') {
    return interruption.phase === 'waiting'
      ? copy(
          'Pausing',
          'WindShare is stopping new transfers and making accepted file data safe to continue.',
          'neutral',
        )
      : copy(
          'Pausing in the background',
          'The wait was detached, but WindShare still owns the save location until native writes and recovery records finish.',
          'warning',
        )
  }
  return interruption.phase === 'waiting'
    ? copy(
        'Stopping',
        'WindShare is stopping new transfers and closing accepted output work.',
        'neutral',
      )
    : copy(
        'Stopping in the background',
        'The wait was detached, but WindShare still owns the save location until native writes and task records finish.',
        'warning',
      )
}

function lifecycleCopy(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  plan: MaterializationPlan,
  directZipProgress: V2DirectZipProgressSnapshot | null,
  compatibleNameRepair: CompatibleNameRepairPresentation | null,
  recoverySummary: RecoverySummary | null,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> {
  const override = lifecycleOverrideCopy(
    state,
    plan,
    directZipProgress,
    compatibleNameRepair,
  )
  if (override !== null) return override
  const name = browserArtifactName(artifact)
  switch (state.kind) {
    case 'intent-frozen':
      return copy('Ready to receive', 'The selected result and its save semantics are fixed.', 'neutral')
    case 'resumable-start':
      return copy(state.reason === 'paused' ? 'Paused before downloading' : 'Download could not start',
        'The selected download is retained. Try again when you are ready.', 'warning')
    case 'preparing':
      return copy(
        'Checking selected content',
        'WindShare is confirming the complete selection before requesting file content.',
        'neutral',
      )
    case 'receiving':
      return copy('Receiving files', receivingDescription(artifact, plan), 'neutral')
    case 'resumable-receive': return resumableReceiveCopy(state, plan, recoverySummary)
    case 'finalizing-tree':
      return copy(
        'Finishing the folder hierarchy',
        'WindShare is verifying every selected item and the saved folder result.',
        'neutral',
      )
    case 'committing-atomic':
      return copy(`Saving ${name}`, 'The complete result is being committed.', 'neutral')
    case 'materialization-sealed':
      return copy('Receiving complete', 'The received content is sealed and ready for final packaging.', 'neutral')
    case 'packaging':
      return artifact.kind === 'zip-archive'
        ? copy(
            'Finalizing ZIP',
            'All selected content is received. WindShare is finishing the ZIP directory locally; the sender can go offline.',
            'neutral',
          )
        : copy('Preparing the complete file', 'WindShare is sealing the file for saving.', 'neutral')
    case 'resumable-package':
      return copy(
        artifact.kind === 'zip-archive' ? 'Ready to continue generating ZIP' : 'Ready to continue preparing the file',
        'Received content was retained. Continuing does not receive completed files again.',
        'warning',
      )
    case 'artifact-sealed':
      return copy('Final result sealed', `${name} is complete and is being made ready to save.`, 'neutral')
    case 'waiting-to-save':
      return copy('Ready to save', `${name} is complete and retained in browser storage. Save it now or return later; delete the task when you no longer need its retained copy.`, 'positive')
    case 'publishing-managed':
      return copy(`Saving ${name}`, 'The complete result is being published to the chosen location.', 'neutral')
    case 'handing-off':
      return copy('Starting browser download', 'The complete result is being handed to the browser.', 'neutral')
    case 'published':
      return copy(
        'Saved',
        state.cleanupState === 'cleanup-pending'
          ? 'The complete result was saved. WindShare is finishing task cleanup.'
          : 'The complete result was saved and task cleanup is complete.',
        'positive',
      )
    case 'download-started':
      return copy(
        'Download started',
        'The browser took over the download. WindShare cannot confirm where or whether it was saved.',
        'positive',
      )
    case 'partial-directory':
      return copy(
        state.reason === 'stopped' ? 'Receiving stopped' : 'Some files were saved',
        partialDirectoryDescription(state),
        'warning',
      )
    case 'restart-required':
      return copy('Start again required', restartRequiredDescription(state.reason), 'warning')
    case 'source-invalidated':
      return copy('Source file changed', SOURCE_INVALIDATED_DESCRIPTION, 'warning')
    case 'discarded':
      return copy('Task discarded', 'Owned unfinished data and task records were removed.', 'neutral')
    case 'needs-attention':
      return copy('Needs attention', needsAttentionDescription(state.reason), 'critical')
    case 'authorization-required':
      return copy(
        'Save authorization required',
        'Choose Continue to authorize the same saved target. WindShare will not create or switch to another target.',
        'warning',
      )
    case 'target-verification-required':
      return copy(
        'Saved target must be verified',
        'WindShare needs a slower ownership check before it can safely continue or change the incomplete ZIP.',
        'warning',
      )
    case 'destination-space-required':
      return copy(
        'More destination space is required',
        'Free space at the selected destination, then retry. The last verified resume position is retained.',
        'warning',
      )
  }
}

function lifecycleOverrideCopy(
  state: ReceiveLifecycleState,
  plan: MaterializationPlan,
  directZipProgress: V2DirectZipProgressSnapshot | null,
  compatibleNameRepair: CompatibleNameRepairPresentation | null,
): ReturnType<typeof copy> | null {
  const repair = compatibleNameLifecycleCopy(state, compatibleNameRepair)
  if (repair !== null) return repair
  return state.kind === 'published' ? null : directZipProgressCopy(plan, directZipProgress)
}

function compatibleNameLifecycleCopy(
  state: ReceiveLifecycleState,
  repair: CompatibleNameRepairPresentation | null,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> | null {
  if (repair === null || (state.kind !== 'published' && state.kind !== 'partial-directory')) {
    return null
  }
  if (repair.actionMode !== 'routine-restoration') {
    return copy(
      'Restoration tool catch-up required',
      'WindShare has not validated a complete terminal sidecar checkpoint, so this result is not presented as complete yet.',
      'warning',
    )
  }
  if (state.kind === 'published') {
    return copy(
      'Completed with compatible names',
      'The complete result was saved under compatible names. Run the restoration tool to restore the logical names.',
      'positive',
    )
  }
  return copy(
    'Partial with compatible names',
    `${partialDirectoryDescription(state)} Saved entries still use compatible names until the restoration tool runs.`,
    'warning',
  )
}

function resumableReceiveCopy(
  state: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
  plan: MaterializationPlan,
  recoverySummary: RecoverySummary | null,
) {
  if (state.payloadKind === 'direct-zip') {
    return copy(
      'Ready to continue the ZIP',
      `${formatBytes(state.safeSelectedPayloadBytes)} can be resumed safely. ` +
        `Continuing may require up to ${formatBytes(state.committedArchiveLength)} of additional temporary space.`,
      'warning',
    )
  }
  if (state.payloadKind === 'workspace-archive') {
    if (state.pauseReason === 'storage-pressure') {
      return copy('Browser storage is full',
        `${formatBytes(state.completedBytes)} of received content is retained. Free browser storage, then choose Continue.`, 'warning')
    }
    return copy('ZIP progress retained',
      `${state.completedFileCount.toString()} complete files (${formatBytes(state.completedBytes)}) are retained. ` +
        (state.discoveryComplete ? 'The saved checkpoint preserves received content for continuation.' :
          'Selection discovery is unfinished; continue when the sender is available.'), 'warning')
  }
  return copy(
    'Ready to continue receiving',
    resumableFileSetDescription(state, plan.kind, recoverySummary),
    'warning',
  )
}

function presentLifecycleAction(kind: LifecycleUserAction, input: ReceiveActionFacts & { readonly artifact: ArtifactSpec }): LifecycleActionPresentation {
  const { state, plan, artifact } = input
  switch (kind) {
    case 'pause': return action(kind, 'Pause and keep verified progress')
    case 'stop': return plan.kind === 'direct-resumable-zip'
      ? action(kind, 'Stop and retain the unfinished ZIP') : action(kind, 'Stop receiving', true)
    case 'continue': return action(kind, continueActionLabel(state, artifact, plan.kind))
    case 'save': return action(kind, 'Save ' + browserArtifactName(artifact))
    case 'save-staged-files': return action(kind, 'Save received files to folder')
    case 'cleanup-staging': return action(kind, 'Retry staging cleanup')
    case 'discard-incomplete-staging': return action(kind, 'Discard incomplete browser data', true)
    case 'redownload': return state.kind === 'resumable-receive'
      ? action(kind, 'Restart incomplete files', true) : action(kind, 'Download again')
    case 'change-location': return action(kind, 'Change save location')
    case 'discard': return action(kind, discardActionLabel(state), true)
    case 'delete': return action(kind, deleteActionLabel(state), true)
  }
}

function continueActionLabel(state: ReceiveLifecycleState, artifact: ArtifactSpec, plan: MaterializationPlan['kind']): string {
  switch (state.kind) {
    case 'resumable-start': return state.reason === 'paused' ? 'Continue' : 'Retry download'
    case 'resumable-receive':
      if (state.payloadKind === 'direct-zip') return 'Continue ZIP'
      return plan === 'workspace-then-publish' ? 'Continue receiving' : 'Continue and preserve partial files'
    case 'resumable-package': return artifact.kind === 'zip-archive' ? 'Continue generating ZIP' : 'Continue preparing file'
    case 'authorization-required': return 'Authorize and continue'
    case 'target-verification-required': return 'Verify target and continue'
    case 'destination-space-required': return 'Retry after freeing space'
    default: return 'Continue'
  }
}

function discardActionLabel(state: ReceiveLifecycleState): string {
  if (state.kind === 'resumable-start') return 'Discard download'
  if (state.kind === 'resumable-receive') return 'Discard task and clean unfinished content'
  return 'Discard task and delete retained content'
}

function deleteActionLabel(state: ReceiveLifecycleState): string {
  if (state.kind === 'resumable-receive') return 'Delete only after ownership is verified'
  if (state.kind === 'authorization-required' || state.kind === 'target-verification-required' ||
      state.kind === 'destination-space-required') return 'Verify ownership and delete unfinished ZIP'
  return 'Delete retained result'
}

function action(
  kind: LifecycleUserAction,
  label: string,
  destructive = false,
): LifecycleActionPresentation {
  return Object.freeze({ kind, label, destructive })
}

function workspaceUsagePresentation(input: Readonly<{
  state: ReceiveLifecycleState
  plan: MaterializationPlan
  workspaceUsage?: WorkspaceUsage | null
}>): WorkspaceUsagePresentation | null {
  const usage = input.workspaceUsage
  if (usage === undefined || usage === null || input.plan.kind !== 'workspace-then-publish' ||
      !lifecycleOwnsWorkspaceData(input.state)) return null
  if (usage.ownedBytes < 0n || (usage.maximumBytes !== undefined && usage.maximumBytes <= 0n)) {
    throw new RangeError('workspace usage must use non-negative owned bytes and a positive limit')
  }
  const label = usage.maximumBytes === undefined
    ? `${formatBytes(usage.ownedBytes)} stored by this task`
    : `${formatBytes(usage.ownedBytes)} of ${formatBytes(usage.maximumBytes)} used by this task`
  return Object.freeze({
    ownedBytes: usage.ownedBytes,
    ...(usage.maximumBytes === undefined ? {} : { maximumBytes: usage.maximumBytes }),
    label,
  })
}

function lifecycleOwnsWorkspaceData(state: ReceiveLifecycleState): boolean {
  if (state.kind === 'discarded' || state.kind === 'partial-directory' ||
      state.kind === 'restart-required') return false
  if (state.kind === 'published') return state.cleanupState === 'cleanup-pending'
  return state.kind !== 'download-started' || state.attemptKind === 'workspace'
}

function receivingDescription(artifact: ArtifactSpec, plan: MaterializationPlan): string {
  switch (artifact.kind) {
    case 'directory-tree':
      return 'Files are being saved with their selected folder hierarchy. Completed files may already be visible.'
    case 'original-file':
      return 'The complete file is being received. An incomplete file will not be published.'
    case 'zip-archive':
      return plan.kind === 'direct-resumable-zip'
        ? `${plan.binding.stableName} is an incomplete target until closing and verification finish. Do not move or modify it meanwhile.`
        : 'Selected files are being received for one complete ZIP without compression.'
  }
}

function partialDirectoryDescription(
  state: Extract<ReceiveLifecycleState, { kind: 'partial-directory' }>,
): string {
  if (state.failureCount === 0n) return `${state.successCount} file(s) remain saved.`
  return `${state.successCount} file(s) remain saved; ${state.failureCount} item(s) did not finish.`
}

function browserArtifactName(artifact: ArtifactSpec): string {
  return artifactRequestedName(artifact)
}

function restartRequiredDescription(
  reason: Extract<ReceiveLifecycleState, { kind: 'restart-required' }>['reason'],
): string {
  switch (reason) {
    case 'direct-atomic-rolled-back':
      return 'The incomplete result was rolled back. Start a new receive operation.'
    case 'portable-aborted':
      return 'The browser download did not start and no resumable copy was retained.'
    case 'source-revision-changed':
      return 'The shared file changed, so the complete result must be received again.'
    case 'preparation-invalidated':
      return 'The confirmed selection changed before completion. Start again with fresh evidence.'
    case 'content-session-ended':
      return 'The non-resumable receive session ended before the complete result was ready.'
    case 'target-deleted':
      return 'The owned ZIP target is missing. WindShare will not recreate it under the old operation; start a new receive operation.'
  }
}

function directZipProgressCopy(
  plan: MaterializationPlan,
  progress: V2DirectZipProgressSnapshot | null,
): ReturnType<typeof copy> | null {
  if (plan.kind !== 'direct-resumable-zip' || progress === null) return null
  switch (progress.phase) {
    case 'receiving': return null
    case 'saving-resume-position':
      return copy(
        'Saving a safe resume position',
        `${plan.binding.stableName} is being closed at a verified cut. Keep this page open until the safe position is recorded.`,
        'neutral',
      )
    case 'closing':
      return copy(
        `Closing ${plan.binding.stableName}`,
        'All selected bytes were received, but the ZIP is still incomplete until closing records are written and verified.',
        'neutral',
      )
    case 'verifying':
      return copy(
        'Confirming saved content',
        `WindShare is verifying ${plan.binding.stableName}. Do not move or modify the target until this check finishes.`,
        'neutral',
      )
  }
}

function needsAttentionDescription(
  reason: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>['reason'],
): string {
  switch (reason) {
    case 'target-ownership-unknown':
      return 'WindShare cannot safely determine whether the selected destination still belongs to this task. No automatic change was made.'
    case 'publication-unknown':
      return 'WindShare cannot safely determine whether the final save operation completed. It will not retry automatically.'
    case 'cleanup-unknown':
      return 'WindShare cannot safely confirm ownership while cleaning up retained data. It will not remove anything automatically.'
  }
}

function copy(
  title: string,
  description: string,
  tone: ReceiveLifecyclePresentation['tone'],
) {
  return Object.freeze({ title, description, tone })
}
