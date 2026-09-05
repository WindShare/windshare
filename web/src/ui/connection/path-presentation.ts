import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'

export function presentReceiverPathActivity(activity: ReceiverPathActivitySnapshot): string | null {
  switch (activity.content) {
    case 'parallel': return 'Receiving directly and through relay'
    case 'direct': return 'Receiving directly'
    case 'relay': return activity.directConnected ? 'Direct connected · Receiving through relay' : 'Receiving through relay'
    case 'idle': return activity.directConnected ? 'Direct connected' : null
  }
}
