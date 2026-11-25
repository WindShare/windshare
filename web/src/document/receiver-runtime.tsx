import { browserPreferences } from '../ui/preferences/browser'
import '../App.css'
import type { DocumentRuntime } from './owner'
import type { DocumentDiagnostics } from './diagnostics'
import type { BrowserOutputWindow } from '../output/backends/contract'
import { BrowserReceiverGateway } from '../application/browser-share/gateway'
import { BuiltinNetwork } from '../application/network/builtin'
import { networkObserver } from '../application/network/diagnostics'
import {
  createConnectivityTraceSource,
  createFailureReporter,
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
  const { runtime } = diagnosticOwner
  const outputTrace = createOutputTraceSource(runtime.trace)
  const network = new BuiltinNetwork(networkObserver(runtime.trace))
  const controller = new ReceiverController(new BrowserReceiverGateway({
    network,
    protocolTrace: createProtocolTraceSource(runtime),
    connectivityTrace: createConnectivityTraceSource(runtime.trace),
  }), {
    windowPort: windowPort as unknown as BrowserOutputWindow,
    outputTrace,
    preferences: browserPreferences,
    trace: createReceiverTraceSource(runtime.trace),
    recordFailure: createFailureReporter(runtime),
  })
  controller.initialize({ capabilityInput: null, pageUrl })
  return {
    surface: controller.presentation.surface,
    openLocation: captured => { network.clearReport(); controller.openLocation(captured) },
    refreshDownloads: () => controller.refreshDownloads(),
    View: function ReceiverSurface() { return <ReceiverApp controller={controller} report={network} /> },
    Downloads: function RuntimeDownloads(props) {
      return <ReceiverDownloads controller={controller} {...props} />
    },
    dispose: async () => {
      network.close()
      await controller.dispose()
    },
  }
}
