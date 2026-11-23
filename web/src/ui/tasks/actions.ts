import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { ReceiveAction, ReceiveBlockReason, RetainedTaskSnapshot } from '../../application/receive/contracts'
import type { SaveBackendKind } from '../../output/model'
import type { ReceiveLifecycleKind } from '../../transfer/state'
import type { TaskAction, TaskRecoveryReadiness } from './model'

export function activeTaskAction(
  action: ReceiveAction,
  lifecycleKind: ReceiveLifecycleKind,
  backend: SaveBackendKind,
  language: Language = 'en',
): TaskAction<ReceiveBlockReason> {
  return Object.freeze({
    id: action,
    label: actionLabel(action, lifecycleKind, backend, language),
    destructive: isDestructiveAction(action),
    disabledReason: null,
    consequence: actionConsequence(action, lifecycleKind, backend, language),
    target: Object.freeze({ kind: 'active', action }),
  })
}

export function retainedTaskAction(
  task: RetainedTaskSnapshot,
  action: ReceiveAction,
  readiness: TaskRecoveryReadiness,
  language: Language = 'en',
): TaskAction<ReceiveBlockReason> {
  let disabledReason: ReceiveBlockReason | null = null
  if (action === 'continue' && (readiness === 'original-link-required' || readiness === 'different-share')) {
    disabledReason = 'matchingShare'
  }
  if (task.activeElsewhere) disabledReason = 'activeInAnotherTab'

  return Object.freeze({
    id: action,
    label: actionLabel(action, task.lifecycle.kind, task.backend, language),
    destructive: isDestructiveAction(action),
    disabledReason,
    consequence: actionConsequence(action, task.lifecycle.kind, task.backend, language),
    target: Object.freeze({ kind: 'retained', task, action }),
  })
}

function isDestructiveAction(action: ReceiveAction): boolean {
  return action === 'discard'
}

function hasSavedBrowserCopy(lifecycleKind: ReceiveLifecycleKind, backend: SaveBackendKind): boolean {
  return lifecycleKind === 'ready-to-save' ||
    lifecycleKind === 'download-started' ||
    (lifecycleKind === 'partial' && backend === 'opfs')
}

function actionLabel(
  action: ReceiveAction,
  lifecycleKind: ReceiveLifecycleKind,
  backend: SaveBackendKind,
  language: Language,
): string {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'pause': return t.pause
    case 'stop': return t.stop
    case 'continue': return t.resume
    case 'save': return lifecycleKind === 'download-started' ? t.saveAgain : t.save
    case 'save-partial': return t.savePartial
    case 'discard': return hasSavedBrowserCopy(lifecycleKind, backend) ? t.deleteSavedCopy : t.discardOutput
    case 'forget': return t.forget
  }
}

function actionConsequence(
  action: ReceiveAction,
  lifecycleKind: ReceiveLifecycleKind,
  backend: SaveBackendKind,
  language: Language,
): string | null {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'pause': return t.pauseHint
    case 'stop':
      if (backend === 'fsa') return t.stopFolder
      return backend === 'opfs' ? t.stopWorkspace : t.stopPortable
    case 'save': return lifecycleKind === 'download-started' ? t.saveAgainHint : null
    // Only paused tasks remain resumable after a partial export.
    case 'save-partial': return lifecycleKind === 'paused' ? t.savePartialHint : null
    case 'discard': return hasSavedBrowserCopy(lifecycleKind, backend) ? t.deleteSavedCopyHint : t.discardOutputHint
    case 'forget': return t.forgetHint
    default: return null
  }
}
