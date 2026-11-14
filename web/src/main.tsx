import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { captureLocation, observeLocation } from './ui/capability/location'
import { initializeBrowserPreferences } from './ui/preferences/browser'
import { BrowserCapabilityIntake } from './ui/capability/intake'
import { createDocumentDiagnostics } from './document/diagnostics'
import { DocumentOwner } from './document/owner'
import { DiagnosticsProvider } from './ui/diagnostics/DiagnosticsProvider'
import App from './App'
import './index.css'

// Capture precedes imports of the receiver and all fallible storage discovery.
const initial = captureLocation(window)
initializeBrowserPreferences()
const diagnostics = createDocumentDiagnostics(window, initial.pageUrl)
const intake = new BrowserCapabilityIntake(() => diagnostics.session.enableFromLink())
const owner = new DocumentOwner({
  pageUrl: initial.pageUrl,
  load: () => import('./document/receiver-runtime').then(module => () =>
    module.createReceiverRuntime(window, diagnostics, initial.pageUrl)),
  acceptDiagnostics: location => intake.accept(location),
  record: event => {
    performance.mark(`windshare:startup:${event.attempt}:${event.transition}`)
    if (event.transition === 'failed') console.error({ event: 'document_runtime', ...event })
  },
})
const stopLocation = observeLocation(window, location => owner.openLocation(location))
owner.openLocation(initial)
window.addEventListener('pagehide', event => {
  if (event.persisted) return
  stopLocation()
  void owner.dispose().catch(() => undefined).finally(() => diagnostics.dispose())
})
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <DiagnosticsProvider session={diagnostics.session} delivery={diagnostics.delivery}>
      <App owner={owner} />
    </DiagnosticsProvider>
  </StrictMode>,
)
