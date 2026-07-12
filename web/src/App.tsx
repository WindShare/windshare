import './App.css'
import { ReceiverApp, type ReceiverController } from './ui'

export interface AppProps {
  readonly controller: ReceiverController
}

export default function App({ controller }: AppProps) {
  return <ReceiverApp controller={controller} />
}
