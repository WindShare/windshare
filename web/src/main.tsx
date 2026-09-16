import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './index.css'
import { browserBuildSnapshot } from './diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from './diagnostics/browser-composition'
import { createBrowserTraceActivationStore } from './diagnostics/browser-trace-activation'
import { BrowserDiagnosticsSession, observeDiagnosticsPage } from './diagnostics/browser/session'
import { IndexedDBDiagnosticsArchive } from './diagnostics/browser/indexeddb-archive'
import { createDiagnosticsDelivery } from './diagnostics/browser/delivery'
import { DiagnosticsProvider } from './ui/diagnostics/DiagnosticsProvider'
import { installWindShareDiagnostics } from './diagnostics/export/developer-api'
import type { IncidentRecordV2 } from './diagnostics/export/incident-record-v2'
import { createBrowserReceiveOperationMutationPort } from './output/resume/reopen-authority'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from './ui/v2-browser-receive-composition'
import { createBrowserDirectZipComposition } from './ui/browser-receive/direct-zip/production'
import { V2ReceiverController } from './ui/v2-controller'
import { captureV2Location, observeV2Location } from './ui/capability/location'
import { BrowserCapabilityIntake } from './ui/capability/intake'
import { V2BrowserReceiverGateway } from './ui/v2-gateway'
import {
  createConnectivityTraceSource,
  createOutputTraceSource,
  createProtocolTraceSource,
  createV2ReceiverTraceSource,
} from './ui/v2-production-trace'

// Initialization runs outside React so StrictMode cannot duplicate capability
// parsing, fragment erasure, or the pre-gesture relay join. Fragment erasure
// happens before any fallible browser-capability discovery or receiver assembly.
const initialCapability = captureV2Location(window)

// Late binding keeps startup incidents reportable before the controller exists,
// without exposing the composition root to a temporal-dead-zone lookup.
const controllerContext: {
  read:
    | (() => ReturnType<V2ReceiverController['getDiagnosticSnapshot']>)
    | undefined
} = { read: undefined }

const diagnostics = createBrowserDiagnosticsComposition({
  build: browserBuildSnapshot(),
  activationStore: createBrowserTraceActivationStore(() => window.sessionStorage, initialCapability.pageUrl),
  secureContext: window.isSecureContext,
  consoleSink: Object.freeze({
    error: (record: IncidentRecordV2) => console.error(record),
  }),
  controllerSnapshot: () => controllerContext.read?.(),
})
const diagnosticSession = new BrowserDiagnosticsSession({
  runtime: diagnostics.runtime,
  observeCapture: diagnostics.trace.subscribe,
  archive: new IndexedDBDiagnosticsArchive(() => window.indexedDB),
  pageUrl: initialCapability.pageUrl,
})
const diagnosticDelivery = createDiagnosticsDelivery({
  navigator: window.navigator,
  document: window.document,
  urls: URL,
  defer: (callback, milliseconds) => window.setTimeout(callback, milliseconds),
})
const stopDiagnosticsObservation = observeDiagnosticsPage(window, diagnosticSession)
const receiverTrace = createV2ReceiverTraceSource(diagnostics.trace)
const outputTrace = createOutputTraceSource(diagnostics.trace)
const protocolTrace = createProtocolTraceSource(diagnostics.trace)
const connectivityTrace = createConnectivityTraceSource(diagnostics.trace)

const receiveMutations = createBrowserReceiveOperationMutationPort({ outputTrace })
const receiveComposition = createBrowserReceiveComposition(
  window as BrowserReceiveWindow,
  {
    directZip: createBrowserDirectZipComposition(window as BrowserReceiveWindow, { outputTrace }),
    resumeMutations: receiveMutations,
    outputTrace,
    localOutputFailures: diagnostics.localOutputFailures,
  },
)
const gateway = new V2BrowserReceiverGateway({
  protocolTrace,
  connectivityTrace,
})
const controller = new V2ReceiverController(gateway, {
  capabilityIntake: new BrowserCapabilityIntake(() => diagnosticSession.enableFromLink()),
  receive: receiveComposition,
  trace: receiverTrace,
  incidents: diagnostics.incidents,
})
controllerContext.read = () => controller.getDiagnosticSnapshot()
controller.initialize(initialCapability)
const stopLocationObservation = observeV2Location(window, captured => controller.openLocation(captured))
installWindShareDiagnostics(window, diagnosticSession)

window.addEventListener('pagehide', (event) => {
  // A persisted page resumes the same controller from the back-forward cache;
  // disposing it would leave key entry and transfer actions permanently inert.
  if (event.persisted) {
    return
  }
  stopLocationObservation()
  stopDiagnosticsObservation()
  diagnosticSession.dispose()
  controller.dispose().catch(() => undefined)
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <DiagnosticsProvider session={diagnosticSession} delivery={diagnosticDelivery}>
      <App controller={controller} />
    </DiagnosticsProvider>
  </StrictMode>,
)
