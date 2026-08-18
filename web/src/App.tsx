import { useSyncExternalStore } from 'react'
import './App.css'
import { V2ReceiverApp } from './ui/V2ReceiverApp'
import { PortalApp } from './ui/portal/PortalApp'
import type { V2ReceiverController } from './ui/v2-controller'

export interface AppProps {
  readonly controller: V2ReceiverController
}

export default function App({ controller }: AppProps) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  )

  const isReceivingShare =
    snapshot.breadcrumbs.length > 0 ||
    snapshot.phase === 'joining' ||
    snapshot.phase === 'browsing' ||
    (snapshot.phase === 'failed' && snapshot.error !== null)

  if (isReceivingShare) {
    return <V2ReceiverApp controller={controller} />
  }

  return <PortalApp controller={controller} />
}
