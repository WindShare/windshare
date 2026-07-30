import { createHash } from 'node:crypto'
import { lstat, readFile } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'

import { createProductionContainmentBackend } from '../../browser-evidence/process/containment-factory.ts'
import type {
  NetworkMatrixExecutionRuntime,
  NetworkMatrixRuntimeBootstrap,
  NetworkMatrixRuntimeBootstrapContext,
} from '../cli/execute.ts'
import type { NetworkMatrixOwnedOperation } from '../owned-operation.ts'
import type { NetworkMatrixSampleExecutionContext } from '../runner.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  loadNetworkMatrixExternalFixtureConfig,
  type NetworkMatrixExternalFixtureConfig,
} from './concrete-runtime-config.ts'
import {
  ProcessExternalFixtureControlCredentialAuthority,
} from './control-credential-broker.ts'
import type {
  GitHubActionsOidcBootstrapLease,
  GitHubActionsOidcIdentityAuthority,
} from './parent-workload-identity.ts'
import {
  createExternalFixtureNetworkMatrixRuntimeBootstrap,
} from './external-fixture-runtime-bootstrap.ts'
import type { ManualOperatorTopologyIdentity } from './external-fixture-attestation.ts'
import type { ContainedBrowserTopologyFiles } from './contained-browser-input.ts'

export const PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA =
  'windshare.browser-network-matrix.production-runtime/v1' as const

const MAXIMUM_RUNTIME_CONFIG_BYTES = 1_048_576
const PROCESS_DEADLINE_MS = 180_000
const PROCESS_TERMINATION_GRACE_MS = 10_000
const ATTEMPT_LEASE_MS = 45_000
const RESULT_POLL_INTERVAL_MS = 250
const RESULT_DEADLINE_MS = 40_000
const CHALLENGE_DEADLINE_MS = 15_000
const CLEANUP_DEADLINE_MS = 10_000
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CHECKOUT_SHA_PATTERN = /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/u

type ProductionTopologyFiles = ContainedBrowserTopologyFiles

interface ProductionRuntimeConfig {
  readonly schemaVersion: typeof PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA
  readonly externalFixtureTrustConfigFile: string
  readonly credentialBrokerHelperFile: string
  readonly credentialBrokerWorkloadIdentity: {
    readonly kind: 'github-actions-oidc'
    readonly audience: string
    readonly issuer: string
    readonly repository: string
    readonly ref: string
    readonly workflowRef: string
    readonly requestOrigin: string
    readonly requestPath: string
    readonly requestQuery: string
  }
  readonly repositoryRoot: string
  readonly nodeExecutable: string
  readonly checkoutSha: string
  readonly containment:
    | { readonly kind: 'posix-process-group'; readonly windowsJobHelperFile: null }
    | { readonly kind: 'windows-job'; readonly windowsJobHelperFile: string }
  readonly topologyFiles: Readonly<Record<NetworkMatrixProfileId, ProductionTopologyFiles | null>>
  readonly manualOperatorIdentity: ManualOperatorTopologyIdentity | null
}

export interface ProductionNetworkMatrixCliRuntime {
  readonly platform: 'linux' | 'win32'
  readonly windowsJobHelperPath?: string
  bindWorkloadIdentityBootstrap(
    lease: GitHubActionsOidcBootstrapLease,
  ): NetworkMatrixRuntimeBootstrap
}

export async function loadProductionNetworkMatrixCliRuntime(
  configPath: string,
  platform: NodeJS.Platform = process.platform,
): Promise<ProductionNetworkMatrixCliRuntime> {
  const canonicalConfigPath = requireAbsolutePath(configPath)
  const concretePlatform = requirePlatform(platform)
  return Object.freeze({
    platform: concretePlatform,
    bindWorkloadIdentityBootstrap: (lease: GitHubActionsOidcBootstrapLease) =>
      productionRuntimeBootstrap(canonicalConfigPath, concretePlatform, lease),
  })
}

