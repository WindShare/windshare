import type { ReceiveBlockReason } from '../../application/receive/contracts'
import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import { destinationLabel } from './display'
import { objectLabel } from '../draft/presentation'
import type {
  TaskAction,
  TaskFacts,
  TaskPresentation,
  TaskPresentationTransition,
  TaskSafeNameRestoreState,
  TaskStage,
} from './model'
import { presentTaskProgress } from './progress'
import { receiveElapsedMilliseconds, type ReceiveContentWarning } from '../../transfer/state'
import { resolveTaskStage, type StageCopy } from './stage'

const FALLBACK_OPERATION_LABEL_LENGTH = 8

function localizeAction(action: TaskAction<ReceiveBlockReason>, language: Language): TaskAction {
  return action.disabledReason === null
    ? action
    : Object.freeze({ ...action, disabledReason: receiverCopy[language].admission[action.disabledReason] })
}

function presentRemoval(
  facts: TaskFacts,
  destructiveActions: readonly TaskAction[],
  language: Language,
): TaskPresentation['removal'] {
  if (destructiveActions.length === 0) return null
  const downloads = receiverCopy[language].downloads
  const folder = facts.backend === 'fsa'
  return Object.freeze({
    summary: folder ? downloads.removeFolder : downloads.remove,
    hint: folder ? downloads.removeFolderHint : downloads.removeHint,
    fallbackForget: facts.fallbackForget == null ? null : localizeAction(facts.fallbackForget, language),
  })
}

function partialCatalogLimitHint(
  state: TaskFacts['lifecycle'],
  language: Language,
): string | null {
  if (state.kind !== 'partial') return null
  const stageCopy = receiverCopy[language].stage
  if (state.reason === 'browser-catalog-limit') return stageCopy.browserCatalogLimitHint
  if (state.reason === 'share-catalog-limit') return stageCopy.shareCatalogLimitHint
  return null
}

function collectTaskDetails(
  facts: TaskFacts,
  progress: ReturnType<typeof presentTaskProgress>,
  showProgress: boolean,
  language: Language,
): readonly string[] {
  const t = receiverCopy[language].progress
  const details = [...facts.details]
  if (!showProgress && progress !== null) {
    if (facts.lifecycle.kind === 'paused' || facts.lifecycle.kind === 'downloading') details.push(progress.label)
    details.push(...progress.details)
  }
  if (facts.lifecycle.kind === 'failed' && facts.lifecycle.detail) details.push(facts.lifecycle.detail)
  const catalogLimitHint = partialCatalogLimitHint(facts.lifecycle, language)
  if (facts.lifecycle.contentWarning !== undefined) {
    details.unshift(...contentWarningDetails(facts.lifecycle.contentWarning, facts.lifecycle.kind === 'paused', language))
  } else if (facts.completeness === 'partial') {
    details.unshift(t.partial)
  }
  if (catalogLimitHint !== null) details.unshift(catalogLimitHint)
  if (facts.lifecycle.kind === 'partial' && facts.lifecycle.cleanupPending) {
    details.push(receiverCopy[language].downloads.partialCleanupPending)
  }
  if (facts.sourceOmittedEntries > 0n) {
    details.push(receiverCopy[language].downloads.sourceOmittedCount(facts.sourceOmittedEntries))
    details.push(t.sourceOmittedHint)
  }
  return Object.freeze(details)
}

function resolveSafeNameRestore(facts: TaskFacts): TaskSafeNameRestoreState {
  if (facts.backend !== 'fsa' || facts.safeRenamed === null || facts.safeRenamed.count <= 0) {
    return 'none'
  }
  switch (facts.lifecycle.kind) {
    case 'preparing':
    case 'downloading':
      return 'active'
    case 'paused':
      return 'paused'
    case 'saved':
    case 'partial':
      return 'available'
    default:
      return 'none'
  }
}

