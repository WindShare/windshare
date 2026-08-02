import type { NetworkMatrixSampleAuthority } from '../../sample-authority.ts'
import type { LinuxTopologyTraceChannel } from '../trace/index.ts'
import type { NetworkMatrixExternalFixtureConfig } from '../concrete-runtime-config.ts'
import type {
  ExternalFixtureControlCredentialLeasePayload,
  SignedExternalFixtureControlCredentialLease,
} from '../control-credential.ts'
import type { ParentWorkloadIdentityAuthority } from '../parent-workload-identity.ts'
import type { TestProcessOwnerArtifact } from '../../../browser-evidence/process/test-process-owner-client.mjs'

export const EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL =
  'windshare.browser-network-matrix.credential-broker/v2' as const

export interface CredentialBrokerPipeExchange {
  exchange(input: {
    readonly operationId: string
    readonly stdin: Uint8Array
    readonly signal: AbortSignal
  }): Promise<Uint8Array>
}

export interface CredentialBrokerSecretStore {
  /** A distinct allocation prevents the transport owner from retaining credential authority. */
  adopt(source: Uint8Array): Uint8Array
}

export interface CredentialBrokerOptions {
  readonly helperPath: string
  readonly workingDirectory: string
  readonly platform: NodeJS.Platform
  readonly processOwner: TestProcessOwnerArtifact
  readonly config: NetworkMatrixExternalFixtureConfig
  readonly workloadIdentity: ParentWorkloadIdentityAuthority
}

export interface CredentialBrokerTestHarnessOptions extends CredentialBrokerOptions {
  readonly pipeExchange: CredentialBrokerPipeExchange
  readonly secretStore?: CredentialBrokerSecretStore
}

export interface InternalCredentialBrokerOptions extends CredentialBrokerOptions {
  readonly pipeExchange?: CredentialBrokerPipeExchange
  readonly secretStore?: CredentialBrokerSecretStore
}

export interface AuthenticatedCredentialLease
extends ExternalFixtureControlCredentialLeasePayload {
  readonly signedLease: SignedExternalFixtureControlCredentialLease
  readonly credential: Uint8Array
}

export interface CredentialBrokerScope {
  readonly sampleAuthority: NetworkMatrixSampleAuthority
  readonly probeNonce: string
}

export type CredentialBrokerDispatchOutcome = 'not-dispatched' | 'dispatched'

export interface CredentialBrokerExchangeExecution {
  readonly result: Promise<Buffer>
  readonly traces: LinuxTopologyTraceChannel
  readonly dispatchOutcome: Promise<CredentialBrokerDispatchOutcome>
}

export type CredentialBrokerExchange = (
  request: Readonly<Record<string, unknown>>,
  scope: CredentialBrokerScope,
  signal: AbortSignal,
) => CredentialBrokerExchangeExecution
