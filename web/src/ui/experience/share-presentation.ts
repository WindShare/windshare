import type { V2ReceiverSnapshot } from '../v2-model'
import type { TaskBlocking } from '../tasks'

export function downloadScopeLabel(share: V2ReceiverSnapshot['share'], draft: V2ReceiverSnapshot['draft']): string {
  if (share !== null && share.kind !== 'browser') return share.kind === 'file' ? 'Download file' : 'Download original'
  if (draft.scope === 'selected') return 'Download selected'
  return draft.scope === 'current-folder' ? 'Download this folder' : 'Download all'
}

export function shareConnectionLabel(connection: V2ReceiverSnapshot['connection'], phase: V2ReceiverSnapshot['phase'], status: string): string {
  switch (connection.kind) {
    case 'connected': return 'Sender connected'
    case 'reconnecting': return 'Reconnecting to the sender…'
    case 'ended': return 'Share ended — a new link is needed'
    case 'unavailable': return 'Connection unavailable'
    case 'idle': return phase === 'joining' ? 'Connecting to the sender…' : status
  }
}

export function taskConnectionBlocking(connection: V2ReceiverSnapshot['connection']): TaskBlocking | null {
  if (connection.kind === 'reconnecting') return { kind: 'reconnecting' }
  if (connection.kind === 'ended') return { kind: 'share-ended' }
  if (connection.kind === 'unavailable') return { kind: 'unavailable' }
  return null
}
