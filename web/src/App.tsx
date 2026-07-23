import './App.css'
import { V2ReceiverApp } from './ui/V2ReceiverApp'
import type { V2ReceiverController } from './ui/v2-controller'

export interface AppProps {
  readonly controller: V2ReceiverController
}

export default function App({ controller }: AppProps) {
  return <V2ReceiverApp controller={controller} />
}
