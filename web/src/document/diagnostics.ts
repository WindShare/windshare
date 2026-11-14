import { browserBuildSnapshot } from '../diagnostics/build-identity'
import { createBrowserDiagnosticsComposition } from '../application/diagnostics/composition'
import { createBrowserTraceActivationStore } from '../diagnostics/browser-trace-activation'
import { BrowserDiagnosticsSession, observeDiagnosticsPage } from '../diagnostics/browser/session'
import { IndexedDBDiagnosticsArchive } from '../diagnostics/browser/indexeddb-archive'
import { createDiagnosticsDelivery } from '../diagnostics/browser/delivery'
import { installWindShareDiagnostics } from '../diagnostics/export/developer-api'
import type { IncidentRecordV2 } from '../diagnostics/export/incident-record-v2'
import type { ReceiverDiagnosticSnapshot } from '../diagnostics/export/context'

export function createDocumentDiagnostics(windowPort: Window, pageUrl: string) {
  const context: { read: (() => ReceiverDiagnosticSnapshot) | undefined } = { read: undefined }
  const diagnostics = createBrowserDiagnosticsComposition({
    build: browserBuildSnapshot(),
    activationStore: createBrowserTraceActivationStore(() => windowPort.sessionStorage, pageUrl),
    secureContext: windowPort.isSecureContext,
    consoleSink: Object.freeze({ error: (record: IncidentRecordV2) => console.error(record) }),
    controllerSnapshot: () => context.read?.(),
  })
  const session = new BrowserDiagnosticsSession({
    runtime: diagnostics.runtime, observeCapture: diagnostics.observeEvidence,
    archive: new IndexedDBDiagnosticsArchive(() => windowPort.indexedDB), pageUrl,
  })
  const delivery = createDiagnosticsDelivery({
    navigator: windowPort.navigator, document: windowPort.document, urls: URL,
    defer: (callback, milliseconds) => windowPort.setTimeout(callback, milliseconds),
  })
  const stop = observeDiagnosticsPage(windowPort, session)
  installWindShareDiagnostics(windowPort, session)
  return { diagnostics, session, delivery, context, dispose: () => { stop(); session.dispose(); context.read = undefined } }
}
export type DocumentDiagnostics = ReturnType<typeof createDocumentDiagnostics>