function productionRuntimeBootstrap(
  configPath: string,
  platform: 'linux' | 'win32',
  workloadIdentityBootstrap: GitHubActionsOidcBootstrapLease,
): NetworkMatrixRuntimeBootstrap {
  return Object.freeze({
    bootstrap: (context: NetworkMatrixRuntimeBootstrapContext) => new ProductionRuntimeBootstrapOperation(
      configPath,
      platform,
      context,
      workloadIdentityBootstrap,
    ).ownedOperation,
  })
}

class ProductionRuntimeBootstrapOperation {
  readonly #controller = new AbortController()
  readonly #result: Promise<NetworkMatrixExecutionRuntime>
  #runtime: NetworkMatrixExecutionRuntime | undefined
  #credentials: ProcessExternalFixtureControlCredentialAuthority | undefined
  #workloadIdentity: GitHubActionsOidcIdentityAuthority | undefined
  #nestedOperation: NetworkMatrixOwnedOperation<NetworkMatrixExecutionRuntime> | undefined
  readonly #workloadIdentityBootstrap: GitHubActionsOidcBootstrapLease
  #forceOperation: Promise<void> | undefined

  constructor(
    configPath: string,
    platform: 'linux' | 'win32',
    context: NetworkMatrixRuntimeBootstrapContext,
    workloadIdentityBootstrap: GitHubActionsOidcBootstrapLease,
  ) {
    this.#workloadIdentityBootstrap = workloadIdentityBootstrap
    this.#result = this.#bootstrap(configPath, platform, context)
  }

  get ownedOperation(): NetworkMatrixOwnedOperation<NetworkMatrixExecutionRuntime> {
    return Object.freeze({
      result: this.#result,
      forceTerminateAndWait: () => this.#forceTerminateAndWait(),
    })
  }

