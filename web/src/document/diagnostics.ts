import { createBrowserTraceActivationStore } from '../diagnostics/activation'
import { browserBuildSnapshot } from '../diagnostics/build-identity'
import { createDiagnosticsDelivery } from '../diagnostics/delivery'
import { installWindShareDiagnostics } from '../diagnostics/developer-api'
import { createDiagnosticsRuntime } from '../diagnostics/runtime'

export function createDocumentDiagnostics(windowPort: Window, pageUrl: string) {
  const runtime = createDiagnosticsRuntime({
    build: browserBuildSnapshot(),
    secureContext: windowPort.isSecureContext,
    activationStore: createBrowserTraceActivationStore(() => windowPort.sessionStorage, pageUrl),
    consoleSink: Object.freeze({ error: (record: unknown) => console.error(record) }),
  })
  const delivery = createDiagnosticsDelivery({
    navigator: windowPort.navigator,
    document: windowPort.document,
    urls: URL,
    defer: (callback, milliseconds) => windowPort.setTimeout(callback, milliseconds),
  })
  installWindShareDiagnostics(windowPort, runtime)
  return {
    runtime,
    delivery,
    dispose: () => runtime.dispose(),
  }
}

export type DocumentDiagnostics = ReturnType<typeof createDocumentDiagnostics>
