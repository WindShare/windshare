import type { IncidentRecordV2 } from './incident-record-v2'
import type { DiagnosticsStatusV2 } from './diagnostic-bundle-v2'
import type { DiagnosticsRuntimePort } from '../runtime'

export interface WindShareDiagnostics {
  enable(): DiagnosticsStatusV2
  disable(): DiagnosticsStatusV2
  status(): DiagnosticsStatusV2
  inspectLastFailure(): IncidentRecordV2 | null
  export(): string
  clear(): void
}

export type DiagnosticsGlobalTarget = object

declare global {
  interface Window {
    readonly windshareDiagnostics: WindShareDiagnostics
  }
}

export function createWindShareDiagnostics(
  runtime: DiagnosticsRuntimePort,
): WindShareDiagnostics {
  return Object.freeze({
    enable: () => runtime.enable(),
    disable: () => runtime.disable(),
    status: () => runtime.status(),
    inspectLastFailure: () => runtime.inspectLastFailure(),
    export: () => runtime.export(),
    clear: () => runtime.clear(),
  })
}

export function installWindShareDiagnostics(
  target: DiagnosticsGlobalTarget,
  runtime: DiagnosticsRuntimePort,
): WindShareDiagnostics {
  if (Reflect.has(target, 'windshareDiagnostics')) {
    throw new TypeError('windshareDiagnostics is already installed')
  }
  const diagnostics = createWindShareDiagnostics(runtime)
  Object.defineProperty(target, 'windshareDiagnostics', {
    value: diagnostics,
    enumerable: false,
    configurable: false,
    writable: false,
  })
  return diagnostics
}