  async #bootstrap(
    configPath: string,
    platform: 'linux' | 'win32',
    context: NetworkMatrixRuntimeBootstrapContext,
  ): Promise<NetworkMatrixExecutionRuntime> {
    const config = await loadProductionRuntimeConfig(configPath)
    this.#requireActive()
    requireContainmentPlatform(config, platform)
    const externalFixtureConfig = await loadNetworkMatrixExternalFixtureConfig(
      config.externalFixtureTrustConfigFile,
    )
    this.#requireActive()
    await verifyOperationalFiles(config, externalFixtureConfig)
    this.#requireActive()
    const windowsJobHelperPath = config.containment.kind === 'windows-job'
      ? config.containment.windowsJobHelperFile
      : undefined
    const containment = createProductionContainmentBackend(
      windowsJobHelperPath === undefined ? {} : { windowsJobHelperPath },
    )
    this.#requireActive()
    const workloadIdentity = this.#workloadIdentityBootstrap.consume({
      ...config.credentialBrokerWorkloadIdentity,
    })
    this.#workloadIdentity = workloadIdentity
    this.#requireActive()
    const credentials = ProcessExternalFixtureControlCredentialAuthority.create({
      helperPath: config.credentialBrokerHelperFile,
      workingDirectory: config.repositoryRoot,
      platform,
      ...(windowsJobHelperPath === undefined ? {} : { windowsJobHelperPath }),
      config: externalFixtureConfig,
      workloadIdentity,
    })
    this.#credentials = credentials
    this.#requireActive()
    const bootstrap = createExternalFixtureNetworkMatrixRuntimeBootstrap({
      config: externalFixtureConfig,
      controlCredentials: credentials,
      containment,
      checkoutSha: config.checkoutSha,
      topologyFiles: (sampleContext) => topologyFilesFor(config, sampleContext),
      nodeExecutable: config.nodeExecutable,
      repositoryRoot: config.repositoryRoot,
      processDeadlineMs: PROCESS_DEADLINE_MS,
      terminationGraceMs: PROCESS_TERMINATION_GRACE_MS,
      attemptLeaseMs: ATTEMPT_LEASE_MS,
      resultPollIntervalMs: RESULT_POLL_INTERVAL_MS,
      resultDeadlineMs: RESULT_DEADLINE_MS,
      challengeDeadlineMs: CHALLENGE_DEADLINE_MS,
      cleanupDeadlineMs: CLEANUP_DEADLINE_MS,
      ...(config.manualOperatorIdentity === null
        ? {}
        : { manualOperatorIdentity: config.manualOperatorIdentity }),
    })
    const nestedOperation = bootstrap.bootstrap(context)
    this.#nestedOperation = nestedOperation
    const runtime = await nestedOperation.result
    this.#runtime = runtime
    this.#requireActive()
    return runtime
  }

  #requireActive(): void {
    if (this.#controller.signal.aborted) {
      throw new Error('production browser network matrix runtime bootstrap was terminated')
    }
  }

  #forceTerminateAndWait(): Promise<void> {
    this.#controller.abort()
    if (this.#forceOperation !== undefined) return this.#forceOperation
    const operation = this.#force().catch((cause: unknown) => {
      if (this.#forceOperation === operation) this.#forceOperation = undefined
      throw cause
    })
    this.#forceOperation = operation
    return operation
  }

  async #force(): Promise<void> {
    const failures: unknown[] = []
    try {
      await this.#nestedOperation?.forceTerminateAndWait('runtime-bootstrap')
    } catch (cause) {
      failures.push(cause)
    }
    await this.#result.catch(() => undefined)
    try {
      if (this.#runtime !== undefined) {
        requireClosedReceipt(await this.#runtime.forceTerminateAndWait())
      } else if (this.#credentials !== undefined) {
        requireClosedReceipt(await this.#credentials.forceTerminateAndWait())
      } else if (this.#workloadIdentity !== undefined) {
        requireClosedReceipt(await this.#workloadIdentity.forceTerminateAndWait())
      }
    } catch (cause) {
      failures.push(cause)
    }
    try {
      requireClosedReceipt(await this.#workloadIdentityBootstrap.forceTerminateAndWait())
    } catch (cause) {
      failures.push(cause)
    }
    if (failures.length !== 0) {
      throw new AggregateError(failures, 'production runtime bootstrap ownership did not settle')
    }
  }
}

function requireClosedReceipt(value: unknown): void {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidConfig()
  const receipt = value as Record<string, unknown>
  const keys = Object.keys(receipt)
  if (keys.length !== 1 || keys[0] !== 'terminal' || receipt.terminal !== 'closed') invalidConfig()
}

async function loadProductionRuntimeConfig(path: string): Promise<ProductionRuntimeConfig> {
  const canonicalPath = requireAbsolutePath(path)
  const bytes = await readFile(canonicalPath)
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_RUNTIME_CONFIG_BYTES) invalidConfig()
  let value: unknown
  try {
    value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
  } catch {
    invalidConfig()
  }
  return parseProductionRuntimeConfig(value)
}

function parseProductionRuntimeConfig(value: unknown): ProductionRuntimeConfig {
  const config = exactRecord(value, [
    'schemaVersion', 'externalFixtureTrustConfigFile', 'credentialBrokerHelperFile',
    'credentialBrokerWorkloadIdentity',
    'repositoryRoot', 'nodeExecutable', 'checkoutSha', 'containment', 'topologyFiles',
    'manualOperatorIdentity',
  ])
  if (
    config.schemaVersion !== PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA ||
    typeof config.checkoutSha !== 'string' || !CHECKOUT_SHA_PATTERN.test(config.checkoutSha)
  ) invalidConfig()
  return Object.freeze({
    schemaVersion: PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA,
    externalFixtureTrustConfigFile: requireAbsolutePath(config.externalFixtureTrustConfigFile),
    credentialBrokerHelperFile: requireAbsolutePath(config.credentialBrokerHelperFile),
    credentialBrokerWorkloadIdentity: parseWorkloadIdentity(
      config.credentialBrokerWorkloadIdentity,
    ),
    repositoryRoot: requireAbsolutePath(config.repositoryRoot),
    nodeExecutable: requireAbsolutePath(config.nodeExecutable),
    checkoutSha: config.checkoutSha,
    containment: parseContainment(config.containment),
    topologyFiles: parseTopologyFiles(config.topologyFiles),
    manualOperatorIdentity: parseManualOperatorIdentity(config.manualOperatorIdentity),
  })
}

function parseWorkloadIdentity(
  value: unknown,
): ProductionRuntimeConfig['credentialBrokerWorkloadIdentity'] {
  const identity = exactRecord(value, [
    'kind', 'audience', 'issuer', 'repository', 'ref', 'workflowRef',
    'requestOrigin', 'requestPath', 'requestQuery',
  ])
  if (
    identity.kind !== 'github-actions-oidc' || typeof identity.audience !== 'string' ||
    identity.audience.length < 8 || identity.audience.length > 512 ||
    !/^[A-Za-z0-9._:/-]+$/u.test(identity.audience) ||
    typeof identity.repository !== 'string' ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(identity.repository) ||
    typeof identity.ref !== 'string' ||
    !/^refs\/(?:heads|tags)\/[A-Za-z0-9_./-]+$/u.test(identity.ref) ||
    typeof identity.workflowRef !== 'string' ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/\.github\/workflows\/[A-Za-z0-9_./-]+@refs\/(?:heads|tags)\/[A-Za-z0-9_./-]+$/u.test(identity.workflowRef)
  ) invalidConfig()
  return Object.freeze({
    kind: 'github-actions-oidc',
    audience: identity.audience,
    issuer: requireCanonicalHttpsOrigin(identity.issuer),
    repository: identity.repository,
    ref: identity.ref,
    workflowRef: identity.workflowRef,
    requestOrigin: requireCanonicalHttpsOrigin(identity.requestOrigin),
    requestPath: requireAbsoluteRequestPath(identity.requestPath),
    requestQuery: requireCanonicalRequestQuery(identity.requestQuery),
  })
}

function parseContainment(value: unknown): ProductionRuntimeConfig['containment'] {
  const containment = exactRecord(value, ['kind', 'windowsJobHelperFile'])
  if (containment.kind === 'posix-process-group') {
    if (containment.windowsJobHelperFile !== null) invalidConfig()
    return Object.freeze({ kind: 'posix-process-group', windowsJobHelperFile: null })
  }
  if (containment.kind !== 'windows-job') invalidConfig()
  return Object.freeze({
    kind: 'windows-job',
    windowsJobHelperFile: requireAbsolutePath(containment.windowsJobHelperFile),
  })
}

function parseTopologyFiles(
  value: unknown,
): Readonly<Record<NetworkMatrixProfileId, ProductionTopologyFiles | null>> {
  const files = exactRecord(value, [
    'scheduled-public-stun', 'scheduled-restricted-udp', 'scheduled-coturn', 'manual-real-nat',
  ])
  return Object.freeze({
    'scheduled-public-stun': parseOptionalTopologyFiles(files['scheduled-public-stun']),
    'scheduled-restricted-udp': parseOptionalTopologyFiles(files['scheduled-restricted-udp']),
    'scheduled-coturn': parseOptionalTopologyFiles(files['scheduled-coturn']),
    'manual-real-nat': parseOptionalTopologyFiles(files['manual-real-nat']),
  })
}

function parseOptionalTopologyFiles(value: unknown): ProductionTopologyFiles | null {
  if (value === null) return null
  const files = exactRecord(value, [
    'topologyProfilePath', 'topologyProfileSha256',
    'topologyResolutionPath', 'topologyResolutionSha256',
  ])
  const topologyProfilePath = requireAbsolutePath(files.topologyProfilePath)
  const topologyResolutionPath = requireAbsolutePath(files.topologyResolutionPath)
  if (topologyProfilePath === topologyResolutionPath) invalidConfig()
  return Object.freeze({
    topologyProfilePath,
    topologyProfileSha256: requireSha256(files.topologyProfileSha256),
    topologyResolutionPath,
    topologyResolutionSha256: requireSha256(files.topologyResolutionSha256),
  })
}

function parseManualOperatorIdentity(value: unknown): ManualOperatorTopologyIdentity | null {
  if (value === null) return null
  const identity = exactRecord(value, ['senderHostId', 'senderNetworkBoundaryId'])
  return Object.freeze({
    senderHostId: requireCanonicalId(identity.senderHostId),
    senderNetworkBoundaryId: requireCanonicalId(identity.senderNetworkBoundaryId),
  })
}

async function verifyOperationalFiles(
  config: ProductionRuntimeConfig,
  fixtures: NetworkMatrixExternalFixtureConfig,
): Promise<void> {
  const paths = [
    config.credentialBrokerHelperFile,
    config.repositoryRoot,
    config.nodeExecutable,
    ...(config.containment.kind === 'windows-job'
      ? [config.containment.windowsJobHelperFile]
      : []),
  ]
  await Promise.all(paths.map(async (path) => {
    const status = await lstat(path)
    if (status.isSymbolicLink()) invalidConfig()
  }))
  for (const profileId of profileIds()) {
    const provisioned = fixtureForProfile(fixtures, profileId) !== null
    const topology = config.topologyFiles[profileId]
    if (provisioned !== (topology !== null)) invalidConfig()
    if (topology === null) continue
    await Promise.all([
      verifyFileDigest(topology.topologyProfilePath, topology.topologyProfileSha256),
      verifyFileDigest(topology.topologyResolutionPath, topology.topologyResolutionSha256),
    ])
  }
  const manualProvisioned = fixtures.manualRealNat !== null
  if (manualProvisioned !== (config.manualOperatorIdentity !== null)) invalidConfig()
}

async function verifyFileDigest(path: string, expected: string): Promise<void> {
  const status = await lstat(path)
  if (!status.isFile() || status.isSymbolicLink()) invalidConfig()
  const bytes = await readFile(path)
  if (createHash('sha256').update(bytes).digest('hex') !== expected) invalidConfig()
}

function topologyFilesFor(
  config: ProductionRuntimeConfig,
  context: NetworkMatrixSampleExecutionContext,
): ContainedBrowserTopologyFiles {
  const files = config.topologyFiles[context.identity.profileId]
  if (files === null) throw new Error('sample topology files were not provisioned')
  return files
}

function requireContainmentPlatform(
  config: ProductionRuntimeConfig,
  platform: 'linux' | 'win32',
): void {
  if (
    platform === 'linux' && config.containment.kind !== 'posix-process-group' ||
    platform === 'win32' && config.containment.kind !== 'windows-job'
  ) invalidConfig()
}

function fixtureForProfile(config: NetworkMatrixExternalFixtureConfig, profileId: NetworkMatrixProfileId) {
  return {
    'scheduled-public-stun': config.publicStun,
    'scheduled-restricted-udp': config.restrictedUdp,
    'scheduled-coturn': config.coturn,
    'manual-real-nat': config.manualRealNat,
  }[profileId]
}

function profileIds(): readonly NetworkMatrixProfileId[] {
  return Object.freeze([
    'scheduled-public-stun', 'scheduled-restricted-udp', 'scheduled-coturn', 'manual-real-nat',
  ])
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

function requirePlatform(value: NodeJS.Platform): 'linux' | 'win32' {
  if (value !== 'linux' && value !== 'win32') {
    throw new Error(`production browser network matrix is unsupported on ${value}`)
  }
  return value
}

function requireAbsolutePath(value: unknown): string {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value || value.includes('\0')) {
    invalidConfig()
  }
  return value
}

function requireSha256(value: unknown): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) invalidConfig()
  return value
}

