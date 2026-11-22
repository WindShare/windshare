import type {
  DiagnosticFailureRecord,
  DiagnosticsRuntime,
  DiagnosticsStatus,
  TraceActivationSnapshot,
} from './runtime'

export interface WindShareDiagnostics {
  activation(): TraceActivationSnapshot
  enable(): DiagnosticsStatus
  disable(): DiagnosticsStatus
  status(): DiagnosticsStatus
  lastFailure(): DiagnosticFailureRecord | null
  export(): string
  clear(): void
}

declare global {
  interface Window {
    readonly windshareDiagnostics: WindShareDiagnostics
  }
}

export function createWindShareDiagnostics(
  runtime: Pick<
    DiagnosticsRuntime,
    'activation' | 'enable' | 'disable' | 'status' | 'lastFailure' | 'export' | 'clear'
  >,
): WindShareDiagnostics {
  return Object.freeze({
    activation: () => runtime.activation(),
    enable: () => runtime.enable(),
    disable: () => runtime.disable(),
    status: () => runtime.status(),
    lastFailure: () => runtime.lastFailure(),
    export: () => runtime.export(),
    clear: () => runtime.clear(),
  })
}

export function installWindShareDiagnostics(
  target: object,
  runtime: Pick<
    DiagnosticsRuntime,
    'activation' | 'enable' | 'disable' | 'status' | 'lastFailure' | 'export' | 'clear'
  >,
): WindShareDiagnostics {
  if (Reflect.has(target, 'windshareDiagnostics')) {
    throw new TypeError('windshareDiagnostics is already installed')
  }
  const api = createWindShareDiagnostics(runtime)
  Object.defineProperty(target, 'windshareDiagnostics', {
    value: api,
    enumerable: false,
    configurable: false,
    writable: false,
  })
  return api
}
