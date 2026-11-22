import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import { destinationLabel } from './display'
import { objectLabel } from '../draft/presentation'
import type { TaskAction, TaskFacts, TaskPresentation, TaskPresentationTransition, TaskStage } from './model'
import { presentTaskProgress } from './progress'
import { receiveElapsedMilliseconds } from '../../transfer/lifecycle/timing'
import type { ReceiveContentWarning } from '../../transfer/lifecycle/content-warning'

import { resolveTaskStage, type StageCopy } from './stage'

const FALLBACK_OPERATION_LABEL_LENGTH = 8

export function presentTask(facts: TaskFacts, language: Language = 'en'): TaskPresentation {
  const t = receiverCopy[language].progress
  const copy = resolveTaskStage(facts, language)
  const actions: readonly TaskAction[] = facts.actions.map(action => action.disabledReason === null ? action :
    Object.freeze({ ...action, disabledReason: receiverCopy[language].admission[action.disabledReason] }))
  const usableActions = actions.filter(action => !action.destructive)
  const primaryAction = facts.execution.kind === 'local-finalization' ? null : preferredAction(usableActions, copy.stage)
  const attention = copy.stage === 'needs-action' || copy.stage === 'ready-to-save' ||
    facts.completeness === 'partial'
  const details = [...facts.details]
  const progress = presentTaskProgress(facts, language)
  const showProgress = ['preparing', 'downloading', 'waiting', 'paused', 'finishing'].includes(copy.stage)
  if (!showProgress && progress !== null) details.push(progress.label, ...progress.details)
  if (facts.lifecycle.contentWarning !== undefined) {
    details.unshift(...contentWarningDetails(facts.lifecycle.contentWarning, facts.lifecycle.kind === 'resumable-receive', language))
  } else if (facts.completeness === 'partial') details.unshift(t.partial)
  if (facts.safeRenamed !== null && facts.safeRenamed.count > 0) {
    details.push(receiverCopy[language].downloads.adjustedCount(facts.safeRenamed.count))
    details.push(t.adjustedHint)
  }
  if (facts.lifecycle.kind === 'published' && facts.lifecycle.cleanupState === 'cleanup-pending') {
    details.push(t.pendingCleanup)
  }
  const transition = transitionFor(facts, copy, attention)
  return Object.freeze({
    operationId: facts.lifecycle.operationId,
    generation: facts.lifecycle.generation,
    objectLabel: facts.display === null ? receiverCopy[language].downloads.unnamedTask(facts.lifecycle.operationId.slice(0, FALLBACK_OPERATION_LABEL_LENGTH))
      : objectLabel(facts.display.objectLabel, receiverCopy[language].browse),
    destinationLabel: facts.display?.destinationLabel === undefined ? null : destinationLabel(facts.display.destinationLabel, language),
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
    destructiveActions: Object.freeze(actions.filter(action => action.destructive)),
    details: Object.freeze(details),
    safeRenamed: facts.safeRenamed,
    completeness: facts.completeness,
    publication: facts.publication,
    transition,
  })
}

function contentWarningDetails(warning: ReceiveContentWarning, paused: boolean, language: Language): readonly string[] {
  const t = receiverCopy[language].progress
  const missing = warning.selectedFileCount - warning.completedFileCount
  const details = [t.missingFileCount(warning.completedFileCount, warning.selectedFileCount, missing, paused)]
  const reasons = { 'source-changed': t.sourceChanged, 'source-unavailable': t.sourceUnavailable, 'file-failed': t.fileFailed }
  for (const file of warning.missingFiles) {
    details.push(t.missingFile(file.path.join('/'), reasons[file.reason], paused))
  }
  const additional = missing - BigInt(warning.missingFiles.length)
  if (additional > 0n) details.push(t.moreMissing(additional, paused))
  return details
}

function taskTone(stage: TaskStage, attention: boolean, facts: TaskFacts): TaskPresentation['tone'] {
  if (stage === 'failed') return 'critical'
  if (attention || stage === 'paused' || stage === 'waiting') return 'warning'
  return facts.publication === 'saved' ? 'positive' : 'neutral'
}

function preferredAction(actions: readonly TaskAction[], taskStage: TaskStage): TaskAction | null {
  let preferred = ['continue', 'save', 'redownload', 'save-partial']
  if (taskStage === 'ready-to-save') preferred = ['save', 'save-partial']
  if (taskStage === 'downloading' || taskStage === 'waiting') preferred = ['pause', 'continue']
  for (const id of preferred) {
    const action = actions.find(candidate => candidate.id === id && candidate.disabledReason === null)
    if (action !== undefined) return action
  }
  return actions.find(action => action.id !== 'forget' && action.disabledReason === null) ??
    actions.find(action => action.id !== 'forget') ?? null
}

function transitionFor(facts: TaskFacts, copy: StageCopy, attention: boolean): TaskPresentationTransition {
  const actionFacts = facts.actions.map(action => `${action.id}:${action.disabledReason ?? ''}`).join('|')
  return Object.freeze({
    name: 'receiver.task.presentation',
    operation_id: facts.lifecycle.operationId,
    generation: facts.lifecycle.generation,
    stage: copy.stage, reason: copy.reason,
    completeness: facts.completeness, publication: facts.publication, attention,
    fingerprint: [facts.lifecycle.operationId, copy.stage, copy.reason, facts.completeness,
      facts.publication, attention, (facts.safeRenamed?.count ?? 0) > 0, actionFacts,
      receiveElapsedMilliseconds(facts.lifecycle.timing)].join(':'),
  })
}

/** Lifecycle generations may change at every checkpoint; only semantic changes are logged. */
export function compareTaskTransition(
  previous: TaskPresentationTransition | null,
  current: TaskPresentationTransition,
): TaskPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