function requireCanonicalHttpsOrigin(value: unknown): string {
  if (typeof value !== 'string') invalidConfig()
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    invalidConfig()
  }
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    endpoint.origin !== value
  ) invalidConfig()
  return value
}

function requireAbsoluteRequestPath(value: unknown): string {
  if (
    typeof value !== 'string' || value.length === 0 || value.length > 2_048 ||
    !value.startsWith('/') || value.includes('\0') || value.includes('?') || value.includes('#')
  ) invalidConfig()
  const parsed = new URL(value, 'https://authority.invalid')
  if (parsed.pathname !== value) invalidConfig()
  return value
}

function requireCanonicalRequestQuery(value: unknown): string {
  if (
    typeof value !== 'string' || value.length === 0 || value.length > 2_048 ||
    !value.startsWith('?') || value.includes('#') || value.includes('\0')
  ) invalidConfig()
  const endpoint = new URL(`https://authority.invalid/${value}`)
  if (endpoint.search !== value) invalidConfig()
  return value
}

function requireCanonicalId(value: unknown): string {
  if (
    typeof value !== 'string' || value.length > 96 ||
    !/^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u.test(value)
  ) invalidConfig()
  return value
}

function invalidConfig(): never {
  throw new Error('production browser network matrix runtime config is invalid')
}
