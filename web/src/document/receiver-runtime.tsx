import '../App.css'
import type { DocumentRuntime } from './owner'
import type { DocumentDiagnostics } from './diagnostics'
import { createBrowserReceiveComposition, type BrowserReceiveWindow } from '../application/browser-receive/composition'
import { createLazyBrowserDirectZipComposition } from '../application/browser-receive/direct-zip/loading'
import { BrowserReceiverGateway } from '../application/browser-share/gateway'
import { BuiltinNetwork } from '../application/network/builtin'
import { networkObserver } from '../application/network/diagnostics'
import { createConnectivityTraceSource, createOutputTraceSource, createProtocolTraceSource, createReceiverTraceSource } from '../application/diagnostics/receive-trace'
import { ReceiverController } from '../ui/controller'
import { ReceiverApp } from '../ui/ReceiverApp'
import { ReceiverDownloads } from '../ui/receiver-presentation/ReceiverTasks'

export function createReceiverRuntime(windowPort: Window, diagnosticOwner: DocumentDiagnostics, pageUrl: string): DocumentRuntime {
  const { diagnostics } = diagnosticOwner
  const outputTrace = createOutputTraceSource(diagnostics.trace)
  const receive = createBrowserReceiveComposition(windowPort as BrowserReceiveWindow, {
    directZip: createLazyBrowserDirectZipComposition(windowPort as BrowserReceiveWindow, { outputTrace }),
    outputTrace, localOutputFailures: diagnostics.localOutputFailures,
  })
  const network = new BuiltinNetwork(networkObserver(diagnostics.trace))
  const controller = new ReceiverController(new BrowserReceiverGateway({
    network, protocolTrace: createProtocolTraceSource(diagnostics.trace, diagnostics.failures),
    connectivityTrace: createConnectivityTraceSource(diagnostics.trace),
  }), {
    receive, trace: createReceiverTraceSource(diagnostics.trace), incidents: diagnostics.incidents,
  })
  diagnosticOwner.context.read = controller.getDiagnosticSnapshot
  controller.initialize({ capabilityInput: null, pageUrl })
  return {
    surface: controller.presentation.surface,
    openLocation: captured => controller.openLocation(captured),
    ensureDownloads: () => controller.ensureDownloads(),
    View: function ReceiverSurface() { return <ReceiverApp controller={controller} /> },
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
