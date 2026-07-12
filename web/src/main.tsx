import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './index.css'
import {
  BrowserReceiverGateway,
  ReceiverController,
  browserNavigation,
  consumeLocationCapability,
} from './ui'

// Initialization runs outside React so StrictMode cannot duplicate capability
// parsing, fragment erasure, or the pre-gesture relay join. Capability removal
// also precedes fallible browser-capability discovery in gateway construction.
const initialCapability = consumeLocationCapability(browserNavigation(window))
const controller = new ReceiverController(new BrowserReceiverGateway())
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
