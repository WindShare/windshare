import { createHash, randomBytes } from 'node:crypto'

import {
  NETWORK_MATRIX_BROWSERS,
  NETWORK_MATRIX_PROFILE_IDS,
  NETWORK_MATRIX_SAMPLE_ORDINALS,
  type NetworkMatrixBrowser,
  type NetworkMatrixProfileId,
  type NetworkMatrixSampleOrdinal,
} from './vocabulary.ts'

export const NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA =
  'windshare.browser-network-matrix.sample-authority/v1' as const
export const NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA =
  'windshare.browser-network-matrix.control-authority/v1' as const
export const NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA =
  'windshare.browser-network-matrix.attempt-request-authority/v1' as const
export const NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA =
  'windshare.browser-network-matrix.attempt-authority/v1' as const

const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const MAXIMUM_CANONICAL_ID_BYTES = 96

export interface NetworkMatrixSampleAuthority {
  readonly schemaVersion: typeof NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA
  readonly runId: string
  readonly profileId: NetworkMatrixProfileId
  readonly browser: NetworkMatrixBrowser
  readonly sampleOrdinal: NetworkMatrixSampleOrdinal
  readonly processInstanceId: string
  readonly operationId: string
}

export interface NetworkMatrixControlAuthority {
  readonly schemaVersion: typeof NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA
  readonly sampleAuthority: NetworkMatrixSampleAuthority
  readonly controlLeaseId: string
}

export interface NetworkMatrixFixtureAuthorityBinding {
  readonly attestationSha256: string
  readonly authorityInstanceId: string
  readonly remoteServiceInstanceId: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
}

export interface NetworkMatrixAttemptRequestAuthority {
  readonly schemaVersion: typeof NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly requestId: string
  readonly fixtureBinding: NetworkMatrixFixtureAuthorityBinding
}

export interface NetworkMatrixAttemptAuthority {
  readonly schemaVersion: typeof NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA
  readonly requestAuthority: NetworkMatrixAttemptRequestAuthority
  readonly attemptId: string
  readonly challenge: string
}

export function newNetworkMatrixProcessInstanceId(): string {
  return `browser-${randomBytes(12).toString('hex')}`
}

export function parseNetworkMatrixSampleAuthority(value: unknown): NetworkMatrixSampleAuthority {
  const record = exactRecord(value, [
    'schemaVersion', 'runId', 'profileId', 'browser', 'sampleOrdinal',
    'processInstanceId', 'operationId',
  ])
  if (
    record.schemaVersion !== NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA ||
    !isProfileId(record.profileId) || !isBrowser(record.browser) ||
    !isSampleOrdinal(record.sampleOrdinal)
  ) invalidAuthority()
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
    runId: canonicalId(record.runId),
    profileId: record.profileId,
    browser: record.browser,
    sampleOrdinal: record.sampleOrdinal,
    processInstanceId: canonicalId(record.processInstanceId),
    operationId: canonicalId(record.operationId),
  })
}

export function parseNetworkMatrixControlAuthority(value: unknown): NetworkMatrixControlAuthority {
  const record = exactRecord(value, [
    'schemaVersion', 'sampleAuthority', 'controlLeaseId',
  ])
  if (record.schemaVersion !== NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA) invalidAuthority()
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_CONTROL_AUTHORITY_SCHEMA,
    sampleAuthority: parseNetworkMatrixSampleAuthority(record.sampleAuthority),
    controlLeaseId: opaqueId(record.controlLeaseId),
  })
}

export function parseNetworkMatrixFixtureAuthorityBinding(
  value: unknown,
): NetworkMatrixFixtureAuthorityBinding {
  const record = exactRecord(value, [
    'attestationSha256', 'authorityInstanceId', 'remoteServiceInstanceId',
    'networkBindingSha256', 'remotePeerBindingSha256',
  ])
  return Object.freeze({
    attestationSha256: sha256(record.attestationSha256),
    authorityInstanceId: canonicalId(record.authorityInstanceId),
    remoteServiceInstanceId: canonicalId(record.remoteServiceInstanceId),
    networkBindingSha256: sha256(record.networkBindingSha256),
    remotePeerBindingSha256: sha256(record.remotePeerBindingSha256),
  })
}

export function parseNetworkMatrixAttemptRequestAuthority(
  value: unknown,
): NetworkMatrixAttemptRequestAuthority {
  const record = exactRecord(value, [
    'schemaVersion', 'controlAuthority', 'requestId', 'fixtureBinding',
  ])
  if (record.schemaVersion !== NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA) {
    invalidAuthority()
  }
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
    controlAuthority: parseNetworkMatrixControlAuthority(record.controlAuthority),
    requestId: opaqueId(record.requestId),
    fixtureBinding: parseNetworkMatrixFixtureAuthorityBinding(record.fixtureBinding),
  })
}

