import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { ReceiveAction, ReceiveBlockReason, RetainedTaskSnapshot } from '../../application/receive/contracts'
import type { SaveBackendKind } from '../../output/model'
import type { TaskAction, TaskRecoveryReadiness } from './model'

export function activeTaskAction(
  action: ReceiveAction,
  backend: SaveBackendKind,
  language: Language = 'en',
): TaskAction<ReceiveBlockReason> {
  return Object.freeze({
    id: action,
    label: actionLabel(action, language),
    destructive: isDestructiveAction(action),
    disabledReason: null,
    consequence: actionConsequence(action, backend, language),
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
    label: actionLabel(action, language),
    destructive: isDestructiveAction(action),
    disabledReason,
    consequence: actionConsequence(action, task.backend, language),
    target: Object.freeze({ kind: 'retained', task, action }),
  })
}

function isDestructiveAction(action: ReceiveAction): boolean {
  return action === 'discard' || action === 'delete'
}

function actionLabel(action: ReceiveAction, language: Language): string {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'pause': return t.pause
    case 'stop': return receiverCopy[language].lifecycle.stopAction
    case 'continue': return t.resume
    case 'save': return t.save
    case 'save-partial': return t.savePartial
    case 'redownload': return t.downloadAgain
    case 'discard': return t.discardOutput
    case 'delete': return t.deleteRetained
    case 'forget': return t.forget
  }
}

function actionConsequence(
  action: ReceiveAction,
  backend: SaveBackendKind,
  language: Language,
): string | null {
  const t = receiverCopy[language].actions
  switch (action) {
    case 'pause': return t.pauseHint
    case 'stop':
      if (backend === 'fsa') return t.stopFolder
      return backend === 'opfs' ? t.stopWorkspace : t.stopPortable
    case 'save-partial': return t.savePartialHint
    case 'redownload': return t.saveAgainHint
    case 'discard': return t.discardOutputHint
    case 'delete': return t.deleteRetainedHint
    case 'forget': return t.forgetHint
    default: return null
  }
}
