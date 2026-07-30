import type { NetworkMatrixVocabularyConstraint, NetworkTopologyProfile } from './profile.ts'
import { isIP } from 'node:net'
import {
  NETWORK_MATRIX_CANDIDATE_TYPES,
  NETWORK_MATRIX_PROTOCOLS,
  NETWORK_MATRIX_SELECTED_PAIR_OBSERVATIONS,
  type NetworkMatrixCandidatePolicyOutcome,
  type NetworkMatrixCandidateType,
  type NetworkMatrixProtocol,
  type NetworkMatrixRationaleCode,
} from './vocabulary.ts'
import {
  networkMatrixError,
  requireEnum,
  requireExactKeys,
  requireRecord,
} from './contract-support.ts'

export interface PresentNetworkCandidatePath {
  readonly selectedPair: 'present'
  readonly localCandidateType: NetworkMatrixCandidateType
  readonly localAddress: string | null
  readonly localPort: number | null
  readonly remoteCandidateType: NetworkMatrixCandidateType
  readonly remoteAddress: string
  readonly remotePort: number
  readonly protocol: NetworkMatrixProtocol
}

export interface AbsentNetworkCandidatePath {
  readonly selectedPair: 'absent'
  readonly localCandidateType: null
  readonly localAddress: null
  readonly localPort: null
  readonly remoteCandidateType: null
  readonly remoteAddress: null
  readonly remotePort: null
  readonly protocol: null
}

export type NetworkCandidatePath = PresentNetworkCandidatePath | AbsentNetworkCandidatePath

export interface NetworkCandidatePolicyEvaluation {
  readonly candidatePolicyOutcome: Exclude<NetworkMatrixCandidatePolicyOutcome, 'not-evaluated'>
  readonly rationaleCodes: readonly NetworkMatrixRationaleCode[]
}

export function parseNetworkCandidatePath(value: unknown): NetworkCandidatePath {
  const record = requireRecord(value, 'network candidate path')
  requireExactKeys(record, [
    'selectedPair',
    'localCandidateType',
    'localAddress',
    'localPort',
    'remoteCandidateType',
    'remoteAddress',
    'remotePort',
    'protocol',
  ], 'network candidate path')
  const selectedPair = requireEnum(
    record.selectedPair,
    NETWORK_MATRIX_SELECTED_PAIR_OBSERVATIONS,
    'network selected-pair observation',
  )
  if (selectedPair === 'absent') {
    if (
      record.localCandidateType !== null || record.localAddress !== null || record.localPort !== null ||
      record.remoteCandidateType !== null || record.remoteAddress !== null || record.remotePort !== null ||
      record.protocol !== null
    ) networkMatrixError('an absent selected pair cannot carry candidate or protocol observations')
    return Object.freeze({
      selectedPair,
      localCandidateType: null,
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    })
  }
  const localAddress = requireOptionalLocalAddress(record.localAddress)
  const localPort = requireOptionalPort(record.localPort, 'local')
  const remoteAddress = requireIpEndpoint(record.remoteAddress, record.remotePort, 'remote')
  return Object.freeze({
    selectedPair,
    localCandidateType: requireEnum(
      record.localCandidateType,
      NETWORK_MATRIX_CANDIDATE_TYPES,
      'network local candidate type',
    ),
    localAddress,
    localPort,
    remoteCandidateType: requireEnum(
      record.remoteCandidateType,
      NETWORK_MATRIX_CANDIDATE_TYPES,
      'network remote candidate type',
    ),
    remoteAddress: remoteAddress.address,
    remotePort: remoteAddress.port,
    protocol: requireEnum(record.protocol, NETWORK_MATRIX_PROTOCOLS, 'network candidate protocol'),
  })
}

function requireOptionalLocalAddress(address: unknown): string | null {
  if (address === null) return null
  if (typeof address !== 'string') networkMatrixError('network local candidate address is invalid')
  if (isIP(address) !== 0) return address
  if (
    address.length > 253 || !address.endsWith('.local') ||
    !address.split('.').every((label) =>
      label.length >= 1 && label.length <= 63 &&
      /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/u.test(label))
  ) networkMatrixError('network local candidate address is not canonical IP or mDNS')
  return address
}

function requireOptionalPort(port: unknown, label: string): number | null {
  if (port === null) return null
  if (!Number.isSafeInteger(port) || (port as number) < 1 || (port as number) > 65_535) {
    networkMatrixError(`network ${label} candidate port is invalid`)
  }
  return port as number
}

function requireIpEndpoint(
  address: unknown,
  port: unknown,
  label: string,
): { readonly address: string; readonly port: number } {
  if (
    typeof address !== 'string' || isIP(address) === 0 ||
    !Number.isSafeInteger(port) || (port as number) < 1 || (port as number) > 65_535
  ) networkMatrixError(`network ${label} candidate endpoint is invalid`)
  return Object.freeze({ address, port: port as number })
}

export function evaluateNetworkCandidatePolicy(
  path: NetworkCandidatePath,
  profile: NetworkTopologyProfile,
): NetworkCandidatePolicyEvaluation {
  const parsed = parseNetworkCandidatePath(path)
  const policy = profile.candidatePolicy
  if (policy.selectedPair === 'required' && parsed.selectedPair === 'absent') {
    return mismatch(['selected-pair-required'])
  }
  if (policy.selectedPair === 'prohibited' && parsed.selectedPair === 'present') {
    return mismatch(['selected-pair-prohibited'])
  }
  if (parsed.selectedPair === 'absent') return matched()
  const rationaleCodes = [
    ...constraintRationales(
      parsed.localCandidateType,
      policy.localCandidateTypes,
      'local-candidate-type',
    ),
    ...constraintRationales(
      parsed.remoteCandidateType,
      policy.remoteCandidateTypes,
      'remote-candidate-type',
    ),
    ...constraintRationales(parsed.protocol, policy.protocols, 'protocol'),
  ]
  return rationaleCodes.length === 0 ? matched() : mismatch(rationaleCodes)
}

function constraintRationales<T extends string>(
  observed: T,
  constraint: NetworkMatrixVocabularyConstraint<T>,
  prefix: 'local-candidate-type' | 'remote-candidate-type' | 'protocol',
): NetworkMatrixRationaleCode[] {
  const result: NetworkMatrixRationaleCode[] = []
  if (constraint.forbidden.includes(observed)) {
    result.push(`${prefix}-forbidden`)
  } else if (!constraint.allowed.includes(observed)) {
    result.push(`${prefix}-not-allowed`)
  }
  if (constraint.required.length === 1 && constraint.required[0] !== observed) {
    result.push(`${prefix}-required-missing`)
  }
  return result
}

function matched(): NetworkCandidatePolicyEvaluation {
  return Object.freeze({ candidatePolicyOutcome: 'matched', rationaleCodes: Object.freeze([]) })
}

function mismatch(
  rationaleCodes: readonly NetworkMatrixRationaleCode[],
): NetworkCandidatePolicyEvaluation {
  return Object.freeze({
    candidatePolicyOutcome: 'mismatched',
    rationaleCodes: Object.freeze([...rationaleCodes]),
  })
}
