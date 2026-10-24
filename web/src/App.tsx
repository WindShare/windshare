import { useSyncExternalStore } from 'react'
import './App.css'
import { ReceiverApp } from './ui/ReceiverApp'
import { PortalApp } from './ui/portal/PortalApp'
import type { ReceiverController } from './ui/controller'

export interface AppProps {
  readonly controller: ReceiverController
}

export default function App({ controller }: AppProps) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  )

  const isReceivingShare =
    snapshot.share !== null ||
    snapshot.breadcrumbs.length > 0 ||
    snapshot.phase === 'joining' ||
    snapshot.phase === 'browsing' ||
    (snapshot.phase === 'failed' && snapshot.error !== null)

  if (isReceivingShare) {
    return <ReceiverApp controller={controller} />
  }

  return <PortalApp controller={controller} />
}
