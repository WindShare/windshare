import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'
import type { TaskBlocking, TaskFacts, TaskStage } from './model'
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

  if (facts.interruption !== null) {
    return stage('finishing', facts.interruption === 'pause' ? t.pausing : t.stopping, t.settling, facts.interruption)
  }

  switch (state.kind) {
    case 'preparing':
      return stage('preparing', t.preparing, t.preparingHint, state.kind)
    case 'downloading':
      return downloadingStage(facts, state, language)
    case 'paused':
      return pausedStage(facts, state, language)
    case 'ready-to-save':
      return stage(
        'ready-to-save',
        t.readyToSave,
        facts.completeness === 'partial' ? t.partialLocal : t.resultLocal,
        state.kind,
      )
    case 'download-started':
      return stage(
        'handed-to-browser',
        facts.completeness === 'partial' ? t.partialHandoff : t.handoff,
        t.handoffHint,
        state.kind,
      )
    case 'saved':
    case 'partial':
      return savedOrPartialStage(facts, state, language)
    case 'failed':
      return failedStage(state.reason, language)
  }
}

function savedOrPartialStage(
  facts: TaskFacts,
  state: Extract<TaskFacts['lifecycle'], { kind: 'saved' | 'partial' }>,
  language: Language,
): StageCopy {
  const t = receiverCopy[language].stage
  if (state.kind === 'partial' && facts.backend === 'opfs') {
    return stage('ready-to-save', t.readyToSave, t.partialLocal, state.kind)
  }
  const description = facts.display?.destinationLabel === undefined
    ? t.savedHint
    : t.savedTo(destinationLabel(facts.display.destinationLabel, language))
  const headline = state.kind === 'partial' || facts.completeness === 'partial' ? t.partiallySaved : t.saved
  return stage('saved', headline, description, state.kind)
}

function downloadingStage(
  facts: TaskFacts,
  state: Extract<TaskFacts['lifecycle'], { kind: 'downloading' }>,
  language: Language,
): StageCopy {
  const t = receiverCopy[language].stage
  if (state.phase === 'finalizing' || facts.progress?.phase === 'finishing') {
    return stage('finishing', t.finishing, t.finishingHint, 'finalizing')
  }
  if (facts.outageCheckpointed && facts.blocking?.kind === 'reconnecting') {
    return stage('waiting', t.reconnectProgressSaved, t.reconnectAutomatically, 'reconnecting-checkpointed')
  }
  if (facts.outageCheckpointed && facts.blocking?.kind === 'relay-bandwidth') {
    return stage('waiting', t.bandwidthProgressSaved, facts.blocking.description ?? t.relayBandwidthHint, 'relay-bandwidth-checkpointed')
  }
  if (facts.blocking !== null) return blockingStage(facts.blocking, language)
  return stage('downloading', t.downloading, t.downloadingHint, 'downloading')
}

function pausedStage(
  facts: TaskFacts,
  state: Extract<TaskFacts['lifecycle'], { kind: 'paused' }>,
  language: Language,
): StageCopy {
  const t = receiverCopy[language].stage
  if (facts.blocking !== null && (facts.blocking.kind === 'share-ended' || facts.blocking.kind === 'unavailable')) {
    return blockingStage(facts.blocking, language)
  }
  if (facts.readiness === 'original-link-required' || facts.readiness === 'different-share') {
    return stage('needs-action', t.openToContinue, t.openToContinueHint, facts.readiness)
  }
  if (state.reason === 'storage-full') {
    return stage('needs-action', t.storageFull, t.storageFullHint, 'storage-full')
  }
  if (state.reason === 'permission-needed') {
    return stage('needs-action', t.permissionNeeded, t.permissionNeededHint, 'permission-needed')
  }
  return stage('paused', t.paused, t.pausedHint, state.reason)
}

function failedStage(
  reason: Extract<TaskFacts['lifecycle'], { kind: 'failed' }>['reason'],
  language: Language,
): StageCopy {
  const t = receiverCopy[language].stage
  if (reason === 'source-changed') {
    return stage('failed', t.sourceChanged, receiverCopy[language].failures.sourceChangedHint, 'source_invalidated')
  }
  if (reason === 'cancelled') {
    return stage('cancelled', t.cancelled, t.discardedHint, reason)
  }
  if (reason === 'session-ended') {
    return stage('failed', t.newAttempt, t.sessionEnded, reason)
  }
  return stage('failed', t.newAttempt, t.restart, reason)
}

function blockingStage(blocking: TaskBlocking, language: Language): StageCopy {
  const t = receiverCopy[language].stage
  switch (blocking.kind) {
    case 'reconnecting': return stage('waiting', t.reconnecting, blocking.description ?? t.reconnectKeepOpen, blocking.kind)
    case 'unavailable': return stage('needs-action', t.unavailable, blocking.description ?? t.unavailableHint, blocking.kind)
    case 'sender-capacity': return stage('waiting', t.senderCapacity, blocking.description ?? t.senderCapacityHint, blocking.kind)
    case 'relay-bandwidth': return stage('waiting', t.relayBandwidth, blocking.description ?? t.relayBandwidthHint, blocking.kind)
    case 'share-ended': return stage('needs-action', t.shareEnded, blocking.description ?? t.shareEndedHint, blocking.kind)
    case 'storage': return stage('needs-action', t.storage, blocking.description ?? t.storageHint, blocking.kind)
  }
}

function stage(taskStage: TaskStage, headline: string, description: string, reason: string): StageCopy {
  return { stage: taskStage, headline, description, reason }
}
