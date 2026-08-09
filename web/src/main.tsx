import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './index.css'
import { createBrowserReceiveOperationMutationPort } from './output/resume/reopen-authority'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from './ui/v2-browser-receive-composition'
import { captureV2Location, V2ReceiverController } from './ui/v2-controller'
import { V2BrowserReceiverGateway } from './ui/v2-gateway'
import { createPrivacySafeV2ReceiverTraceSink } from './ui/v2-production-trace'

// Initialization runs outside React so StrictMode cannot duplicate capability
// parsing, fragment erasure, or the pre-gesture relay join. Capability removal
// also precedes fallible browser-capability discovery in gateway construction.
const initialCapability = captureV2Location(window)
const receiveTrace = createPrivacySafeV2ReceiverTraceSink(console)
const receiveMutations = createBrowserReceiveOperationMutationPort({ trace: receiveTrace })
const receiveComposition = createBrowserReceiveComposition(
  window as BrowserReceiveWindow,
  { resumeMutations: receiveMutations, onTrace: receiveTrace },
)
const controller = new V2ReceiverController(
  new V2BrowserReceiverGateway(),
  {
    receive: receiveComposition,
    onOutputTrace: receiveTrace,
  },
)
controller.initialize(initialCapability)

window.addEventListener('pagehide', (event) => {
  // A persisted page resumes the same controller from the back-forward cache;
  // disposing it would leave key entry and transfer actions permanently inert.
  if (event.persisted) {
    return
  }
  controller.dispose().catch(() => undefined)
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App controller={controller} />
  </StrictMode>,
)
