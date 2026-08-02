import type { NetworkCandidatePath } from '../../candidate.ts'
import type {
  NetworkMatrixAttemptAuthority,
} from '../../sample-authority.ts'
import type {
  ExternalFixtureProbeResult,
  NetworkMatrixRtcConfiguration,
} from '../../runtime-authority.ts'
import type { NetworkMatrixBrowser } from '../../vocabulary.ts'
import type { SignedExternalFixtureAttestation } from '../external-fixture-attestation.ts'
import type { SignedExternalFixtureTerminalReceipt } from '../external-fixture-terminal-receipt.ts'
import type {
  RemotePionAttemptLease,
  RemotePionAttemptResult,
  RemotePionControlLeaseBinding,
} from '../remote-pion.ts'

export const CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA =
  'windshare.browser-network-matrix.contained-browser-secret/v4' as const
export const CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA =
  'windshare.browser-network-matrix.contained-browser-output/v3' as const

export interface ContainedBrowserSampleSecret {
  readonly schemaVersion: typeof CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA
  readonly expectedConnectivity: 'established' | 'blocked'
  readonly control: {
    readonly controllerOrigin: string
    readonly controlLease: RemotePionControlLeaseBinding
    readonly tlsCertificateAuthority: string
    readonly tlsCertificateSha256: string
    readonly attestationPublicKey: string
    readonly credential: Uint8Array
  }
  readonly attemptLeaseMs: number
  readonly resultPollIntervalMs: number
  readonly resultDeadlineMs: number
  readonly challengeDeadlineMs: number
  readonly cleanupDeadlineMs: number
}

export interface ContainedBrowserProtocolResult {
  readonly runId: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: SignedExternalFixtureAttestation
  readonly remoteServiceInstanceId: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly controllerPublicIp: string
  readonly attestationExpiresAt: string
  readonly remotePeerPublicIp: string
  readonly remotePeerUdpPortMin: number
  readonly remotePeerUdpPortMax: number
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly state: 'established' | 'failed'
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly challenge: string
  readonly failureCode: string | null
  readonly challengeEchoed: boolean
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt
}

export interface ContainedBrowserSampleOutput {
  readonly schemaVersion: typeof CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA
  readonly processInstanceId: string
  readonly browser: NetworkMatrixBrowser
  readonly protocolResult: ContainedBrowserProtocolResult
  readonly browserSelectedPair: NetworkCandidatePath
}

export interface ContainedBrowserSession {
  readonly engine: string
  createOffer(configuration: NetworkMatrixRtcConfiguration): Promise<string>
  acceptAnswer(answer: string): Promise<void>
  exchangeChallenge(frame: string, deadlineMs: number): Promise<string>
  getStats(): Promise<readonly unknown[]>
  close(): Promise<void>
}

export interface ContainedBrowserPionControl {
  probeFixture(signal: AbortSignal): Promise<ExternalFixtureProbeResult>
  createAttempt(
    requestId: string,
    leaseMillis: number,
    signal: AbortSignal,
  ): Promise<RemotePionAttemptLease>
  offer(attemptId: string, sdp: string, signal: AbortSignal): Promise<string>
  result(attemptId: string, signal: AbortSignal): Promise<RemotePionAttemptResult>
  deleteAttempt(attemptId: string, signal: AbortSignal): Promise<void>
}

export interface ContainedBrowserSampleDependencies {
  readonly launch: (browser: NetworkMatrixBrowser) => Promise<ContainedBrowserSession>
  readonly control: (secret: ContainedBrowserSampleSecret) => ContainedBrowserPionControl
  readonly requestId: () => string
  readonly delay: (milliseconds: number, signal: AbortSignal) => Promise<void>
  readonly now: () => number
}

export interface RunContainedBrowserSampleOptions {
  readonly browser: NetworkMatrixBrowser
  readonly secret: ContainedBrowserSampleSecret
  readonly signal: AbortSignal
  readonly dependencies?: ContainedBrowserSampleDependencies
}

export interface ContainedBrowserSampleSecretFrame {
  readonly bytes: Uint8Array
  readonly credentialOffset: number
  readonly credentialByteLength: number
}
