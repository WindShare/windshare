import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { ReceiveBlockReason } from '../../application/receive/admission'
import type { LifecycleActionPresentation } from '../lifecycle-presentation'
import type { RetainedReceiveAction, RetainedReceiveOperation } from '../../application/receive/contracts'
import type { TaskAction, TaskRecoveryReadiness } from './model'

export function activeTaskAction(action: LifecycleActionPresentation, planKind: string, language: Language = 'en'): TaskAction<ReceiveBlockReason> {
  const t = receiverCopy[language].actions
  return Object.freeze({
    id: action.kind,
    label: action.kind === 'pause' ? t.pause : action.label,
    destructive: action.destructive,
    disabledReason: null,
    consequence: activeConsequence(action, planKind, language),
    target: Object.freeze({ kind: 'active', action: action.kind }),
  })
}

function activeConsequence(action: LifecycleActionPresentation, planKind: string, language: Language): string | null {
  const t = receiverCopy[language].actions
  if (action.kind === 'save-staged-files') return t.saveStagedHint
  if (action.kind === 'cleanup-staging') return t.cleanupStagingHint
  if (action.kind === 'discard-incomplete-staging') return incompleteStagingConsequence(language)
  if (action.kind === 'pause') return t.pauseHint
  if (action.kind === 'stop') return stopConsequence(planKind, language)
  if (action.kind === 'delete' || action.kind === 'discard') {
    return t.deleteHint
  }
  return action.kind === 'redownload' && action.destructive
    ? t.restartHint : null
}

export function retainedTaskAction(
  operation: RetainedReceiveOperation,
  action: RetainedReceiveAction,
  readiness: TaskRecoveryReadiness,
  language: Language = 'en',
): TaskAction<ReceiveBlockReason> {
  const remote = action === 'continue' || (action === 'redownload' && operation.recoverySummary !== undefined)
  let disabledReason: ReceiveBlockReason | null = null
  if (remote && (readiness === 'original-link-required' || readiness === 'different-share')) {
    disabledReason = 'matchingShare'
  } else if (remote) {
    disabledReason = operation.unavailableReason ?? null
  }
  const cleanupOnly = operation.continuation === 'retry-cleanup'
  const destructive = action === 'discard' || action === 'delete' || action === 'discard-incomplete-staging' ||
    (action === 'redownload' && operation.recoverySummary !== undefined)
  return Object.freeze({
    id: action,
    label: retainedLabel(operation, action, readiness, language),
    destructive: action === 'discard-incomplete-staging' || (destructive && !cleanupOnly),
    disabledReason,
    consequence: retainedConsequence(operation, action, language),
    target: Object.freeze({ kind: 'retained', operation, action }),
  })
}

function stopConsequence(planKind: string, language: Language): string {
  const t = receiverCopy[language].actions
  switch (planKind) {
    case 'direct-tree': return t.stopFolder
    case 'direct-resumable-zip': return t.stopZip
    case 'workspace-then-publish': return t.stopWorkspace
    default: return t.stopPortable
  }
}

function retainedLabel(
  operation: RetainedReceiveOperation,
  action: RetainedReceiveAction,
  readiness: TaskRecoveryReadiness,
  language: Language,
): string {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'save-staged-files': return t.saveStaged
    case 'cleanup-staging': return t.cleanupStaging
    case 'discard-incomplete-staging': return t.discardStaging
    case 'save-partial': return t.savePartial
    case 'catch-up': return t.finishRestoration
    case 'continue': return continuationLabel(operation, readiness, language)
    case 'save': return t.save
    case 'redownload': return operation.recoverySummary === undefined ? t.downloadAgain : t.restartIncomplete
    case 'forget': return t.forget
    case 'discard': return operation.continuation === 'cleanup-incompatible' ? t.deleteHistory : t.discardOutput
    case 'delete':
      if (operation.continuation === 'retry-cleanup') return t.retryCleanup
      if (['resume-direct-zip', 'reauthorize-direct-zip', 'verify-direct-zip-target', 'retry-direct-zip-space'].includes(operation.continuation)) return t.deleteZip
      return t.deleteRetained
  }
}

function continuationLabel(operation: RetainedReceiveOperation, readiness: TaskRecoveryReadiness, language: Language): string {
  const t = receiverCopy[language].actions
  if (operation.continuation === 'resume-start') return operation.lifecycle.kind === 'resumable-start' &&
    operation.lifecycle.reason === 'paused' ? t.resume : t.retryDownload
  if (readiness === 'destination-authorization-required') return t.authorizeContinue
  if (operation.continuation === 'resume-local-finalization' || operation.continuation === 'resume-package') return t.finishSave
  if (operation.continuation === 'verify-direct-zip-target') return t.verifyContinue
  if (operation.continuation === 'verify-direct-zip-completion') return t.verifyZip
  if (operation.continuation === 'retry-direct-zip-space') return t.retrySpace
  return operation.recoverySummary === undefined ? t.resume : t.resumePartial
}

function retainedConsequence(operation: RetainedReceiveOperation, action: RetainedReceiveAction, language: Language): string | null {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'save-staged-files': return t.saveStagedHint
    case 'cleanup-staging': return t.cleanupStagingHint
    case 'discard-incomplete-staging': return incompleteStagingConsequence(language)
    case 'save-partial':
      return t.savePartialHint
    case 'forget': return t.forgetHint
    case 'discard':
      return operation.continuation === 'cleanup-incompatible'
        ? t.deleteHistoryHint
        : t.discardOutputHint
    case 'delete':
      return operation.continuation === 'retry-cleanup'
        ? t.retryCleanupHint
        : t.deleteRetainedHint
    case 'redownload':
      return operation.recoverySummary === undefined ? null
        : t.restartIncompleteHint
    default: return null
  }
}

function incompleteStagingConsequence(language: Language): string {
  const t = receiverCopy[language].actions
  return t.discardStagingHint
}
