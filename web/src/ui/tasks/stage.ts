import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { TaskBlocking, TaskFacts, TaskStage } from './model'
import { browserDeliveryLocalActions } from '../../output/model/browser-delivery-actions'
import { destinationLabel } from './display'

export interface StageCopy {
  readonly stage: TaskStage
  readonly headline: string
  readonly description: string
  readonly reason: string
}

export function resolveTaskStage(facts: TaskFacts, language: Language = 'en'): StageCopy {
  const t = receiverCopy[language].stage
  const state = facts.lifecycle
  if (facts.execution.kind === 'local-finalization') return stage('finishing', t.finishingLocally,
    t.finishingLocallyHint, 'local-finalization-active')
  const folderDelivery = folderDeliveryStage(facts, language)
  if (folderDelivery !== null) return folderDelivery
  if (state.kind === 'published' || state.kind === 'partial-directory') {
    return savedStage(facts, language)
  }
  if (state.kind === 'download-started') return stage('handed-to-browser',
    facts.completeness === 'partial' ? t.partialHandoff
      : t.handoff,
    t.handoffHint, state.kind)
  if (state.kind === 'waiting-to-save') return stage('ready-to-save', t.readyToSave,
    facts.completeness === 'partial'
      ? t.partialLocal
      : t.resultLocal, state.kind)
  if (facts.execution.kind === 'retained') return retainedStage(facts, facts.execution.continuation, language)
  if (facts.interruption === 'finish') return stage('finishing', t.slowFinish,
    t.slowFinishHint,
    'settlement-deadline')
  if (facts.interruption !== null) return stage('finishing',
    facts.interruption === 'pause' ? t.pausing : t.stopping,
    t.settling, facts.interruption)
  if (state.kind === 'receiving') return activeReceivingStage(facts, language)
  return executionStage(facts, language)
}

function folderDeliveryStage(facts: TaskFacts, language: Language): StageCopy | null {
  const t = receiverCopy[language].stage
  const delivery = facts.browserDelivery
  if (delivery != null && delivery.copyingFiles > 0 && facts.execution.kind === 'active') {
    return stage('finishing', t.savingToFolder,
      t.savingToFolderHint,
      'browser-folder-copying')
  }
  const localActions = browserDeliveryLocalActions(facts.lifecycle, delivery)
  if (localActions.includes('save-staged-files')) {
    return stage('ready-to-save', t.receivedReady,
      t.receivedReadyHint,
      'browser-folder-local-save')
  }
  if (localActions.includes('cleanup-staging')) return stage('needs-action', t.cleanupNeeded,
    t.cleanupNeededHint, 'browser-folder-cleanup')
  if (localActions.includes('discard-incomplete-staging')) return stage('needs-action', t.incompleteData,
    t.incompleteDataHint,
    'browser-folder-incomplete-staging')
  return null
}

function savedStage(facts: TaskFacts, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  const description = facts.display?.destinationLabel === undefined
    ? t.savedHint : t.savedTo(destinationLabel(facts.display.destinationLabel, language))
  return stage('saved', facts.completeness === 'partial' ? t.partiallySaved : t.saved, description, facts.lifecycle.kind)
}

function retainedStage(facts: TaskFacts, continuation: import('../../output/model/continuation').ReceiveOperationContinuation, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  switch (continuation) {
    case 'resume-start': return startupStage(facts, language)
    case 'cleanup-only': return executionStage(facts, language)
    case 'resume-receive': return pausedStage(facts, language)
    case 'resume-package':
    case 'resume-local-finalization': return stage('paused', t.localFinishReady,
      t.localFinishReadyHint, continuation)
    case 'save-artifact':
    case 'retry-download': return stage('ready-to-save', t.readyToSave,
      t.retainedReadyHint, continuation)
    case 'pending-catch-up': return stage('needs-action', t.setupNames,
      t.setupNamesHint, continuation)
    case 'restoration-available': return stage('needs-action', t.reviewOutput,
      t.reviewOutputHint, continuation)
    case 'cleanup-incompatible': return stage('needs-action', t.removeRecord,
      t.removeRecordHint, continuation)
    case 'retry-cleanup': return stage('needs-action', t.cleanup,
      t.cleanupHint, continuation)
    case 'needs-attention': return stage('needs-action', t.needsAction,
      t.needsActionHint, continuation)
    case 'history-only': throw new TypeError('History-only operation requires settled publication facts')
  }
}

