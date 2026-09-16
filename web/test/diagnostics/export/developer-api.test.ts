import { describe, expect, it, vi } from 'vitest'

import {
  createWindShareDiagnostics,
  installWindShareDiagnostics,
} from '../../../src/diagnostics/export/developer-api'
import { projectDiagnosticsStatusV2 } from '../../../src/diagnostics/export/diagnostic-bundle-v2'
import type { DiagnosticsRuntimePort } from '../../../src/diagnostics/runtime'
import {
  diagnosticsHealthV1,
  incidentRecord,
  traceStatus,
} from './test-support'

describe('windshareDiagnostics developer API', () => {
  it('is a frozen facade over the injected runtime operations', () => {
    const status = projectDiagnosticsStatusV2(traceStatus(), diagnosticsHealthV1())
    const failure = incidentRecord('1')
    const runtime: DiagnosticsRuntimePort = {
      runtimeRunId: 'AQAAAAAAAAAAAAAAAAAAAA',
      activation: vi.fn(() => ({ kind: 'off' } as const)),
      enable: vi.fn(() => status),
      disable: vi.fn(() => status),
      status: vi.fn(() => status),
      inspectLastFailure: vi.fn(() => failure),
      export: vi.fn(() => '{"line_type":"bundle_header"}\n'),
      clear: vi.fn(),
    }
    const api = createWindShareDiagnostics(runtime)

    expect(api.activation()).toEqual({ kind: 'off' })
    expect(api.enable()).toBe(status)
    expect(api.disable()).toBe(status)
    expect(api.status()).toBe(status)
    expect(api.inspectLastFailure()).toBe(failure)
    expect(api.export()).toBe('{"line_type":"bundle_header"}\n')
    expect(api.clear()).toBeUndefined()
    expect(Object.keys(api)).toEqual([
      'activation',
      'enable',
      'disable',
      'status',
      'inspectLastFailure',
      'export',
      'clear',
    ])
    expect(Object.isFrozen(api)).toBe(true)
  })

  it('installs one non-writable, non-configurable readonly global', () => {
    const runtime = runtimePort()
    const target = {}
    const installed = installWindShareDiagnostics(target, runtime)
    const descriptor = Object.getOwnPropertyDescriptor(target, 'windshareDiagnostics')

    expect(descriptor).toMatchObject({
      value: installed,
      enumerable: false,
      configurable: false,
      writable: false,
    })
    expect(Reflect.set(target, 'windshareDiagnostics', {})).toBe(false)
    expect(Reflect.deleteProperty(target, 'windshareDiagnostics')).toBe(false)
    expect(() => installWindShareDiagnostics(target, runtime)).toThrow(/already installed/)
  })

  it('does not shadow a diagnostics property inherited from the host', () => {
    const target = Object.create({ windshareDiagnostics: Object.freeze({}) }) as object
    expect(() => installWindShareDiagnostics(target, runtimePort())).toThrow(/already installed/)
    expect(Object.hasOwn(target, 'windshareDiagnostics')).toBe(false)
  })
})

function runtimePort(): DiagnosticsRuntimePort {
  const status = projectDiagnosticsStatusV2(traceStatus(), diagnosticsHealthV1())
  return {
    runtimeRunId: 'AQAAAAAAAAAAAAAAAAAAAA',
    activation: () => ({ kind: 'off' }),
    enable: () => status,
    disable: () => status,
    status: () => status,
    inspectLastFailure: () => null,
    export: () => '',
    clear: () => undefined,
  }
}
