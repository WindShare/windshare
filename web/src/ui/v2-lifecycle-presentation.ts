import { artifactRequestedName } from '../output/planning'
import {
  lifecycleDeadline,
  type ReceiveLifecycleState,
} from '../output/workspace'
import type {
  ArtifactSpec,
  MaterializationPlan,
} from '../transfer/intent'
import { formatBytes } from './v2-progress-presentation'

export interface WorkspaceUsage {
  readonly ownedBytes: bigint
  readonly maximumBytes?: bigint
}

export interface RetentionPresentation {
  readonly expiresAt: number
  readonly remainingMilliseconds: number
  readonly elapsed: boolean
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
  | 'redownload'
  | 'change-location'
  | 'discard'
  | 'delete'

export type V2ActiveReceiveControl = Extract<LifecycleUserAction, 'pause' | 'stop'>

export interface LifecycleActionPresentation {
  readonly kind: LifecycleUserAction
  readonly label: string
  readonly destructive: boolean
}

export interface ReceiveLifecyclePresentation {
  readonly stateKind: ReceiveLifecycleState['kind']
  readonly category: 'active' | 'retained' | 'terminal'
  readonly title: string
  readonly description: string
  readonly tone: 'neutral' | 'positive' | 'warning' | 'critical'
  readonly retention: RetentionPresentation | null
  readonly usage: WorkspaceUsagePresentation | null
  readonly actions: readonly LifecycleActionPresentation[]
}

export function presentReceiveLifecycle(input: Readonly<{
  state: ReceiveLifecycleState
  artifact: ArtifactSpec
  planKind: MaterializationPlan['kind']
  nowMilliseconds: number
  workspaceUsage?: WorkspaceUsage | null
  activeControls?: readonly V2ActiveReceiveControl[]
}>): ReceiveLifecyclePresentation {
  requireClock(input.nowMilliseconds)
  const retention = retentionPresentation(input.state, input.nowMilliseconds)
  const copy = lifecycleCopy(input.state, input.artifact)
  const actions = presentedLifecycleActions(input, retention)
  return Object.freeze({
    stateKind: input.state.kind,
    category: lifecycleCategory(input.state),
    title: retention?.elapsed === true && isStableState(input.state)
      ? 'The retention period has ended.'
      : copy.title,
    description: retention?.elapsed === true && isStableState(input.state)
      ? 'This task can no longer continue. WindShare will clean up its retained data.'
      : copy.description,
    tone: retention?.elapsed === true && isStableState(input.state) ? 'warning' : copy.tone,
    retention,
    usage: workspaceUsagePresentation(input),
    actions,
  })
}

function presentedLifecycleActions(
  input: Readonly<{
    state: ReceiveLifecycleState
    artifact: ArtifactSpec
    planKind: MaterializationPlan['kind']
    activeControls?: readonly V2ActiveReceiveControl[]
  }>,
  retention: RetentionPresentation | null,
): readonly LifecycleActionPresentation[] {
  if (retention?.elapsed === true && isStableState(input.state)) return Object.freeze([])
  if (input.activeControls !== undefined && input.activeControls.length > 0) {
    return activeControlActions(input.state, input.activeControls)
  }
  return lifecycleActions(input.state, input.artifact, input.planKind)
}

function lifecycleCopy(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> {
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
      return copy('Receiving files', receivingDescription(artifact), 'neutral')
    case 'resumable-receive':
      return copy(
        'Ready to continue receiving',
        `${state.completedFileCount} file(s) and ${formatBytes(state.completedBytes)} are complete. Continuing still requires the sender and save permission.`,
        'warning',
      )
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
            'Generating ZIP',
            'WindShare is creating one ZIP without compression. It cannot be saved until the complete package is sealed.',
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
      return copy('Ready to save', `${name} is complete. Choose where to save it before the retention period ends.`, 'positive')
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
    case 'expired':
      return copy(
        'Task expired',
        state.cleanupState === 'cleanup-pending'
          ? 'The retention period ended. Continuing is disabled while owned data is cleaned up.'
          : 'The retention period ended and owned retained data was cleaned up.',
        'warning',
      )
    case 'needs-attention':
      return copy('Needs attention', needsAttentionDescription(state.reason), 'critical')
  }
}

function lifecycleActions(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  planKind: MaterializationPlan['kind'],
): readonly LifecycleActionPresentation[] {
  switch (state.kind) {
    case 'resumable-receive':
      return planKind === 'workspace-then-publish'
        ? Object.freeze([
            action('continue', 'Continue receiving'),
            action('discard', 'Discard task and clean unfinished content', true),
          ])
        : Object.freeze([action('continue', 'Continue receiving')])
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
    case 'expired':
      return state.cleanupState === 'cleanup-pending' && planKind === 'workspace-then-publish'
        ? Object.freeze([action('delete', 'Delete expired data', true)])
        : Object.freeze([])
    default:
      return Object.freeze([])
  }
}

function activeControlActions(
  state: ReceiveLifecycleState,
  controls: readonly V2ActiveReceiveControl[],
): readonly LifecycleActionPresentation[] {
  if (lifecycleCategory(state) !== 'active') {
    throw new TypeError('active receive controls require an active lifecycle state')
  }
  if (new Set(controls).size !== controls.length) {
    throw new TypeError('active receive controls contain a duplicate action')
  }
  return Object.freeze(controls.map((control) => control === 'pause'
    ? action('pause', 'Pause and keep verified progress')
    : action('stop', 'Stop receiving', true)))
}

function action(
  kind: LifecycleUserAction,
  label: string,
  destructive = false,
): LifecycleActionPresentation {
  return Object.freeze({ kind, label, destructive })
}

function retentionPresentation(
  state: ReceiveLifecycleState,
  nowMilliseconds: number,
): RetentionPresentation | null {
  const expiresAt = state.kind === 'expired' ? state.expiresAt : lifecycleDeadline(state)
  if (expiresAt === undefined) return null
  return Object.freeze({
    expiresAt,
    remainingMilliseconds: Math.max(0, expiresAt - nowMilliseconds),
    elapsed: nowMilliseconds >= expiresAt,
  })
}

function workspaceUsagePresentation(input: Readonly<{
  state: ReceiveLifecycleState
  planKind: MaterializationPlan['kind']
  workspaceUsage?: WorkspaceUsage | null
}>): WorkspaceUsagePresentation | null {
  const usage = input.workspaceUsage
  if (usage === undefined || usage === null || input.planKind !== 'workspace-then-publish' ||
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
  if (state.kind === 'expired') return state.cleanupState === 'cleanup-pending'
  return state.kind !== 'download-started' || state.attemptKind === 'workspace'
}

function lifecycleCategory(state: ReceiveLifecycleState): ReceiveLifecyclePresentation['category'] {
  if (state.kind === 'resumable-receive' || state.kind === 'resumable-package' ||
      state.kind === 'waiting-to-save' ||
      (state.kind === 'download-started' && state.attemptKind === 'workspace')) return 'retained'
  if (state.kind === 'published' || state.kind === 'partial-directory' ||
      state.kind === 'restart-required' || state.kind === 'discarded' ||
      state.kind === 'expired' || state.kind === 'needs-attention' ||
      state.kind === 'download-started') return 'terminal'
  return 'active'
}

function isStableState(state: ReceiveLifecycleState): boolean {
  return state.kind === 'resumable-receive' || state.kind === 'resumable-package' ||
    state.kind === 'waiting-to-save' ||
    (state.kind === 'download-started' && state.attemptKind === 'workspace')
}

function receivingDescription(artifact: ArtifactSpec): string {
  switch (artifact.kind) {
    case 'directory-tree':
      return 'Files are being saved with their selected folder hierarchy. Completed files may already be visible.'
    case 'original-file':
      return 'The complete file is being received. An incomplete file will not be published.'
    case 'zip-archive':
      return 'Selected files are being received for one complete ZIP without compression.'
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

function requireClock(nowMilliseconds: number): void {
  if (!Number.isSafeInteger(nowMilliseconds) || nowMilliseconds < 0) {
    throw new TypeError('presentation clock must be a non-negative safe integer')
  }
}
