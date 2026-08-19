import type { IncidentRecordV1 } from './incident-record-v1'
import type { DiagnosticsStatusV1 } from './diagnostic-bundle-v1'
import type { DiagnosticsRuntimePort } from '../runtime'

export interface WindShareDiagnostics {
  enable(): DiagnosticsStatusV1
  disable(): DiagnosticsStatusV1
  status(): DiagnosticsStatusV1
  inspectLastFailure(): IncidentRecordV1 | null
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
