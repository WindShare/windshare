import type { TaskAction, TaskFacts, TaskPresentation, TaskPresentationTransition, TaskStage } from './model'
import { presentTaskProgress } from './progress'

import { resolveTaskStage, type StageCopy } from './stage'

const FALLBACK_OPERATION_LABEL_LENGTH = 8

export function presentTask(facts: TaskFacts): TaskPresentation {
  const copy = resolveTaskStage(facts)
  const fidelityPending = facts.fidelity?.actionMode === 'catch-up-required'
  const usableActions = facts.actions.filter(action => !action.destructive)
  const primaryAction = facts.execution.kind === 'local-finalization' ? null : preferredAction(usableActions, copy.stage)
  const attention = copy.stage === 'needs-action' || copy.stage === 'ready-to-save' || fidelityPending ||
    facts.completeness === 'partial'
  const details = [...facts.details]
  const progress = presentTaskProgress(facts)
  const showProgress = ['preparing', 'downloading', 'waiting', 'paused', 'finishing'].includes(copy.stage)
  if (!showProgress && progress !== null) details.push(progress.label, ...progress.details)
  if (facts.completeness === 'partial') details.unshift('Partial result: some selected items are missing or unfinished.')
  if (facts.fidelity !== null) {
    const count = facts.fidelity.replacementCount
    details.push(`${count} ${count === 1 ? 'filename was' : 'filenames were'} adjusted for this device.`)
    details.push('Adjusted paths may affect projects or scripts that expect the original names.')
    if (fidelityPending) details.push('Finish local filename restoration setup before running the restoration tool.')
  }
  if (facts.lifecycle.kind === 'published' && facts.lifecycle.cleanupState === 'cleanup-pending') {
    details.push('The result is saved. Cleanup of owned temporary data is still pending.')
  }
  const transition = transitionFor(facts, copy, attention)
  return Object.freeze({
    operationId: facts.lifecycle.operationId,
    generation: facts.lifecycle.generation,
    objectLabel: facts.display?.objectLabel ?? `Download ${facts.lifecycle.operationId.slice(0, FALLBACK_OPERATION_LABEL_LENGTH)}`,
    destinationLabel: facts.display?.destinationLabel ?? null,
    createdAtMilliseconds: facts.display?.createdAtMilliseconds ?? null,
    stage: copy.stage,
    headline: copy.headline,
    description: copy.description,
    tone: taskTone(copy.stage, attention, facts),
    attention,
    progress: showProgress ? progress : null,
    primaryAction,
    secondaryActions: Object.freeze(usableActions.filter(action => action !== primaryAction)),
    destructiveActions: Object.freeze(facts.actions.filter(action => action.destructive)),
    details: Object.freeze(details),
    fidelity: facts.fidelity,
    completeness: facts.completeness,
    publication: facts.publication,
    transition,
  })
}

function taskTone(stage: TaskStage, attention: boolean, facts: TaskFacts): TaskPresentation['tone'] {
  if (stage === 'failed') return 'critical'
  if (attention || stage === 'paused' || stage === 'waiting') return 'warning'
  return facts.publication === 'saved' ? 'positive' : 'neutral'
}

function preferredAction(actions: readonly TaskAction[], taskStage: TaskStage): TaskAction | null {
  let preferred = ['continue', 'save', 'catch-up', 'redownload', 'save-partial']
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
      facts.publication, attention, facts.fidelity?.actionMode,
      (facts.fidelity?.replacementCount ?? 0) > 0, actionFacts].join(':'),
  })
}

/** Lifecycle generations may change at every checkpoint; only semantic changes are logged. */
export function compareTaskTransition(
  previous: TaskPresentationTransition | null,
  current: TaskPresentationTransition,
): TaskPresentationTransition | null {
  return previous?.fingerprint === current.fingerprint ? null : current
}
