import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import { availableReceiveActions, receiveLifecycleCategory, newReceiveOperationKind, type ReceiveActionFacts } from '../application/receive/policy'
import type { LifecycleUserAction, PersistentWriterOpenPauseFact, ActiveReceiveControl, ReceiveSettlementWait, WorkspaceUsage } from '../application/receive/actions'
import { artifactRequestedName } from '../output/model/artifact-name'
import type {
  CompatibleNameRepairSummary,
} from '../transfer/lifecycle/name-repair'
import type {
  RecoverySummary,
} from '../transfer/lifecycle/recovery-summary'
import {
  type ReceiveLifecycleState,
} from '../transfer/lifecycle/state'
import type {
  ArtifactSpec,
  MaterializationPlan,
} from '../transfer/intent'
import { resumableFileSetDescription } from './resumable-file-set-presentation'
import {
  presentCompatibleNameRepair,
  type CompatibleNameRepairPresentation,
} from './compatible-name-repair-presentation'
import { formatBytes } from './progress-presentation'
import type { DirectZipProgressSnapshot } from '../application/receive/contracts'

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
}>, language: Language = 'en'): NewReceiveOperationPresentation | null {
  const t = receiverCopy[language].lifecycle
  const { lifecycle, plan } = input
  if (lifecycle === null || plan === null) return null
  const kind = newReceiveOperationKind(lifecycle, plan)
  if (kind === 'direct-tree-to-zip') {
    return Object.freeze({
      kind: 'direct-tree-to-zip',
      ariaLabel: t.receiveZip,
      title: t.selectionAsZip,
      description: t.selectionAsZipHint,
      actionLabel: t.chooseZip,
    })
  }
  if (kind === 'deleted-direct-zip-target') {
    return Object.freeze({
      kind: 'deleted-direct-zip-target',
      ariaLabel: t.newReceive,
      title: t.chooseRoute,
      description: t.chooseRouteHint,
      actionLabel: t.chooseNewRoute,
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
  activeControls?: readonly ActiveReceiveControl[]
  interruption?: ReceiveSettlementWait | null
  repairSummary?: CompatibleNameRepairSummary | null
  recoverySummary?: RecoverySummary | null
  writerOpenPause?: PersistentWriterOpenPauseFact | null
  directZipProgress?: DirectZipProgressSnapshot | null
  browserDelivery?: import('../output/model/browser-delivery').BrowserDeliverySummary | null
}>, language: Language = 'en'): ReceiveLifecyclePresentation {
  const compatibleNameRepair = input.repairSummary === undefined || input.repairSummary === null
    ? null
    : presentCompatibleNameRepair({ state: input.state, summary: input.repairSummary }, language)
  const copy = input.interruption === undefined || input.interruption === null
    ? describeLifecycle(
        input.state,
        input.artifact,
        input.plan,
        input.directZipProgress ?? null,
        compatibleNameRepair,
        input.recoverySummary ?? null, language,
      )
    : interruptionCopy(input.interruption, language)
  const actions = Object.freeze(availableReceiveActions(input).map(kind => presentLifecycleAction(kind, input, language)))
  const writerOpenPause = presentPersistentWriterOpenPause(input, language)
  return Object.freeze({
    stateKind: input.state.kind,
    category: receiveLifecycleCategory(input.state),
    title: copy.title,
    description: copy.description,
    tone: copy.tone,
    usage: workspaceUsagePresentation(input, language),
    actions,
    compatibleNameRepair,
    writerOpenPause,
  })
}

function presentPersistentWriterOpenPause(input: Readonly<{
  state: ReceiveLifecycleState
  plan: MaterializationPlan
  writerOpenPause?: PersistentWriterOpenPauseFact | null
}>, language: Language): PersistentWriterOpenPausePresentation | null {
  const t = receiverCopy[language].lifecycle
  const fact = input.writerOpenPause
  if (fact === undefined || fact === null || input.plan.kind !== 'direct-tree' ||
      input.state.kind !== 'resumable-receive' || input.state.payloadKind !== 'file-set') return null
  const materializationPath = fact.materializationRelativePath.join('/')
  const context = fact.purpose === 'automatic-checkpoint'
    ? t.checkpointFailed
    : t.writerFailed
  return Object.freeze({
    materializationPath,
    title: t.reopenFile(materializationPath),
    description: t.reopenCost(context, formatBytes(fact.cost.prefixCopyBytes),
      formatBytes(fact.cost.writeAmplificationBytes), formatBytes(fact.cost.temporaryBytes)),
  })
}

function interruptionCopy(
  interruption: ReceiveSettlementWait,
  language: Language,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> {
  const t = receiverCopy[language].lifecycle
  if (interruption.operation === 'finish') {
    return copy(
      t.slowFinish,
      t.slowFinishHint,
      'warning',
    )
  }
  if (interruption.operation === 'pause') {
    return interruption.phase === 'waiting'
      ? copy(
          t.pausing,
          t.pausingHint,
          'neutral',
        )
      : copy(
          t.pausingBackground,
          t.pausingBackgroundHint,
          'warning',
        )
  }
  return interruption.phase === 'waiting'
    ? copy(
        t.stopping,
        t.stoppingHint,
        'neutral',
      )
    : copy(
        t.stoppingBackground,
        t.stoppingBackgroundHint,
        'warning',
      )
}

function describeLifecycle(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  plan: MaterializationPlan,
  directZipProgress: DirectZipProgressSnapshot | null,
  compatibleNameRepair: CompatibleNameRepairPresentation | null,
  recoverySummary: RecoverySummary | null,
  language: Language,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> {
  const t = receiverCopy[language].lifecycle
  const override = lifecycleOverrideCopy(
    state,
    plan,
    directZipProgress,
    compatibleNameRepair, language,
  )
  if (override !== null) return override
  const name = browserArtifactName(artifact)
  switch (state.kind) {
    case 'intent-frozen':
      return copy(t.ready, t.readyHint, 'neutral')
    case 'resumable-start':
      return copy(state.reason === 'paused' ? t.pausedBeforeStart : t.startFailed,
        t.retryHint, 'warning')
    case 'preparing':
      return copy(
        t.checking,
        t.checkingHint,
        'neutral',
      )
    case 'receiving':
      return copy(t.receiving, receivingDescription(artifact, plan, language), 'neutral')
    case 'resumable-receive': return resumableReceiveCopy(state, plan, recoverySummary, language)
    case 'finalizing-tree':
      return copy(
        t.finishingFolder,
        t.finishingFolderHint,
        'neutral',
      )
    case 'materialization-sealed':
      return copy(t.received, t.receivedHint, 'neutral')
    case 'packaging':
      return artifact.kind === 'zip-archive'
        ? copy(
            t.finalizingZip,
            t.finalizingZipHint,
            'neutral',
          )
        : copy(t.preparingFile, t.preparingFileHint, 'neutral')
    case 'resumable-package':
      return copy(
        artifact.kind === 'zip-archive' ? t.resumeZip : t.resumeFile,
        t.resumeHint,
        'warning',
      )
    case 'artifact-sealed':
      return copy(t.sealed, t.completeName(name), 'neutral')
    case 'waiting-to-save':
      return copy(t.readyToSave, t.retainedName(name), 'positive')
    case 'publishing-managed':
      return copy(t.savingName(name), t.publishingHint, 'neutral')
    case 'handing-off':
      return copy(t.startingBrowser, t.handoffHint, 'neutral')
    case 'published':
      return copy(
        t.saved,
        state.cleanupState === 'cleanup-pending'
          ? t.pendingCleanup
          : t.cleaned,
        'positive',
      )
    case 'download-started':
      return copy(
        t.downloadStarted,
        t.browserHint,
        'positive',
      )
    case 'partial-directory':
      return copy(
        state.reason === 'stopped' ? t.stopped : t.partialFolder,
        partialDirectoryDescription(state, language),
        'warning',
      )
    case 'restart-required':
      return copy(t.restart, restartRequiredDescription(state.reason, language), 'warning')
    case 'source-invalidated':
      return copy(t.sourceChanged, t.sourceChangedHint, 'warning')
    case 'discarded':
      return copy(t.discarded, t.discardedHint, 'neutral')
    case 'needs-attention':
      return copy(t.needsAttention, needsAttentionDescription(state.reason, language), 'critical')
    case 'authorization-required':
      return copy(
        t.authorization,
        t.authorizationHint,
        'warning',
      )
    case 'target-verification-required':
      return copy(
        t.verify,
        t.verifyHint,
        'warning',
      )
    case 'destination-space-required':
      return copy(
        t.space,
        t.spaceHint,
        'warning',
      )
  }
}

function lifecycleOverrideCopy(
  state: ReceiveLifecycleState,
  plan: MaterializationPlan,
  directZipProgress: DirectZipProgressSnapshot | null,
  compatibleNameRepair: CompatibleNameRepairPresentation | null,
  language: Language,
): ReturnType<typeof copy> | null {
  const repair = compatibleNameLifecycleCopy(state, compatibleNameRepair, language)
  if (repair !== null) return repair
  return state.kind === 'published' ? null : directZipProgressCopy(plan, directZipProgress, language)
}

function compatibleNameLifecycleCopy(
  state: ReceiveLifecycleState,
  repair: CompatibleNameRepairPresentation | null,
  language: Language,
): Readonly<{
  title: string
  description: string
  tone: ReceiveLifecyclePresentation['tone']
}> | null {
  const t = receiverCopy[language].lifecycle
  if (repair === null || (state.kind !== 'published' && state.kind !== 'partial-directory')) {
    return null
  }
  if (repair.actionMode !== 'routine-restoration') {
    return copy(
      t.repairNeeded,
      t.repairNeededHint,
      'warning',
    )
  }
  if (state.kind === 'published') {
    return copy(
      t.compatibleComplete,
      t.compatibleCompleteHint,
      'positive',
    )
  }
  return copy(
    t.compatiblePartial,
    t.partialCompatible(partialDirectoryDescription(state, language)),
    'warning',
  )
}

function resumableReceiveCopy(
  state: Extract<ReceiveLifecycleState, { kind: 'resumable-receive' }>,
  plan: MaterializationPlan,
  recoverySummary: RecoverySummary | null,
  language: Language,
) {
  const t = receiverCopy[language].lifecycle
  if (state.payloadKind === 'direct-zip') {
    return copy(
      t.continueZip,
      t.zipSpace(formatBytes(state.safeSelectedPayloadBytes), formatBytes(state.committedArchiveLength)),
      'warning',
    )
  }
  if (state.payloadKind === 'workspace-archive') {
    if (state.pauseReason === 'storage-pressure') {
      return copy(t.storageFull,
        t.retainedSpace(formatBytes(state.completedBytes)), 'warning')
    }
    return copy(t.zipRetained,
      t.retainedFiles(state.completedFileCount, formatBytes(state.completedBytes), state.discoveryComplete ? t.checkpointComplete :
          t.incompleteDiscovery), 'warning')
  }
  return copy(
    t.continueReceiving,
    resumableFileSetDescription(state, plan.kind, recoverySummary, language),
    'warning',
  )
}

function presentLifecycleAction(kind: LifecycleUserAction, input: ReceiveActionFacts & { readonly artifact: ArtifactSpec }, language: Language): LifecycleActionPresentation {
  const t = receiverCopy[language].lifecycle
  const { state, plan, artifact } = input
  switch (kind) {
    case 'pause': return action(kind, t.pauseAction)
    case 'stop': return plan.kind === 'direct-resumable-zip'
      ? action(kind, t.stopZip) : action(kind, t.stopAction, true)
    case 'continue': return action(kind, continueActionLabel(state, artifact, plan.kind, language))
    case 'save': return action(kind, t.saveNamed(browserArtifactName(artifact)))
    case 'save-staged-files': return action(kind, t.saveStaged)
    case 'cleanup-staging': return action(kind, t.cleanupStaging)
    case 'discard-incomplete-staging': return action(kind, t.discardStaging, true)
    case 'redownload': return state.kind === 'resumable-receive'
      ? action(kind, t.restartIncomplete, true) : action(kind, t.downloadAgain)
    case 'change-location': return action(kind, t.changeLocation)
    case 'discard': return action(kind, discardActionLabel(state, language), true)
    case 'delete': return action(kind, deleteActionLabel(state, language), true)
  }
}

function continueActionLabel(state: ReceiveLifecycleState, artifact: ArtifactSpec, plan: MaterializationPlan['kind'], language: Language): string {
  const t = receiverCopy[language].lifecycle
  switch (state.kind) {
    case 'resumable-start': return state.reason === 'paused' ? t.resumeAction : t.retryDownload
    case 'resumable-receive':
      if (state.payloadKind === 'direct-zip') return t.continueZipAction
      return plan === 'workspace-then-publish' ? t.continueReceiveAction : t.resumePartial
    case 'resumable-package': return artifact.kind === 'zip-archive' ? t.continuePackaging : t.continueFile
    case 'authorization-required': return t.authorizeContinue
    case 'target-verification-required': return t.verifyContinue
    case 'destination-space-required': return t.retrySpace
    default: return t.resumeAction
  }
}

function discardActionLabel(state: ReceiveLifecycleState, language: Language): string {
  const t = receiverCopy[language].lifecycle
  if (state.kind === 'resumable-start') return t.discardDownload
  if (state.kind === 'resumable-receive') return t.discardUnfinished
  return t.discardRetained
}

function deleteActionLabel(state: ReceiveLifecycleState, language: Language): string {
  const t = receiverCopy[language].lifecycle
  if (state.kind === 'resumable-receive') return t.deleteAfterVerify
  if (state.kind === 'authorization-required' || state.kind === 'target-verification-required' ||
      state.kind === 'destination-space-required') return t.verifyDelete
  return t.deleteRetained
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
}>, language: Language): WorkspaceUsagePresentation | null {
  const t = receiverCopy[language].lifecycle
  const usage = input.workspaceUsage
  if (usage === undefined || usage === null || input.plan.kind !== 'workspace-then-publish' ||
      !lifecycleOwnsWorkspaceData(input.state)) return null
  if (usage.ownedBytes < 0n || (usage.maximumBytes !== undefined && usage.maximumBytes <= 0n)) {
    throw new RangeError('workspace usage must use non-negative owned bytes and a positive limit')
  }
  const label = usage.maximumBytes === undefined
    ? t.stored(formatBytes(usage.ownedBytes))
    : t.used(formatBytes(usage.ownedBytes), formatBytes(usage.maximumBytes))
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

function receivingDescription(artifact: ArtifactSpec, plan: MaterializationPlan, language: Language): string {
  const t = receiverCopy[language].lifecycle
  switch (artifact.kind) {
    case 'directory-tree':
      return t.treeHint
    case 'original-file':
      return t.fileHint
    case 'zip-archive':
      return plan.kind === 'direct-resumable-zip'
        ? t.incompleteZip(plan.binding.stableName)
        : t.zipHint
  }
}

function partialDirectoryDescription(
  state: Extract<ReceiveLifecycleState, { kind: 'partial-directory' }>,
  language: Language,
): string {
  const t = receiverCopy[language].lifecycle
  if (state.failureCount === 0n) return t.folderSaved(state.successCount)
  return t.folderPartial(state.successCount, state.failureCount)
}

function browserArtifactName(artifact: ArtifactSpec): string {
  return artifactRequestedName(artifact)
}

function restartRequiredDescription(
  reason: Extract<ReceiveLifecycleState, { kind: 'restart-required' }>['reason'],
  language: Language,
): string {
  const t = receiverCopy[language].lifecycle
  switch (reason) {
    case 'portable-aborted':
      return t.aborted
    case 'source-revision-changed':
      return t.changedHint
    case 'preparation-invalidated':
      return t.invalidated
    case 'content-session-ended':
      return t.ended
    case 'target-deleted':
      return t.targetDeleted
  }
}

function directZipProgressCopy(
  plan: MaterializationPlan,
  progress: DirectZipProgressSnapshot | null,
  language: Language,
): ReturnType<typeof copy> | null {
  const t = receiverCopy[language].lifecycle
  if (plan.kind !== 'direct-resumable-zip' || progress === null) return null
  switch (progress.phase) {
    case 'receiving': return null
    case 'saving-resume-position':
      return copy(
        t.checkpoint,
        t.savingCheckpoint(plan.binding.stableName),
        'neutral',
      )
    case 'closing':
      return copy(
        t.closing(plan.binding.stableName),
        t.closingHint,
        'neutral',
      )
    case 'verifying':
      return copy(
        t.verifying,
        t.verifyingName(plan.binding.stableName),
        'neutral',
      )
  }
}

function needsAttentionDescription(
  reason: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>['reason'],
  language: Language,
): string {
  const t = receiverCopy[language].lifecycle
  switch (reason) {
    case 'target-ownership-unknown':
      return t.unknownOwner
    case 'publication-unknown':
      return t.unknownPublication
    case 'cleanup-unknown':
      return t.unknownCleanup
  }
}

function copy(
  title: string,
  description: string,
  tone: ReceiveLifecyclePresentation['tone'],
) {
  return Object.freeze({ title, description, tone })
}
