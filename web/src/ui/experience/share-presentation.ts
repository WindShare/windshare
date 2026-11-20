import { receiverCopy } from '../locales/receiver'
import type { InitialJoinState } from '../../receiver/initial-join'
import type { ReceiverSnapshot } from '../model'
import type { TaskBlocking } from '../tasks'

export function downloadScopeLabel(share: ReceiverSnapshot['share'], draft: ReceiverSnapshot['draft'], t = receiverCopy.en.saving): string {
  if (share !== null && share.kind !== 'browser') return share.kind === 'file' ? t.downloadFile : t.downloadOriginal
  if (draft.scope === 'selected') return t.downloadSelected
  return draft.scope === 'current-folder' ? t.downloadFolder : t.downloadAll
}

export function shareConnectionLabel(connection: ReceiverSnapshot['connection'], phase: ReceiverSnapshot['phase'], status: ReceiverSnapshot['status'], t = receiverCopy.en.connection): string {
  switch (connection.kind) {
    case 'connected': return t.senderConnected
    case 'reconnecting': return connection.activity.kind === 'waiting' ? t.waitingReconnect : t.reconnecting
    case 'ended': return t.shareEnded
    case 'unavailable': return t.unavailable
    case 'idle': {
      const join = connection.join
      if (join?.kind === 'paused' || join?.kind === 'waiting') return initialJoinHoldLabel(join, t)
      return phase === 'joining' ? t.connectingSender : receiverStatus(status, t)
    }
  }
}

function initialJoinHoldLabel(join: Exclude<InitialJoinState, { kind: 'connecting' }>, t: typeof receiverCopy.en.connection): string {
  if (join.reason === 'authorization') return t.relayRestricted
  if (join.kind === 'paused') {
    return join.reason === 'bandwidth' ? t.relayUnavailable : t.senderUnreachable
  }
  return join.reason === 'bandwidth' ? t.waitingBandwidth : t.waitingSender
}

export function initialJoinStatus(join: InitialJoinState, t = receiverCopy.en.connection): string {
  if (join.kind !== 'connecting' && join.reason === 'authorization') {
    return t.restrictedStatus
  }
  switch (join.kind) {
    case 'paused': return join.reason === 'bandwidth'
      ? t.bandwidthTimeout
      : t.unreachableStatus
    case 'waiting': return join.reason === 'bandwidth'
      ? t.bandwidthStatus
      : t.senderStatus
    case 'connecting': return t.connectingSender
  }
}

const RECONNECTING: TaskBlocking = Object.freeze({ kind: 'reconnecting' })
const SHARE_ENDED: TaskBlocking = Object.freeze({ kind: 'share-ended' })
const UNAVAILABLE: TaskBlocking = Object.freeze({ kind: 'unavailable' })

export function taskConnectionBlocking(connection: ReceiverSnapshot['connection']): TaskBlocking | null {
  if (connection.kind === 'reconnecting') return RECONNECTING
  if (connection.kind === 'ended') return SHARE_ENDED
  if (connection.kind === 'unavailable') return UNAVAILABLE
  return null
}

export function receiverStatus(status: ReceiverSnapshot['status'], t = receiverCopy.en.connection): string {
  switch (status) {
    case null: return ''
    case 'enter-key': return t.enterKey
    case 'connecting': return t.connectingShare
    case 'cancelled': return t.joinCancelled
    case 'stopped': return t.receiverStopped
    case 'replacement-selected': return t.replacementSelected
    default: return t.findingReplacement(status.path)
  }
}
