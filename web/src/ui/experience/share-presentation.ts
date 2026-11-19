import type { InitialJoinState } from '../../receiver/initial-join'
import type { ReceiverSnapshot } from '../model'
import type { TaskBlocking } from '../tasks'

export function downloadScopeLabel(share: ReceiverSnapshot['share'], draft: ReceiverSnapshot['draft']): string {
  if (share !== null && share.kind !== 'browser') return share.kind === 'file' ? 'Download file' : 'Download original'
  if (draft.scope === 'selected') return 'Download selected'
  return draft.scope === 'current-folder' ? 'Download this folder' : 'Download all'
}

export function shareConnectionLabel(connection: ReceiverSnapshot['connection'], phase: ReceiverSnapshot['phase'], status: string): string {
  switch (connection.kind) {
    case 'connected': return 'Sender connected'
    case 'reconnecting': return connection.activity.kind === 'waiting' ? 'Waiting to reconnect automatically' : 'Reconnecting to the sender…'
    case 'ended': return 'Share ended — a new link is needed'
    case 'unavailable': return 'Connection unavailable'
    case 'idle': {
      const join = connection.join
      if (join?.kind === 'paused' || join?.kind === 'waiting') return initialJoinHoldLabel(join)
      return phase === 'joining' ? 'Connecting to the sender…' : status
    }
  }
}

function initialJoinHoldLabel(join: Exclude<InitialJoinState, { kind: 'connecting' }>): string {
  if (join.reason === 'authorization') return 'Official relay access restricted'
  if (join.kind === 'paused') {
    return join.reason === 'bandwidth' ? 'Relay bandwidth unavailable' : 'Sender temporarily unreachable'
  }
  return join.reason === 'bandwidth' ? 'Waiting for relay bandwidth' : 'Waiting for the sender; reconnecting automatically'
}

export function initialJoinStatus(join: InitialJoinState): string {
  if (join.kind !== 'connecting' && join.reason === 'authorization') {
    return 'This share is temporarily unavailable through official relays.'
  }
  switch (join.kind) {
    case 'paused': return join.reason === 'bandwidth'
      ? 'The connection timed out while waiting for relay bandwidth.'
      : 'The sender is temporarily unreachable. The link may still be valid.'
    case 'waiting': return join.reason === 'bandwidth'
      ? 'Relay bandwidth is busy. Waiting automatically.'
      : 'Waiting for the sender. Reconnection is automatic.'
    case 'connecting': return 'Connecting to the sender…'
  }
}

const RECONNECTING: TaskBlocking = Object.freeze({ kind: 'reconnecting',
  description: 'Reconnection is automatic. Keep this page open to preserve the current download and its received progress.' })
const SHARE_ENDED: TaskBlocking = Object.freeze({ kind: 'share-ended' })
const UNAVAILABLE: TaskBlocking = Object.freeze({ kind: 'unavailable' })

export function taskConnectionBlocking(connection: ReceiverSnapshot['connection']): TaskBlocking | null {
  if (connection.kind === 'reconnecting') return RECONNECTING
  if (connection.kind === 'ended') return SHARE_ENDED
  if (connection.kind === 'unavailable') return UNAVAILABLE
  return null
}