export function presentTask(facts: TaskFacts, language: Language = 'en'): TaskPresentation {
  const copy = resolveTaskStage(facts, language)
  const actions: readonly TaskAction[] = facts.actions.map(action => localizeAction(action, language))
  const usableActions = actions.filter(action => !action.destructive)
  const destructiveActions = Object.freeze(actions.filter(action => action.destructive))
  const removal = presentRemoval(facts, destructiveActions, language)
  const primaryAction = preferredAction(usableActions, copy.stage)
  const attention = copy.stage === 'needs-action' || copy.stage === 'ready-to-save' ||
    facts.completeness === 'partial'
  const progress = presentTaskProgress(facts, language)
  const showProgress = ['preparing', 'downloading', 'waiting', 'paused', 'finishing'].includes(copy.stage)
  const details = collectTaskDetails(facts, progress, showProgress, language)
  const safeNameRestore = resolveSafeNameRestore(facts)
  const transition = transitionFor(facts, copy, attention, safeNameRestore)
  return Object.freeze({
    operationId: facts.lifecycle.taskId,
    generation: facts.lifecycle.generation,
    objectLabel: facts.display === null
      ? receiverCopy[language].downloads.unnamedTask(facts.lifecycle.taskId.slice(0, FALLBACK_OPERATION_LABEL_LENGTH))
      : objectLabel(facts.display.objectLabel, receiverCopy[language].browse),
    destinationLabel: facts.display?.destinationLabel === undefined
      ? null
      : destinationLabel(facts.display.destinationLabel, language),
    createdAtMilliseconds: facts.display?.createdAtMilliseconds ?? null,
    elapsedMilliseconds: receiveElapsedMilliseconds(facts.lifecycle.timing),
    stage: copy.stage,
    headline: copy.headline,
    description: copy.description,
    tone: taskTone(copy.stage, attention, facts),
    attention,
    progress: showProgress ? progress : null,
    primaryAction,
    secondaryActions: Object.freeze(usableActions.filter(action => action !== primaryAction)),
    destructiveActions,
    removal,
    details,
    safeRenamed: facts.safeRenamed,
    safeNameRestore,
    sourceOmittedEntries: facts.sourceOmittedEntries,
    completeness: facts.completeness,
    publication: facts.publication,
    transition,
  })
}

function contentWarningDetails(
  warning: ReceiveContentWarning,
  paused: boolean,
  language: Language,
): readonly string[] {
  const t = receiverCopy[language].progress
  const missing = warning.selectedFileCount - warning.completedFileCount
  const details = [t.missingFileCount(warning.completedFileCount, warning.selectedFileCount, missing, paused)]
  const reasons = {
    'source-changed': t.sourceChanged,
    'source-unavailable': t.sourceUnavailable,
    'source-busy': t.sourceBusy,
    'file-failed': t.fileFailed,
  }
  for (const file of warning.missingFiles) {
    details.push(file.reason === 'destination-conflict'
      ? t.destinationConflict(file.destinationPath)
      : t.missingFile(file.path.join('/'), reasons[file.reason]))
  }
  const additional = missing - BigInt(warning.missingFiles.length)
  if (additional > 0n) details.push(t.moreMissing(additional, paused))
  return details
}

function taskTone(stage: TaskStage, attention: boolean, facts: TaskFacts): TaskPresentation['tone'] {
  if (stage === 'failed') return 'critical'
  if (attention || stage === 'paused' || stage === 'waiting') return 'warning'
  // Omitted source items cannot be fixed by retrying locally, so surface a warning tone on
  // delivered cards without keeping a permanent needs-attention badge on Downloads.
  if (facts.sourceOmittedEntries > 0n && (stage === 'saved' || stage === 'handed-to-browser')) return 'warning'
  return facts.publication === 'saved' ? 'positive' : 'neutral'
}

function preferredAction(actions: readonly TaskAction[], taskStage: TaskStage): TaskAction | null {
  let preferred = ['continue', 'save', 'save-partial']
  if (taskStage === 'ready-to-save') preferred = ['save', 'save-partial']
  if (taskStage === 'downloading' || taskStage === 'waiting') preferred = ['pause', 'continue']
  for (const id of preferred) {
    const action = actions.find(candidate => candidate.id === id && candidate.disabledReason === null)
    if (action !== undefined) return action
  }
  return actions.find(action => action.id !== 'forget' && action.disabledReason === null) ??
    actions.find(action => action.id !== 'forget') ?? null
}

function transitionFor(
  facts: TaskFacts,
  copy: StageCopy,
  attention: boolean,
  safeNameRestore: TaskSafeNameRestoreState,
): TaskPresentationTransition {
  const actionFacts = facts.actions.map(action => `${action.id}:${action.disabledReason ?? ''}`).join('|')
  return Object.freeze({
    name: 'receiver.task.presentation',
    operation_id: facts.lifecycle.taskId,
    generation: facts.lifecycle.generation,
    stage: copy.stage,
    reason: copy.reason,
    completeness: facts.completeness,
    publication: facts.publication,
    attention,
    fingerprint: [
      facts.lifecycle.taskId, copy.stage, copy.reason, facts.completeness,
      facts.publication, attention, safeNameRestore, facts.safeRenamed?.count ?? 0, facts.sourceOmittedEntries, actionFacts,
      receiveElapsedMilliseconds(facts.lifecycle.timing),
    ].join(':'),
  })
}

export function compareTaskTransition(
  previous: TaskPresentationTransition | null,
  current: TaskPresentationTransition,
): TaskPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