function blockingStage(blocking: TaskBlocking, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  switch (blocking.kind) {
    case 'reconnecting': return stage('waiting', t.reconnecting,
      blocking.description ?? t.reconnectKeepOpen, blocking.kind)
    case 'unavailable': return stage('needs-action', t.unavailable,
      blocking.description ?? t.unavailableHint, blocking.kind)
    case 'sender-capacity': return stage('waiting', t.senderCapacity,
      blocking.description ?? t.senderCapacityHint, blocking.kind)
    case 'relay-bandwidth': return stage('waiting', t.relayBandwidth,
      blocking.description ?? t.relayBandwidthHint, blocking.kind)
    case 'share-ended': return stage('needs-action', t.shareEnded,
      blocking.description ?? t.shareEndedHint, blocking.kind)
    case 'storage': return stage('needs-action', t.storage,
      blocking.description ?? t.storageHint, blocking.kind)
  }
}

function executionStage(facts: TaskFacts, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  const state = facts.lifecycle
  switch (state.kind) {
    case 'intent-frozen':
    case 'preparing': return stage('preparing', t.preparing, t.preparingHint, state.kind)
    case 'receiving': return receivingStage(language)
    case 'resumable-start': return startupStage(facts, language)
    case 'resumable-receive':
    case 'resumable-package': return pausedStage(facts, language)
    case 'finalizing-tree':
    case 'materialization-sealed':
    case 'packaging':
    case 'artifact-sealed':
    case 'publishing-managed':
    case 'handing-off': return finishingStage(state.kind, language)
    case 'needs-attention': return stage('needs-action', t.needsAction, attentionDescription(state.reason, language), state.reason)
    case 'restart-required': return stage('failed', t.newAttempt, restartDescription(state.reason, language), state.reason)
    case 'source-invalidated': return stage('failed', t.sourceChanged, receiverCopy[language].lifecycle.sourceChangedHint, state.kind)
    case 'discarded': return stage('cancelled', t.cancelled, t.discardedHint, state.kind)
    default: throw new TypeError('settled task must be resolved before execution stage')
  }
}

function activeReceivingStage(facts: TaskFacts, language: Language): StageCopy {
  if (facts.progress?.phase === 'finishing') return finishingStage('local-finalization', language)
  if (facts.blocking !== null) return blockingStage(facts.blocking, language)
  return receivingStage(language)
}

function startupStage(facts: TaskFacts, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  if (facts.readiness === 'original-link-required' || facts.readiness === 'different-share') {
    return stage('needs-action', t.openToRetry,
      t.openToRetryHint, facts.readiness)
  }
  const paused = facts.lifecycle.kind === 'resumable-start' && facts.lifecycle.reason === 'paused'
  return stage(paused ? 'paused' : 'needs-action', paused ? t.pausedAtStart : t.startFailed,
    t.startRetained, 'resumable-start')
}

function receivingStage(language: Language): StageCopy {
  const t = receiverCopy[language].stage
  return stage('downloading', t.downloading, t.downloadingHint, 'receiving')
}

function pausedStage(facts: TaskFacts, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  const state = facts.lifecycle
  if (state.kind !== 'resumable-package' && facts.blocking !== null &&
      (facts.blocking.kind === 'share-ended' || facts.blocking.kind === 'unavailable')) return blockingStage(facts.blocking, language)
  if (facts.readiness === 'original-link-required' || facts.readiness === 'different-share') {
    return stage('needs-action', t.openToContinue,
      t.openToContinueHint, facts.readiness)
  }
  if (state.kind === 'resumable-receive' && state.payloadKind === 'workspace-archive' && state.pauseReason === 'storage-pressure') {
    return stage('needs-action', t.storageFull, t.storageFullHint, 'storage-pressure')
  }
  return stage('paused', t.paused, facts.readiness === 'local'
    ? t.localOnly
    : t.pausedHint, state.kind)
}

function finishingStage(kind: string, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  let headline = t.finishing
  if (kind === 'packaging') headline = t.packaging
  if (kind === 'handing-off') headline = t.startingBrowser
  return stage('finishing', headline, t.finishingHint, kind)
}

function attentionDescription(reason: string, language: Language): string {
  const t = receiverCopy[language].stage
  if (reason === 'publication-unknown') return t.unconfirmed
  if (reason === 'target-ownership-unknown') return t.unknownOwner
  return t.unknownCleanup
}

function restartDescription(reason: string, language: Language): string {
  const t = receiverCopy[language].stage
  if (reason === 'content-session-ended') return t.sessionEnded
  return t.restart
}

function stage(taskStage: TaskStage, headline: string, description: string, reason: string): StageCopy {
  return { stage: taskStage, headline, description, reason }
}
