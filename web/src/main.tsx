import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.tsx'
import './index.css'
import { browserBuildSnapshot } from './diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from './diagnostics/browser-composition'
import { installWindShareDiagnostics } from './diagnostics/export/developer-api'
import type { IncidentRecordV1 } from './diagnostics/export/incident-record-v1'
import { createBrowserReceiveOperationMutationPort } from './output/resume/reopen-authority'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from './ui/v2-browser-receive-composition'
import { captureV2Location, V2ReceiverController } from './ui/v2-controller'
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
  secureContext: window.isSecureContext,
  consoleSink: Object.freeze({
    error: (record: IncidentRecordV1) => console.error(record),
  }),
  controllerSnapshot: () => controllerContext.read?.(),
})
const receiverTrace = createV2ReceiverTraceSource(diagnostics.trace)
const outputTrace = createOutputTraceSource(diagnostics.trace)
const protocolTrace = createProtocolTraceSource(diagnostics.trace)
const connectivityTrace = createConnectivityTraceSource(diagnostics.trace)

const receiveMutations = createBrowserReceiveOperationMutationPort()
const receiveComposition = createBrowserReceiveComposition(
  window as BrowserReceiveWindow,
  {
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
  receive: receiveComposition,
  trace: receiverTrace,
  incidents: diagnostics.incidents,
})
controllerContext.read = () => controller.getDiagnosticSnapshot()
controller.initialize(initialCapability)
installWindShareDiagnostics(window, diagnostics.runtime)

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
