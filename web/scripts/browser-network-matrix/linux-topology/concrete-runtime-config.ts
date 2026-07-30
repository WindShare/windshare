import { readFile } from 'node:fs/promises'
import { isIP } from 'node:net'
import { isAbsolute, resolve } from 'node:path'

import type {
  NetworkMatrixExternalFixtureInput,
  NetworkMatrixRuntimeInputs,
} from '../runtime-authority.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'

export const EXTERNAL_FIXTURE_CONFIG_SCHEMA =
  'windshare.browser-network-matrix.external-fixture-trust/v2' as const

const MAXIMUM_CONFIG_BYTES = 1_048_576
const SHA256_PATTERN = /^[a-f0-9]{64}$/u

export interface RemotePionControlConfig {
  readonly controllerOrigin: string
  readonly tlsCertificateSha256: string
  readonly tlsCertificateAuthorityFile: string
  readonly attestationPublicKeyFile: string
}

export interface ExternalFixtureTrustConfig {
  readonly control: RemotePionControlConfig
}

export interface NetworkMatrixExternalFixtureConfig {
  readonly schemaVersion: typeof EXTERNAL_FIXTURE_CONFIG_SCHEMA
  readonly publicStun: ExternalFixtureTrustConfig | null
  readonly restrictedUdp: ExternalFixtureTrustConfig | null
  readonly coturn: ExternalFixtureTrustConfig | null
  readonly manualRealNat: ExternalFixtureTrustConfig | null
}

export async function loadNetworkMatrixExternalFixtureConfig(
  path: string,
): Promise<NetworkMatrixExternalFixtureConfig> {
  if (!canonicalAbsolutePath(path)) invalidConfig()
  let bytes: Buffer
  try {
    bytes = await readFile(path)
  } catch {
    invalidConfig()
  }
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_CONFIG_BYTES) invalidConfig()
  let value: unknown
  try {
    value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
  } catch {
    invalidConfig()
  }
  return parseNetworkMatrixExternalFixtureConfig(value)
}

export function parseNetworkMatrixExternalFixtureConfig(
  value: unknown,
): NetworkMatrixExternalFixtureConfig {
  const root = exactRecord(value, [
    'schemaVersion', 'publicStun', 'restrictedUdp', 'coturn', 'manualRealNat',
  ])
  if (root.schemaVersion !== EXTERNAL_FIXTURE_CONFIG_SCHEMA || root.coturn !== null) {
    // A trust policy is not a revocable TURN provider. Production composition
    // stays unavailable until a provider adapter can prove one-shot delivery
    // and joined revocation; accepting config here would fabricate readiness.
    invalidConfig()
  }
  return Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_CONFIG_SCHEMA,
    publicStun: parseOptionalFixtureTrust(root.publicStun),
    restrictedUdp: parseOptionalFixtureTrust(root.restrictedUdp),
    coturn: null,
    manualRealNat: parseOptionalFixtureTrust(root.manualRealNat),
  })
}

export function runtimeInputsFromExternalFixtureConfig(
  config: NetworkMatrixExternalFixtureConfig,
): NetworkMatrixRuntimeInputs {
  const externalFixtures: Partial<
    Record<NetworkMatrixProfileId, NetworkMatrixExternalFixtureInput>
  > = {}
  if (config.publicStun !== null) {
    externalFixtures['scheduled-public-stun'] = Object.freeze({
      profileId: 'scheduled-public-stun',
    })
  }
  if (config.restrictedUdp !== null) {
    externalFixtures['scheduled-restricted-udp'] = Object.freeze({
      profileId: 'scheduled-restricted-udp',
    })
  }
  if (config.manualRealNat !== null) {
    externalFixtures['manual-real-nat'] = Object.freeze({
      profileId: 'manual-real-nat',
    })
  }
  return Object.freeze({ externalFixtures: Object.freeze(externalFixtures) })
}

function parseOptionalFixtureTrust(value: unknown): ExternalFixtureTrustConfig | null {
  if (value === null) return null
  const fixture = exactRecord(value, ['control'])
  return Object.freeze({ control: parseRemoteControl(fixture.control) })
}

function parseRemoteControl(value: unknown): RemotePionControlConfig {
  const control = exactRecord(value, [
    'controllerOrigin', 'tlsCertificateSha256',
    'tlsCertificateAuthorityFile', 'attestationPublicKeyFile',
  ])
  const tlsCertificateAuthorityFile = requireAbsolutePath(control.tlsCertificateAuthorityFile)
  const attestationPublicKeyFile = requireAbsolutePath(control.attestationPublicKeyFile)
  if (tlsCertificateAuthorityFile === attestationPublicKeyFile) invalidConfig()
  const controllerOrigin = requireCanonicalHttpsOrigin(control.controllerOrigin)
  return Object.freeze({
    controllerOrigin,
    tlsCertificateSha256: requireSha256(control.tlsCertificateSha256),
    tlsCertificateAuthorityFile,
    attestationPublicKeyFile,
  })
}

function requireCanonicalHttpsOrigin(value: unknown): string {
  if (typeof value !== 'string') invalidConfig()
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    invalidConfig()
  }
  const hostname = endpoint.hostname.replace(/^\[|\]$/gu, '')
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    value !== `${endpoint.origin}/` || isIP(hostname) === 6
  ) invalidConfig()
  return value
}

function requireSha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidConfig()
  return value
}

function requireAbsolutePath(value: unknown): string {
  if (typeof value !== 'string' || !canonicalAbsolutePath(value) || value.includes('\0')) {
    invalidConfig()
  }
  return value
}

function canonicalAbsolutePath(value: string): boolean {
  return isAbsolute(value) && resolve(value) === value
}

function exactRecord(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidConfig()
  const record = value as Record<string, unknown>
  const actual = Object.keys(record)
  if (actual.length !== keys.length || actual.some((key, index) => key !== keys[index])) {
    invalidConfig()
  }
  return record
}

function invalidConfig(): never {
  throw new Error('network matrix external fixture trust config is invalid')
}