export function parseNetworkMatrixAttemptAuthority(value: unknown): NetworkMatrixAttemptAuthority {
  const record = exactRecord(value, [
    'schemaVersion', 'requestAuthority', 'attemptId', 'challenge',
  ])
  if (record.schemaVersion !== NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA) invalidAuthority()
  const requestAuthority = parseNetworkMatrixAttemptRequestAuthority(record.requestAuthority)
  const attemptId = opaqueId(record.attemptId)
  if (attemptId === requestAuthority.requestId) invalidAuthority()
  return Object.freeze({
    schemaVersion: NETWORK_MATRIX_ATTEMPT_AUTHORITY_SCHEMA,
    requestAuthority,
    attemptId,
    challenge: opaqueId(record.challenge),
  })
}

export function canonicalNetworkMatrixSampleAuthorityJson(
  value: NetworkMatrixSampleAuthority,
): string {
  return canonicalJson(parseNetworkMatrixSampleAuthority(value))
}

export function canonicalNetworkMatrixControlAuthorityJson(
  value: NetworkMatrixControlAuthority,
): string {
  return canonicalJson(parseNetworkMatrixControlAuthority(value))
}

export function canonicalNetworkMatrixAttemptRequestAuthorityJson(
  value: NetworkMatrixAttemptRequestAuthority,
): string {
  return canonicalJson(parseNetworkMatrixAttemptRequestAuthority(value))
}

export function canonicalNetworkMatrixAttemptAuthorityJson(
  value: NetworkMatrixAttemptAuthority,
): string {
  return canonicalJson(parseNetworkMatrixAttemptAuthority(value))
}

export function networkMatrixAttemptAuthoritySha256(
  value: NetworkMatrixAttemptAuthority,
): string {
  return createHash('sha256').update(canonicalNetworkMatrixAttemptAuthorityJson(value)).digest('hex')
}

export function sameNetworkMatrixSampleAuthority(
  left: NetworkMatrixSampleAuthority,
  right: NetworkMatrixSampleAuthority,
): boolean {
  return canonicalNetworkMatrixSampleAuthorityJson(left) ===
    canonicalNetworkMatrixSampleAuthorityJson(right)
}

export function sameNetworkMatrixControlAuthority(
  left: NetworkMatrixControlAuthority,
  right: NetworkMatrixControlAuthority,
): boolean {
  return canonicalNetworkMatrixControlAuthorityJson(left) ===
    canonicalNetworkMatrixControlAuthorityJson(right)
}

export function sameNetworkMatrixAttemptAuthority(
  left: NetworkMatrixAttemptAuthority,
  right: NetworkMatrixAttemptAuthority,
): boolean {
  return canonicalNetworkMatrixAttemptAuthorityJson(left) ===
    canonicalNetworkMatrixAttemptAuthorityJson(right)
}

function canonicalJson(value: unknown): string {
  return `${JSON.stringify(value)}\n`
}

function exactRecord(value: unknown, expectedKeys: readonly string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidAuthority()
  const prototype = Object.getPrototypeOf(value) as unknown
  if (prototype !== Object.prototype && prototype !== null) invalidAuthority()
  const keys = Reflect.ownKeys(value)
  if (
    keys.length !== expectedKeys.length ||
    keys.some((key, index) => typeof key !== 'string' || key !== expectedKeys[index])
  ) invalidAuthority()
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (expectedKeys.some((key) => {
    const descriptor = descriptors[key]
    return descriptor === undefined || !descriptor.enumerable || !('value' in descriptor)
  })) invalidAuthority()
  return value as Record<string, unknown>
}

function canonicalId(value: unknown): string {
  if (
    typeof value !== 'string' || Buffer.byteLength(value, 'utf8') > MAXIMUM_CANONICAL_ID_BYTES ||
    !CANONICAL_ID_PATTERN.test(value)
  ) invalidAuthority()
  return value
}

function opaqueId(value: unknown): string {
  if (typeof value !== 'string' || !OPAQUE_ID_PATTERN.test(value)) invalidAuthority()
  return value
}

function sha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidAuthority()
  return value
}

function isProfileId(value: unknown): value is NetworkMatrixProfileId {
  return (NETWORK_MATRIX_PROFILE_IDS as readonly unknown[]).includes(value)
}

function isBrowser(value: unknown): value is NetworkMatrixBrowser {
  return (NETWORK_MATRIX_BROWSERS as readonly unknown[]).includes(value)
}

function isSampleOrdinal(value: unknown): value is NetworkMatrixSampleOrdinal {
  return (NETWORK_MATRIX_SAMPLE_ORDINALS as readonly unknown[]).includes(value)
}

function invalidAuthority(): never {
  throw new Error('network matrix sample authority is invalid')
}
