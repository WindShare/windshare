import { artifactRequestedName } from '../output/planning'
import type { CompatibleNameRepairSummary } from '../output/file-system-access/compatible-name/model'
import type { RecoverySummary } from '../output/file-system-access/recovery-summary'
import type {
  PreservingWriterCapacityPurpose,
  PreservingWriterCost,
} from '../output/persistent-tree/contracts'
import {
  isTerminalLifecycleState,
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
import { browserDeliveryLocalActions } from '../output/browser-delivery/recovery/local-actions'
import type { V2DirectZipProgressSnapshot } from './v2-receive-runtime'

export interface WorkspaceUsage {
  readonly ownedBytes: bigint
  readonly maximumBytes?: bigint
}

export interface WorkspaceUsagePresentation {
  readonly ownedBytes: bigint
  readonly maximumBytes?: bigint
  readonly label: string
}

export type LifecycleUserAction =
  | 'pause'
  | 'stop'
  | 'continue'
  | 'save'
  | 'save-staged-files'
  | 'cleanup-staging'
  | 'discard-incomplete-staging'
  | 'redownload'
  | 'change-location'
  | 'discard'
  | 'delete'

export type V2ActiveReceiveControl = Extract<LifecycleUserAction, 'pause' | 'stop'>

export type V2ReceiveInterruptionPresentation = Readonly<{
  readonly operation: V2ActiveReceiveControl | 'finish'
  readonly phase: 'waiting' | 'background'
}>

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
  if (plan.kind === 'direct-tree' && isTerminalLifecycleState(lifecycle)) {
    return Object.freeze({
      kind: 'direct-tree-to-zip',
      ariaLabel: 'Receive again as ZIP',
      title: 'Receive this selection as a ZIP',
      description: 'This starts a new receive operation. Files already received in the folder stay where they are, but their bytes are not reused for the ZIP.',
      actionLabel: 'Choose a ZIP route for a new operation',
    })
  }
  if (plan.kind === 'direct-resumable-zip' && lifecycle.kind === 'restart-required' &&
      lifecycle.reason === 'target-deleted') {
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

export {
  hasValidatedTerminalCompatibleNameRepair,
  presentCompatibleNameRepair,
} from './compatible-name-repair-presentation'
export type {
  CompatibleNameRepairActionMode,
  CompatibleNameRepairPresentation,
  CompatibleNameRepairPresentationContext,
} from './compatible-name-repair-presentation'

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

export interface PersistentWriterOpenPauseFact {
  readonly materializationRelativePath: readonly string[]
  readonly cost: PreservingWriterCost
  readonly purpose: PreservingWriterCapacityPurpose
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
  interruption?: V2ReceiveInterruptionPresentation | null
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
  const actions = presentedLifecycleActions(input)
  const writerOpenPause = presentPersistentWriterOpenPause(input)
  return Object.freeze({
    stateKind: input.state.kind,
    category: lifecycleCategory(input.state),
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
  interruption: V2ReceiveInterruptionPresentation,
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

function presentedLifecycleActions(
  input: Readonly<{
    state: ReceiveLifecycleState
    artifact: ArtifactSpec
    plan: MaterializationPlan
    activeControls?: readonly V2ActiveReceiveControl[]
    recoverySummary?: RecoverySummary | null
    browserDelivery?: import('../output/browser-delivery/retained').BrowserDeliveryResumeSummary | null
  }>,
): readonly LifecycleActionPresentation[] {
  if (input.activeControls !== undefined && input.activeControls.length > 0) {
    return activeControlActions(input.state, input.activeControls, input.plan.kind)
  }
  const localActions = input.plan.kind === 'direct-tree'
    ? browserFolderLocalActions(input.state, input.browserDelivery) : []
  if (input.state.kind === 'resumable-receive' &&
      input.state.payloadKind === 'file-set' &&
      input.plan.kind === 'direct-tree' &&
      (input.recoverySummary === undefined || input.recoverySummary === null)) {
    return localActions
  }
  return Object.freeze([...localActions, ...lifecycleActions(input.state, input.artifact, input.plan.kind)])
}

function browserFolderLocalActions(
  state: ReceiveLifecycleState,
  summary: import('../output/browser-delivery/retained').BrowserDeliveryResumeSummary | null | undefined,
): readonly LifecycleActionPresentation[] {
  return Object.freeze(browserDeliveryLocalActions(state, summary).map(kind => {
    switch (kind) {
      case 'save-staged-files': return { kind, label: 'Save received files to folder', destructive: false }
      case 'cleanup-staging': return { kind, label: 'Retry staging cleanup', destructive: false }
      case 'discard-incomplete-staging': return { kind, label: 'Discard incomplete browser data', destructive: true }
    }
  }))
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
  if (state.payloadKind === 'opfs-zip') {
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

function lifecycleActions(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  planKind: MaterializationPlan['kind'],
): readonly LifecycleActionPresentation[] {
  switch (state.kind) {
    case 'resumable-receive':
      if (state.payloadKind === 'direct-zip') {
        return Object.freeze([
          action('continue', 'Continue ZIP'),
          action('delete', 'Delete only after ownership is verified', true),
        ])
      }
      return planKind === 'workspace-then-publish'
        ? Object.freeze([
            action('continue', 'Continue receiving'),
            action('discard', 'Discard task and clean unfinished content', true),
          ])
        : Object.freeze([
            action('continue', 'Continue and preserve partial files'),
            action('redownload', 'Restart incomplete files', true),
          ])
    case 'resumable-package':
      return Object.freeze([
        action('continue', artifact.kind === 'zip-archive' ? 'Continue generating ZIP' : 'Continue preparing file'),
        action('discard', 'Discard task and delete retained content', true),
      ])
    case 'waiting-to-save':
      return Object.freeze([
        action('save', `Save ${browserArtifactName(artifact)}`),
        action('delete', 'Delete retained result', true),
      ])
    case 'download-started':
      return state.attemptKind === 'workspace'
        ? Object.freeze([
            action('redownload', 'Download again'),
            action('delete', 'Delete retained result', true),
          ])
        : Object.freeze([])
    case 'restart-required':
      return Object.freeze([])
    case 'authorization-required':
      return Object.freeze([
        action('continue', 'Authorize and continue'),
        action('delete', 'Verify ownership and delete unfinished ZIP', true),
      ])
    case 'target-verification-required':
      return Object.freeze([
        action('continue', 'Verify target and continue'),
        action('delete', 'Verify ownership and delete unfinished ZIP', true),
      ])
    case 'destination-space-required':
      return Object.freeze([
        action('continue', 'Retry after freeing space'),
        action('delete', 'Verify ownership and delete unfinished ZIP', true),
      ])
    default:
      return Object.freeze([])
  }
}

function activeControlActions(
  state: ReceiveLifecycleState,
  controls: readonly V2ActiveReceiveControl[],
  planKind: MaterializationPlan['kind'],
): readonly LifecycleActionPresentation[] {
  if (lifecycleCategory(state) !== 'active') {
    throw new TypeError('active receive controls require an active lifecycle state')
  }
  if (new Set(controls).size !== controls.length) {
    throw new TypeError('active receive controls contain a duplicate action')
  }
  return Object.freeze(controls.map(control => activeControlAction(control, planKind)))
}

function activeControlAction(
  control: V2ActiveReceiveControl,
  planKind: MaterializationPlan['kind'],
): LifecycleActionPresentation {
  if (control === 'pause') return action('pause', 'Pause and keep verified progress')
  return planKind === 'direct-resumable-zip'
    ? action('stop', 'Stop and retain the unfinished ZIP')
    : action('stop', 'Stop receiving', true)
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

function lifecycleCategory(state: ReceiveLifecycleState): ReceiveLifecyclePresentation['category'] {
  if (state.kind === 'resumable-receive' || state.kind === 'resumable-package' ||
      state.kind === 'waiting-to-save' || state.kind === 'authorization-required' ||
      state.kind === 'target-verification-required' || state.kind === 'destination-space-required' ||
      (state.kind === 'download-started' && state.attemptKind === 'workspace')) return 'retained'
  if (state.kind === 'published' || state.kind === 'partial-directory' ||
      state.kind === 'restart-required' || state.kind === 'discarded' ||
      state.kind === 'needs-attention' ||
      state.kind === 'download-started') return 'terminal'
  return 'active'
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
