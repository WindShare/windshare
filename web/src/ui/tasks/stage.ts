import type { TaskBlocking, TaskFacts, TaskStage } from './model'

export interface StageCopy {
  readonly stage: TaskStage
  readonly headline: string
  readonly description: string
  readonly reason: string
}

export function resolveTaskStage(facts: TaskFacts): StageCopy {
  const state = facts.lifecycle
  if (facts.execution.kind === 'local-finalization') return stage('finishing', 'Finishing locally',
    'Retained content is being finalized and saved without reconnecting to the sender.', 'local-finalization-active')
  if (state.kind === 'published' || state.kind === 'partial-directory') {
    return savedStage(facts)
  }
  if (state.kind === 'download-started') return stage('handed-to-browser',
    'Download started — check browser downloads',
    'The browser took over. WindShare cannot confirm where or whether the file was saved.', state.kind)
  if (state.kind === 'waiting-to-save') return stage('ready-to-save', 'Ready to save',
    facts.completeness === 'partial'
      ? 'The partial result is retained locally. Save it without reconnecting to the sender.'
      : 'The result is retained locally. Save it without reconnecting to the sender.', state.kind)
  if (facts.execution.kind === 'retained') return retainedStage(facts, facts.execution.continuation)
  if (facts.interruption === 'finish') return stage('finishing', 'Finishing is taking longer than expected',
    'WindShare is waiting for current save operations to finish safely. Keep this page open; you can continue browsing.',
    'settlement-deadline')
  if (facts.interruption !== null) return stage('finishing',
    facts.interruption === 'pause' ? 'Pausing' : 'Stopping',
    'Accepted writes and recovery records are settling before this task releases its destination.', facts.interruption)
  if (state.kind === 'receiving') return activeReceivingStage(facts)
  return executionStage(facts)
}

function savedStage(facts: TaskFacts): StageCopy {
  const description = facts.display?.destinationLabel === undefined
    ? 'The result is saved at the authorized destination.' : `Saved to ${facts.display.destinationLabel}.`
  return stage('saved', facts.completeness === 'partial' ? 'Saved with missing items' : 'Saved', description, facts.lifecycle.kind)
}

function retainedStage(facts: TaskFacts, continuation: import('../../output/resume/descriptor').ReceiveOperationContinuation): StageCopy {
  switch (continuation) {
    case 'resume-receive':
    case 'resume-direct-zip': return pausedStage(facts)
    case 'resume-package':
    case 'resume-local-finalization': return stage('paused', 'Ready to finish locally',
      'Retained content can finish locally without the sender. Choose Finish and save to continue.', continuation)
    case 'save-artifact':
    case 'retry-download': return stage('ready-to-save', 'Ready to save',
      'The retained result can be saved without reconnecting to the sender.', continuation)
    case 'pending-catch-up': return stage('needs-action', 'Finish filename restoration setup',
      'Local restoration records must finish before the restoration tool can run.', continuation)
    case 'restoration-available': return stage('needs-action', 'Review retained output',
      'Receiving is inactive. Review the retained output and available filename restoration actions.', continuation)
    case 'cleanup-incompatible': return stage('needs-action', 'Saved record needs removal',
      'This record cannot be continued. Removing it leaves exported files untouched.', continuation)
    case 'retry-cleanup': return stage('needs-action', 'Cleanup needs attention',
      'Review the retained result before retrying cleanup of owned temporary data.', continuation)
    case 'reauthorize-direct-zip': return stage('needs-action', 'Authorize the save destination',
      'Authorize the same destination to continue the unfinished ZIP.', continuation)
    case 'verify-direct-zip-target': return stage('needs-action', 'Verify the save destination',
      'Ownership must be verified before the unfinished ZIP can change.', continuation)
    case 'verify-direct-zip-completion': return stage('needs-action', 'Verify the saved ZIP',
      'Check the local result without reconnecting. Any unfinished content can then resume from its saved progress.', continuation)
    case 'retry-direct-zip-space': return stage('needs-action', 'Free space at the destination',
      'Free destination space, then retry from the retained resume position.', continuation)
    case 'needs-attention': return stage('needs-action', 'Needs action',
      'The retained output requires an ownership or recovery decision before continuing.', continuation)
    case 'history-only': throw new TypeError('History-only operation requires settled publication facts')
  }
}

function blockingStage(blocking: TaskBlocking): StageCopy {
  switch (blocking.kind) {
    case 'reconnecting': return stage('waiting', 'Reconnecting to the sender',
      blocking.description ?? 'WindShare will reconnect automatically. Browsing and retained output stay available.', blocking.kind)
    case 'unavailable': return stage('needs-action', 'Connection unavailable',
      blocking.description ?? 'The connection could not be restored. Retained progress remains available; reopen the original link to reconnect.', blocking.kind)
    case 'sender-capacity': return stage('waiting', 'Waiting for sender capacity',
      blocking.description ?? 'The download continues automatically when the sender has capacity.', blocking.kind)
    case 'share-ended': return stage('needs-action', 'The share has ended',
      blocking.description ?? 'Remote content needs a new share link. Retained local results remain available.', blocking.kind)
    case 'storage': return stage('needs-action', 'More storage is needed',
      blocking.description ?? 'Free space before continuing. Verified progress is retained.', blocking.kind)
  }
}

