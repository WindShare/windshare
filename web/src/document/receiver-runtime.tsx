import { browserPreferences } from '../ui/preferences/browser'
import '../App.css'
import type { DocumentRuntime } from './owner'
import type { DocumentDiagnostics } from './diagnostics'
import type { BrowserOutputWindow } from '../output/backends/contract'
import { BrowserReceiverGateway } from '../application/browser-share/gateway'
import { BuiltinNetwork } from '../application/network/builtin'
import { networkObserver, authorityObserver } from '../application/network/diagnostics'
import {
  createCatalogStorageObserver,
  createConnectivityTraceSource,
  createOutputTraceSource,
  createProtocolTraceSource,
  createReceiverTraceSource,
} from '../application/diagnostics/receive-trace'
import { ReceiverController } from '../ui/controller'
import { ReceiverApp } from '../ui/ReceiverApp'
import { ReceiverDownloads } from '../ui/receiver-presentation/ReceiverTasks'

export function createReceiverRuntime(
  windowPort: Window,
  diagnosticOwner: DocumentDiagnostics,
  pageUrl: string,
): DocumentRuntime {
  const { diagnostics } = diagnosticOwner
  const outputTrace = createOutputTraceSource(diagnostics.trace)
  const network = new BuiltinNetwork(networkObserver(diagnostics.trace), authorityObserver(diagnostics.trace))
  const controller = new ReceiverController(new BrowserReceiverGateway({
    network,
    protocolTrace: createProtocolTraceSource(diagnostics.trace, diagnostics.failures),
    connectivityTrace: createConnectivityTraceSource(diagnostics.trace),
    onCatalogStorageFallback: createCatalogStorageObserver(diagnostics.trace),
  }), {
    windowPort: windowPort as unknown as BrowserOutputWindow,
    outputTrace,
    preferences: browserPreferences,
    trace: createReceiverTraceSource(diagnostics.trace),
    incidents: diagnostics.incidents,
  })
  diagnosticOwner.context.read = controller.getDiagnosticSnapshot
  controller.initialize({ capabilityInput: null, pageUrl })
  return {
    surface: controller.presentation.surface,
    openLocation: captured => { network.clearReport(); controller.openLocation(captured) },
    ensureDownloads: () => controller.ensureDownloads(),
    View: function ReceiverSurface() { return <ReceiverApp controller={controller} report={network} /> },
    Downloads: function RuntimeDownloads(props) {
      return <ReceiverDownloads controller={controller} {...props} />
    },
    dispose: async () => {
      diagnosticOwner.context.read = undefined
      network.close()
      await controller.dispose()
    },
  }
}
