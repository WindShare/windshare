import type { FailureFact } from './fact'

declare const failureFingerprintBrand: unique symbol

export type FailureFingerprint = string & {
  readonly [failureFingerprintBrand]: true
}

export function fingerprintFailureFact(fact: FailureFact): FailureFingerprint {
  const common = [
    fact.kind,
    fact.stage,
    fact.recoveryDisposition,
  ]
  switch (fact.kind) {
    case 'fault': {
      const value = fact.payload.fault
      return encode([...common, value.domain, value.scope, value.code])
    }
    case 'protocol_failure': {
      const value = fact.payload.protocolFailure
      const settlement = value.settlement.kind === 'received_authenticated'
        ? [value.settlement.kind]
        : [
            value.settlement.kind,
            encodeBoolean(value.settlement.admitted),
            encodeBoolean(value.settlement.settled),
            value.settlement.outcome,
          ]
      return encode([
        ...common,
        value.requestKind,
        value.wireScope,
        String(value.wireCode),
        encodeBoolean(value.retryable),
        value.retryAfterMilliseconds === undefined
          ? '-'
          : String(value.retryAfterMilliseconds),
        ...settlement,
      ])
    }
    case 'peer_failure': {
      const value = fact.payload.peerFailure
      return encode([
        ...common,
        value.scope,
        value.code,
        encodeBoolean(value.retryable),
      ])
    }
    case 'native_output_failure': {
      const value = fact.payload.nativeOutputFailure
      return encode([
        ...common,
        value.nativeClass,
        value.code ?? '-',
      ])
    }
    case 'lifecycle_failure': {
      const value = fact.payload.lifecycleFailure
      return encode([
        ...common,
        value.kind,
        value.reason ?? '-',
      ])
    }
    case 'unclassified':
      return encode(common)
  }
}

function encode(parts: readonly string[]): FailureFingerprint {
  return parts.join(':') as FailureFingerprint
}

function encodeBoolean(value: boolean): string {
  return value ? '1' : '0'
}