function executionStage(facts: TaskFacts): StageCopy {
  const state = facts.lifecycle
  switch (state.kind) {
    case 'intent-frozen':
    case 'preparing': return stage('preparing', 'Preparing download', 'Preparing the requested result and its authorized destination.', state.kind)
    case 'receiving': return receivingStage(facts)
    case 'resumable-receive':
    case 'resumable-package': return pausedStage(facts)
    case 'authorization-required': return stage('needs-action', 'Authorize the save destination',
      'Authorize the same destination to continue; this does not choose a different output.', state.kind)
    case 'target-verification-required': return stage('needs-action', 'Verify the save destination',
      'Ownership must be verified before the unfinished output can change.', state.kind)
    case 'destination-space-required': return stage('needs-action', 'Free space at the destination',
      'The last verified resume position is retained. Free space, then retry.', state.kind)
    case 'finalizing-tree':
    case 'committing-atomic':
    case 'materialization-sealed':
    case 'packaging':
    case 'artifact-sealed':
    case 'publishing-managed':
    case 'handing-off': return finishingStage(state.kind)
    case 'needs-attention': return stage('needs-action', 'Needs action', attentionDescription(state.reason), state.reason)
    case 'restart-required': return stage('failed', 'Download needs a new attempt', restartDescription(state.reason), state.reason)
    case 'discarded': return stage('cancelled', 'Cancelled', 'Task-owned unfinished data and records were removed.', state.kind)
    default: throw new TypeError('settled task must be resolved before execution stage')
  }
}

function activeReceivingStage(facts: TaskFacts): StageCopy {
  const direct = facts.directZipProgress
  if (direct !== null && direct.phase !== 'receiving' && direct.phase !== 'saving-resume-position') return receivingStage(facts)
  if (facts.progress?.phase === 'finishing') return finishingStage('local-finalization')
  if (facts.blocking !== null) return blockingStage(facts.blocking)
  return receivingStage(facts)
}

function receivingStage(facts: TaskFacts): StageCopy {
  const direct = facts.directZipProgress
  if (direct === null || direct.phase === 'receiving') {
    return stage('downloading', 'Downloading', 'Receiving the selected content. You can keep browsing this share.', 'receiving')
  }
  if (direct.phase === 'saving-resume-position') {
    return stage('downloading', 'Saving resume progress', 'Writing a verified checkpoint while the download remains active.', direct.phase)
  }
  return stage('finishing', direct.phase === 'verifying' ? 'Verifying ZIP' : 'Finishing ZIP',
    'The ZIP is not usable until closing and verification finish.', direct.phase)
}

function pausedStage(facts: TaskFacts): StageCopy {
  const state = facts.lifecycle
  if (state.kind !== 'resumable-package' && facts.blocking !== null &&
      (facts.blocking.kind === 'share-ended' || facts.blocking.kind === 'unavailable')) return blockingStage(facts.blocking)
  if (facts.readiness === 'original-link-required' || facts.readiness === 'different-share') {
    return stage('needs-action', 'Open the original link to continue',
      'The retained record identifies this task, but does not contain the credentials needed to receive more content.', facts.readiness)
  }
  if (state.kind === 'resumable-receive' && state.payloadKind === 'opfs-zip' && state.pauseReason === 'storage-pressure') {
    return stage('needs-action', 'Browser storage is full', 'Free browser storage, then continue from retained progress.', 'storage-pressure')
  }
  return stage('paused', 'Paused', facts.readiness === 'local'
    ? 'Retained content can finish locally without the sender.'
    : 'Verified progress is retained. Continue when you are ready.', state.kind)
}

function finishingStage(kind: string): StageCopy {
  let headline = 'Finishing'
  if (kind === 'packaging') headline = 'Finishing the package'
  if (kind === 'handing-off') headline = 'Starting browser download'
  return stage('finishing', headline, 'Local writing and integrity checks must finish before the result is ready.', kind)
}

function attentionDescription(reason: string): string {
  if (reason === 'publication-unknown') return 'Publication could not be confirmed. Inspect the destination before retrying.'
  if (reason === 'target-ownership-unknown') return 'WindShare cannot verify ownership of the destination.'
  return 'Cleanup could not be verified. Retained ownership must be checked before deleting data.'
}

function restartDescription(reason: string): string {
  if (reason === 'content-session-ended') return 'The content session ended. Open a new link to try again.'
  if (reason === 'target-deleted') return 'The chosen target was deleted. Start a new download to choose another destination.'
  return 'This task cannot safely continue from its retained state. Start a new download.'
}

function stage(taskStage: TaskStage, headline: string, description: string, reason: string): StageCopy {
  return { stage: taskStage, headline, description, reason }
}
